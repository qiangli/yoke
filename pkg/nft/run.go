package nft

import (
	"errors"
	"fmt"
	"syscall"
)

// Backend is the kernel (or a fake). Writes are queued and applied
// atomically by Commit — `nft -f` is one transaction. Reads see committed
// state only. Not-found / already-exists conditions are reported as errors
// wrapping syscall.ENOENT / syscall.EEXIST, as the kernel does.
type Backend interface {
	GetTable(fam Family, name string) (*TableState, error)
	GetSet(fam Family, table, name string) (*SetSpec, error)
	GetElements(s *SetSpec) ([]Element, error)
	GetCounters(fam Family, table string) ([]CounterSpec, error)

	AddTable(fam Family, name, comment string, create bool)
	DelTable(fam Family, name string)
	AddChain(ch *ChainSpec, create bool)
	FlushChain(fam Family, table, name string)
	DelChain(fam Family, table, name string)
	AddSet(s *SetSpec, create bool)
	FlushSet(s *SetSpec)
	DelSet(s *SetSpec)
	AddElements(s *SetSpec, els []Element)
	DelElements(s *SetSpec, els []Element)
	AddRule(r *RuleSpec)
	InsertRule(r *RuleSpec)
	DelRule(fam Family, table, chain string, handle uint64)
	AddCounter(c *CounterSpec, create bool)
	DelCounter(fam Family, table, name string)
	Commit() error
}

// CmdError locates a failure in the script.
type CmdError struct {
	Line int
	Err  error
}

func (e *CmdError) Error() string { return fmt.Sprintf("line %d: %v", e.Line, e.Err) }
func (e *CmdError) Unwrap() error { return e.Err }

func notFound(format string, a ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, a...), syscall.ENOENT)
}

type setKey struct {
	fam         Family
	table, name string
}

// Run parses and applies script as one transaction. With check it only
// validates (parse, compile, element encoding) and writes nothing.
func Run(b Backend, script string, check bool) error {
	cmds, err := Parse(script)
	if err != nil {
		return err
	}
	return runCommands(b, cmds, check)
}

func runCommands(b Backend, cmds []*Command, check bool) error {
	pending := map[setKey]*SetSpec{}
	deleted := map[setKey]bool{}
	lookup := func(fam Family, table, name string) (*SetSpec, error) {
		k := setKey{fam, table, name}
		if s, ok := pending[k]; ok {
			return s, nil
		}
		if deleted[k] {
			return nil, notFound("set %s", name)
		}
		return b.GetSet(fam, table, name)
	}
	w := b
	if check {
		w = nil
	}
	for _, c := range cmds {
		if err := apply(w, c, pending, deleted, lookup); err != nil {
			return &CmdError{Line: c.Line, Err: err}
		}
	}
	if check {
		return nil
	}
	return b.Commit()
}

func apply(w Backend, c *Command, pending map[setKey]*SetSpec, deleted map[setKey]bool,
	lookup func(Family, string, string) (*SetSpec, error)) error {
	create := c.Verb == "create"
	k := setKey{c.Family, c.Table, c.Name}
	switch c.Object {
	case "table":
		if c.Verb == "delete" {
			for sk := range pending {
				if sk.fam == c.Family && sk.table == c.Table {
					delete(pending, sk)
				}
			}
			if w != nil {
				w.DelTable(c.Family, c.Table)
			}
			return nil
		}
		if w != nil {
			w.AddTable(c.Family, c.Table, c.TableComment, create)
		}
	case "chain":
		if w == nil {
			return nil
		}
		switch c.Verb {
		case "add", "create":
			if c.Chain == nil {
				c.Chain = &ChainSpec{Family: c.Family, Table: c.Table, Name: c.Name}
			}
			w.AddChain(c.Chain, create)
		case "flush":
			w.FlushChain(c.Family, c.Table, c.Name)
		case "delete":
			w.DelChain(c.Family, c.Table, c.Name)
		}
	case "set", "map":
		switch c.Verb {
		case "add", "create":
			if c.Set.Interval && len(c.Set.Keys) > 1 {
				return fmt.Errorf("unsupported: concatenated interval sets")
			}
			pending[k] = c.Set
			delete(deleted, k)
			if w != nil {
				w.AddSet(c.Set, create)
			}
		case "flush", "delete":
			s, err := lookup(c.Family, c.Table, c.Name)
			if err != nil {
				return err
			}
			if (c.Object == "map") != s.IsMap() {
				return fmt.Errorf("%s %s is not a %s", c.Object, c.Name, c.Object)
			}
			if c.Verb == "delete" {
				delete(pending, k)
				deleted[k] = true
				if w != nil {
					w.DelSet(s)
				}
			} else if w != nil {
				w.FlushSet(s)
			}
		}
	case "counter":
		if w == nil {
			return nil
		}
		if c.Verb == "delete" {
			w.DelCounter(c.Family, c.Table, c.Name)
		} else {
			w.AddCounter(c.Counter, create)
		}
	case "element":
		s, err := lookup(c.Family, c.Table, c.Name)
		if err != nil {
			return err
		}
		els, err := encodeElements(s, c.Elements, c.Verb != "delete")
		if err != nil {
			return err
		}
		if w != nil {
			if c.Verb == "delete" {
				w.DelElements(s, els)
			} else {
				w.AddElements(s, els)
			}
		}
	case "rule":
		if c.Verb == "delete" {
			if w != nil {
				w.DelRule(c.Family, c.Table, c.Name, c.Handle)
			}
			return nil
		}
		r, err := CompileRule(c.Family, c.Table, c.Name, c.RuleToks, func(name string) (*SetSpec, error) {
			return lookup(c.Family, c.Table, name)
		})
		if err != nil {
			return err
		}
		if w != nil {
			r.JSONExpr = c.JSONExpr
			if c.Insert {
				w.InsertRule(r)
			} else {
				w.AddRule(r)
			}
		}
	}
	return nil
}

func encodeElements(s *SetSpec, raw []rawElement, withValue bool) ([]Element, error) {
	if s.Interval {
		return nil, fmt.Errorf("unsupported: elements of interval set %s", s.Name)
	}
	var out []Element
	for _, r := range raw {
		key, err := encodeConcat(s.Keys, r.Key)
		if err != nil {
			return nil, err
		}
		e := Element{Key: key, Comment: r.Comment}
		if withValue && s.IsMap() {
			if r.Value == nil {
				return nil, fmt.Errorf("map %s element needs a value", s.Name)
			}
			if s.IsVerdictMap() {
				if len(r.Value) != 1 {
					return nil, fmt.Errorf("bad verdict %q", joinFields(r.Value))
				}
				if e.Verdict, err = parseVerdict(r.Value[0]); err != nil {
					return nil, err
				}
			} else if e.Data, err = encodeConcat(s.Data, r.Value); err != nil {
				return nil, err
			}
		} else if r.Value != nil {
			return nil, fmt.Errorf("set %s element cannot have a value", s.Name)
		}
		out = append(out, e)
	}
	return out, nil
}

// ErrorText renders err the way nft's stderr does, so callers that grep
// for "No such file or directory" / "File exists" (knftables) classify it.
func ErrorText(err error) string {
	switch {
	case errors.Is(err, syscall.ENOENT):
		return "Error: Could not process rule: No such file or directory\n" + err.Error()
	case errors.Is(err, syscall.EEXIST):
		return "Error: Could not process rule: File exists\n" + err.Error()
	case errors.Is(err, syscall.EBUSY):
		return "Error: Could not process rule: Device or resource busy\n" + err.Error()
	case errors.Is(err, syscall.EPERM):
		return "Error: Could not process rule: Operation not permitted\n" + err.Error()
	}
	return "Error: " + err.Error()
}
