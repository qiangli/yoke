package nft

import (
	"fmt"
	"sort"
	"syscall"
)

// MemBackend is an in-memory Backend with the kernel's transaction
// semantics for the supported objects: queued writes apply to a copy at
// Commit and the first failing operation discards the whole batch. It is
// the hermetic stand-in for netlink in tests (no root, any GOOS).
type MemBackend struct {
	tables  map[tableKey]*memTable
	queue   []func(map[tableKey]*memTable) error
	handles uint64
}

type tableKey struct {
	fam  Family
	name string
}

type memTable struct {
	TableState
	chains   map[string]*memChain
	sets     map[string]*memSet
	counters map[string]*CounterSpec
	order    []string // chain creation order
}

type memChain struct {
	spec  ChainSpec
	rules []RuleSpec
}

type memSet struct {
	spec SetSpec
	els  []Element
}

// NewMemBackend returns an empty in-memory backend.
func NewMemBackend() *MemBackend { return &MemBackend{tables: map[tableKey]*memTable{}} }

func (m *MemBackend) handle() uint64 { m.handles++; return m.handles }

func (m *MemBackend) enqueue(f func(map[tableKey]*memTable) error) { m.queue = append(m.queue, f) }

// Commit applies the queued batch atomically.
func (m *MemBackend) Commit() error {
	q := m.queue
	m.queue = nil
	work := cloneTables(m.tables)
	saved := m.handles
	for _, f := range q {
		if err := f(work); err != nil {
			m.handles = saved
			return err
		}
	}
	m.tables = work
	return nil
}

func cloneTables(in map[tableKey]*memTable) map[tableKey]*memTable {
	out := map[tableKey]*memTable{}
	for k, t := range in {
		nt := &memTable{TableState: t.TableState, chains: map[string]*memChain{}, sets: map[string]*memSet{}, counters: map[string]*CounterSpec{}, order: append([]string{}, t.order...)}
		for n, c := range t.chains {
			nt.chains[n] = &memChain{spec: c.spec, rules: append([]RuleSpec{}, c.rules...)}
		}
		for n, s := range t.sets {
			nt.sets[n] = &memSet{spec: s.spec, els: append([]Element{}, s.els...)}
		}
		for n, c := range t.counters {
			cc := *c
			nt.counters[n] = &cc
		}
		out[k] = nt
	}
	return out
}

func getTable(ts map[tableKey]*memTable, fam Family, name string) (*memTable, error) {
	t, ok := ts[tableKey{fam, name}]
	if !ok {
		return nil, notFound("table %s %s", fam, name)
	}
	return t, nil
}

func (m *MemBackend) GetTable(fam Family, name string) (*TableState, error) {
	t, err := getTable(m.tables, fam, name)
	if err != nil {
		return nil, err
	}
	st := t.TableState
	for _, n := range t.order {
		c := t.chains[n]
		st.Chains = append(st.Chains, c.spec)
		st.Rules = append(st.Rules, c.rules...)
	}
	names := make([]string, 0, len(t.sets))
	for n := range t.sets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		st.Sets = append(st.Sets, t.sets[n].spec)
	}
	cs, _ := m.GetCounters(fam, name)
	st.Counters = cs
	return &st, nil
}

func (m *MemBackend) GetSet(fam Family, table, name string) (*SetSpec, error) {
	t, err := getTable(m.tables, fam, table)
	if err != nil {
		return nil, err
	}
	s, ok := t.sets[name]
	if !ok {
		return nil, notFound("set %s", name)
	}
	spec := s.spec
	return &spec, nil
}

func (m *MemBackend) GetElements(s *SetSpec) ([]Element, error) {
	t, err := getTable(m.tables, s.Family, s.Table)
	if err != nil {
		return nil, err
	}
	ms, ok := t.sets[s.Name]
	if !ok {
		return nil, notFound("set %s", s.Name)
	}
	return append([]Element{}, ms.els...), nil
}

func (m *MemBackend) GetCounters(fam Family, table string) ([]CounterSpec, error) {
	t, err := getTable(m.tables, fam, table)
	if err != nil {
		return nil, err
	}
	var out []CounterSpec
	for _, c := range t.counters {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *MemBackend) AddTable(fam Family, name, comment string, create bool) {
	m.enqueue(func(ts map[tableKey]*memTable) error {
		k := tableKey{fam, name}
		if t, ok := ts[k]; ok {
			if create {
				return fmt.Errorf("table %s: %w", name, syscall.EEXIST)
			}
			if comment != "" {
				t.Comment = comment
			}
			return nil
		}
		ts[k] = &memTable{TableState: TableState{Family: fam, Name: name, Handle: m.handle(), Comment: comment},
			chains: map[string]*memChain{}, sets: map[string]*memSet{}, counters: map[string]*CounterSpec{}}
		return nil
	})
}

func (m *MemBackend) DelTable(fam Family, name string) {
	m.enqueue(func(ts map[tableKey]*memTable) error {
		if _, err := getTable(ts, fam, name); err != nil {
			return err
		}
		delete(ts, tableKey{fam, name})
		return nil
	})
}

func (m *MemBackend) AddChain(ch *ChainSpec, create bool) {
	spec := *ch
	m.enqueue(func(ts map[tableKey]*memTable) error {
		t, err := getTable(ts, spec.Family, spec.Table)
		if err != nil {
			return err
		}
		if c, ok := t.chains[spec.Name]; ok {
			if create {
				return fmt.Errorf("chain %s: %w", spec.Name, syscall.EEXIST)
			}
			if spec.Type != "" && c.spec.Type == "" {
				return fmt.Errorf("chain %s exists as a regular chain: %w", spec.Name, syscall.EEXIST)
			}
			return nil
		}
		spec.Handle = m.handle()
		t.chains[spec.Name] = &memChain{spec: spec}
		t.order = append(t.order, spec.Name)
		return nil
	})
}

func (m *MemBackend) chain(ts map[tableKey]*memTable, fam Family, table, name string) (*memTable, *memChain, error) {
	t, err := getTable(ts, fam, table)
	if err != nil {
		return nil, nil, err
	}
	c, ok := t.chains[name]
	if !ok {
		return nil, nil, notFound("chain %s", name)
	}
	return t, c, nil
}

func (m *MemBackend) FlushChain(fam Family, table, name string) {
	m.enqueue(func(ts map[tableKey]*memTable) error {
		_, c, err := m.chain(ts, fam, table, name)
		if err != nil {
			return err
		}
		c.rules = nil
		return nil
	})
}

func (m *MemBackend) DelChain(fam Family, table, name string) {
	m.enqueue(func(ts map[tableKey]*memTable) error {
		t, c, err := m.chain(ts, fam, table, name)
		if err != nil {
			return err
		}
		if len(c.rules) > 0 {
			return fmt.Errorf("chain %s is not empty: %w", name, syscall.EBUSY)
		}
		delete(t.chains, name)
		for i, n := range t.order {
			if n == name {
				t.order = append(t.order[:i], t.order[i+1:]...)
				break
			}
		}
		return nil
	})
}

func (m *MemBackend) AddSet(s *SetSpec, create bool) {
	spec := *s
	m.enqueue(func(ts map[tableKey]*memTable) error {
		t, err := getTable(ts, spec.Family, spec.Table)
		if err != nil {
			return err
		}
		if _, ok := t.sets[spec.Name]; ok {
			if create {
				return fmt.Errorf("set %s: %w", spec.Name, syscall.EEXIST)
			}
			return nil
		}
		spec.Handle = m.handle()
		t.sets[spec.Name] = &memSet{spec: spec}
		return nil
	})
}

func (m *MemBackend) set(ts map[tableKey]*memTable, s *SetSpec) (*memTable, *memSet, error) {
	t, err := getTable(ts, s.Family, s.Table)
	if err != nil {
		return nil, nil, err
	}
	ms, ok := t.sets[s.Name]
	if !ok {
		return nil, nil, notFound("set %s", s.Name)
	}
	return t, ms, nil
}

func (m *MemBackend) FlushSet(s *SetSpec) {
	spec := *s
	m.enqueue(func(ts map[tableKey]*memTable) error {
		_, ms, err := m.set(ts, &spec)
		if err != nil {
			return err
		}
		ms.els = nil
		return nil
	})
}

func (m *MemBackend) DelSet(s *SetSpec) {
	spec := *s
	m.enqueue(func(ts map[tableKey]*memTable) error {
		t, _, err := m.set(ts, &spec)
		if err != nil {
			return err
		}
		delete(t.sets, spec.Name)
		return nil
	})
}

func (m *MemBackend) AddElements(s *SetSpec, els []Element) {
	spec := *s
	m.enqueue(func(ts map[tableKey]*memTable) error {
		_, ms, err := m.set(ts, &spec)
		if err != nil {
			return err
		}
	next:
		for _, e := range els {
			for i, have := range ms.els {
				if string(have.Key) == string(e.Key) {
					ms.els[i] = e
					continue next
				}
			}
			ms.els = append(ms.els, e)
		}
		return nil
	})
}

func (m *MemBackend) DelElements(s *SetSpec, els []Element) {
	spec := *s
	m.enqueue(func(ts map[tableKey]*memTable) error {
		_, ms, err := m.set(ts, &spec)
		if err != nil {
			return err
		}
	next:
		for _, e := range els {
			for i, have := range ms.els {
				if string(have.Key) == string(e.Key) {
					ms.els = append(ms.els[:i], ms.els[i+1:]...)
					continue next
				}
			}
			return notFound("element in set %s", spec.Name)
		}
		return nil
	})
}

func (m *MemBackend) AddRule(r *RuleSpec) {
	spec := *r
	m.enqueue(func(ts map[tableKey]*memTable) error {
		t, c, err := m.chain(ts, spec.Family, spec.Table, spec.Chain)
		if err != nil {
			return err
		}
		for _, e := range spec.Exprs {
			var name string
			switch x := e.(type) {
			case Lookup:
				name = x.Set
			case Dynset:
				name = x.Set
			case VerdictExpr:
				if x.Chain != "" {
					if _, ok := t.chains[x.Chain]; !ok {
						return notFound("chain %s", x.Chain)
					}
				}
			case ObjRef:
				if _, ok := t.counters[x.Name]; !ok {
					return notFound("counter %s", x.Name)
				}
			}
			if name != "" {
				if _, ok := t.sets[name]; !ok {
					return notFound("set %s", name)
				}
			}
		}
		spec.Handle = m.handle()
		c.rules = append(c.rules, spec)
		return nil
	})
}

func (m *MemBackend) DelRule(fam Family, table, chain string, handle uint64) {
	m.enqueue(func(ts map[tableKey]*memTable) error {
		_, c, err := m.chain(ts, fam, table, chain)
		if err != nil {
			return err
		}
		for i, r := range c.rules {
			if r.Handle == handle {
				c.rules = append(c.rules[:i], c.rules[i+1:]...)
				return nil
			}
		}
		return notFound("rule handle %d", handle)
	})
}

func (m *MemBackend) AddCounter(cs *CounterSpec, create bool) {
	spec := *cs
	m.enqueue(func(ts map[tableKey]*memTable) error {
		t, err := getTable(ts, spec.Family, spec.Table)
		if err != nil {
			return err
		}
		if _, ok := t.counters[spec.Name]; ok {
			if create {
				return fmt.Errorf("counter %s: %w", spec.Name, syscall.EEXIST)
			}
			return nil
		}
		spec.Handle = m.handle()
		t.counters[spec.Name] = &spec
		return nil
	})
}

func (m *MemBackend) DelCounter(fam Family, table, name string) {
	m.enqueue(func(ts map[tableKey]*memTable) error {
		t, err := getTable(ts, fam, table)
		if err != nil {
			return err
		}
		if _, ok := t.counters[name]; !ok {
			return notFound("counter %s", name)
		}
		delete(t.counters, name)
		return nil
	})
}
