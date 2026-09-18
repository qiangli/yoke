// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package spacegraph

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	edgesFile = "edges.jsonl"
	// factFileMode: these are identity, and the default 0644 would put a
	// machine's reachable hosts and logins into every backup.
	fileMode = 0o600
	dirMode  = 0o700
)

// Store is the append-only, host-local edge record.
//
// Deliberately no Export, no Sync, and no marshaller that emits the whole set —
// the same discipline as craft's fact store, for the same reason. Every edge
// names something real about somebody's machine.
type Store struct {
	path string
}

// Open returns the store rooted at dir. Nothing is created until a write.
func Open(dir string) *Store { return &Store{path: filepath.Join(dir, edgesFile)} }

// Path reports the backing file.
func (s *Store) Path() string { return s.path }

// Observation is one thing a command taught about the environment.
type Observation struct {
	Src, Dst   string
	Rel        Relation
	Via        string
	OK         bool
	At         time.Time
	Coordinate string
	Place      string
	Source     string
}

// Record folds an observation into the store.
//
// It is an append, never an edit: the file is a log, and the live view is the
// replay. That buys the bi-temporal question — what did this host believe last
// Tuesday — which an edit-in-place store cannot answer at any price.
func (s *Store) Record(obs Observation) error {
	if obs.Src == "" || obs.Dst == "" || obs.Rel == "" {
		return nil
	}
	if obs.At.IsZero() {
		obs.At = time.Now().UTC()
	}

	live, err := s.Live(obs.At)
	if err != nil {
		return err
	}

	e := Edge{
		Schema: Schema, Src: obs.Src, Rel: obs.Rel, Dst: obs.Dst, Via: obs.Via,
		N: 1, First: obs.At, Last: obs.At, ValidFrom: obs.At,
		Coordinate: obs.Coordinate, Place: obs.Place, Source: obs.Source,
	}
	if obs.OK {
		e.OK = 1
	}

	if prev, ok := live[e.ID()]; ok {
		// Accumulate onto the SAME identity. This is the whole anti-fragmentation
		// property: the seventh ssh to a host strengthens one edge rather than
		// creating a seventh.
		e.N = prev.N + 1
		e.OK = prev.OK + e.OK
		e.First = prev.First
		e.ValidFrom = prev.ValidFrom
		if prev.Via != "" && e.Via == "" {
			e.Via = prev.Via
		}
	}
	return s.append(e)
}

// Supersede closes an edge as of t.
//
// Nothing is deleted. A closed edge still answers "this used to be true", which
// is what makes a stale claim explicable instead of merely absent.
func (s *Store) Supersede(id string, t time.Time) error {
	live, err := s.Live(t)
	if err != nil {
		return err
	}
	e, ok := live[id]
	if !ok {
		return nil
	}
	until := t
	e.ValidUntil = &until
	return s.append(e)
}

func (s *Store) append(e Edge) error {
	if err := os.MkdirAll(filepath.Dir(s.path), dirMode); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
	if err != nil {
		return err
	}
	defer f.Close()

	line, err := json.Marshal(&e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// Live replays the log and returns the edges believed at t, keyed by edge id.
//
// Last writer wins per identity, which is what makes a re-recorded observation
// idempotent rather than duplicative.
func (s *Store) Live(t time.Time) (map[string]Edge, error) {
	all, _, err := s.all()
	if err != nil {
		return nil, err
	}
	out := make(map[string]Edge, len(all))
	for _, e := range all {
		out[e.ID()] = e
	}
	for id, e := range out {
		if !e.Live(t) {
			delete(out, id)
		}
	}
	return out, nil
}

// Edges returns the live edges in a stable order, strongest first.
func (s *Store) Edges(t time.Time) ([]Edge, error) {
	live, err := s.Live(t)
	if err != nil {
		return nil, err
	}
	out := make([]Edge, 0, len(live))
	for _, e := range live {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		if out[i].Src != out[j].Src {
			return out[i].Src < out[j].Src
		}
		return out[i].Dst < out[j].Dst
	})
	return out, nil
}

// Nodes derives the entity set from the live edges.
//
// An entity with no edge is not returned. A name that was typed once and
// connected to nothing is not something the host learned about its environment.
func (s *Store) Nodes(t time.Time) ([]Node, error) {
	edges, err := s.Edges(t)
	if err != nil {
		return nil, err
	}
	idx := map[string]*Node{}
	touch := func(id string, at time.Time) *Node {
		n, ok := idx[id]
		if !ok {
			v := nodeOf(id)
			n, idx[id] = &v, &v
			n.First = at
		}
		if at.Before(n.First) {
			n.First = at
		}
		if at.After(n.Last) {
			n.Last = at
		}
		return n
	}
	for _, e := range edges {
		touch(e.Src, e.Last).Out++
		touch(e.Dst, e.Last).In++
		if e.Via != "" {
			// An account used to reach something has been USED, and a node
			// rendered with no degree at all reads as orphaned — which is the
			// opposite of what the record says about it.
			touch(e.Via, e.Last).In++
		}
	}
	out := make([]Node, 0, len(idx))
	for _, n := range idx {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := out[i].Out+out[i].In, out[j].Out+out[j].In; a != b {
			return a > b
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Malformed reports lines the store could not parse.
//
// Counted, never silently skipped: a corrupt line is evidence the store was
// damaged, and swallowing it makes a partial graph look complete.
func (s *Store) Malformed() int {
	_, bad, _ := s.all()
	return bad
}

func (s *Store) all() ([]Edge, int, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()

	var out []Edge
	bad := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 16<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Edge
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			bad++
			continue
		}
		out = append(out, e)
	}
	if sc.Err() != nil {
		bad++
	}
	return out, bad, nil
}
