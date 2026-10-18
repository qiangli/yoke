package dbstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MemGraph is the supplemental in-memory graph adapter: Bonsai, the graph
// engine, has no in-memory mode at its pinned version (its process-global
// state and Badger directory are file-backed), so memory-mode graphs use this
// minimal typed node/edge store instead. It is NOT DQL and claims no Bonsai
// query compatibility; the graph category maps its operations onto both.
//
// It is built on MemKV, so every Update is one atomic, serializable
// transaction over a copy-on-write snapshot, readers never block, and
// nothing touches disk. Every node and edge carries a revision drawn from
// one per-graph sequence, so contested writes are detected with Cond.
type MemGraph struct{ kv *MemKV }

// NewMemGraph returns an empty in-memory graph.
func NewMemGraph() *MemGraph {
	g := &MemGraph{kv: NewMemKV()}
	_ = g.kv.Update(context.Background(), func(tx KVTx) error {
		for _, b := range [][]byte{bNodes, bOut, bIn, bMeta} {
			if err := tx.CreateBucket(b); err != nil {
				return err
			}
		}
		return nil
	})
	return g
}

func (g *MemGraph) Close() error { return g.kv.Close() }

// Node is a typed vertex. Props is opaque to the store and kept byte-exact.
type Node struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Props []byte `json:"props,omitempty"`
	Rev   uint64 `json:"rev"`
}

// Edge is a typed, directed edge; (From, Type, To) is its identity.
type Edge struct {
	From  string `json:"from"`
	Type  string `json:"type"`
	To    string `json:"to"`
	Props []byte `json:"props,omitempty"`
	Rev   uint64 `json:"rev"`
}

// Cond is a write precondition: IfAbsent requires that the item does not
// exist; IfRev, when non-zero, requires its current revision. A failed
// precondition is ErrConflict.
type Cond struct {
	IfAbsent bool
	IfRev    uint64
}

// Direction selects outgoing or incoming edges.
type Direction int

const (
	Out Direction = iota
	In
)

// GraphTx is one graph transaction.
type GraphTx interface {
	GetNode(id string) (Node, bool, error)
	// Nodes visits all nodes in ID order in this transaction's snapshot.
	// Returning false stops the scan; callback errors propagate.
	Nodes(fn func(Node) (bool, error)) error
	PutNode(n Node, c Cond) (uint64, error)
	// DeleteNode removes the node and every edge touching it.
	DeleteNode(id string) error
	GetEdge(from, typ, to string) (Edge, bool, error)
	// PutEdge needs both endpoints to exist.
	PutEdge(e Edge, c Cond) (uint64, error)
	DeleteEdge(from, typ, to string) error
	// Edges visits the edges of id in one direction, optionally of one type,
	// in (type, other-end) order until fn returns false.
	Edges(id string, d Direction, typ string, fn func(Edge) (bool, error)) error
}

func (g *MemGraph) View(ctx context.Context, fn func(GraphTx) error) error {
	return g.kv.View(ctx, func(tx KVTx) error { return fn(graphTx{tx}) })
}

func (g *MemGraph) Update(ctx context.Context, fn func(GraphTx) error) error {
	return g.UpdateWithin(ctx, DefaultWait, fn)
}

// UpdateWithin is Update with an explicit writer-admission wait.
func (g *MemGraph) UpdateWithin(ctx context.Context, wait time.Duration, fn func(GraphTx) error) error {
	return g.kv.UpdateWithin(ctx, wait, func(tx KVTx) error { return fn(graphTx{tx}) })
}

var (
	bNodes = []byte("n")
	bOut   = []byte("o") // from \x00 type \x00 to -> edge
	bIn    = []byte("i") // to \x00 type \x00 from -> nil
	bMeta  = []byte("m")
	kSeq   = []byte("seq")
)

type graphTx struct{ tx KVTx }

func checkName(what, s string) error {
	if s == "" || strings.ContainsRune(s, 0) {
		return fmt.Errorf("%w: %s %q must be non-empty without NUL", ErrInvalidInput, what, s)
	}
	return nil
}

func edgeKey(a, typ, b string) []byte { return []byte(a + "\x00" + typ + "\x00" + b) }

func (g graphTx) next() (uint64, error) {
	v, _, err := g.tx.Get(bMeta, kSeq)
	if err != nil {
		return 0, err
	}
	var n uint64
	if len(v) == 8 {
		n = binary.BigEndian.Uint64(v)
	}
	n++
	return n, g.tx.Put(bMeta, kSeq, binary.BigEndian.AppendUint64(nil, n))
}

func check(what string, exists bool, rev uint64, c Cond) error {
	if c.IfAbsent && exists {
		return fmt.Errorf("%w: %s exists", ErrConflict, what)
	}
	if c.IfRev != 0 && (!exists || rev != c.IfRev) {
		return fmt.Errorf("%w: %s revision %d, expected %d", ErrConflict, what, rev, c.IfRev)
	}
	return nil
}

func (g graphTx) GetNode(id string) (Node, bool, error) {
	var n Node
	v, ok, err := g.tx.Get(bNodes, []byte(id))
	if err != nil || !ok {
		return n, false, err
	}
	return n, true, json.Unmarshal(v, &n)
}

func (g graphTx) Nodes(fn func(Node) (bool, error)) error {
	return g.tx.Scan(bNodes, nil, func(_, v []byte) (bool, error) {
		var n Node
		if err := json.Unmarshal(v, &n); err != nil {
			return false, err
		}
		return fn(n)
	})
}

func (g graphTx) PutNode(n Node, c Cond) (uint64, error) {
	if err := checkName("node id", n.ID); err != nil {
		return 0, err
	}
	cur, ok, err := g.GetNode(n.ID)
	if err != nil {
		return 0, err
	}
	if err := check("node "+n.ID, ok, cur.Rev, c); err != nil {
		return 0, err
	}
	if n.Rev, err = g.next(); err != nil {
		return 0, err
	}
	v, err := json.Marshal(n)
	if err != nil {
		return 0, err
	}
	return n.Rev, g.tx.Put(bNodes, []byte(n.ID), v)
}

func (g graphTx) DeleteNode(id string) error {
	if _, ok, err := g.GetNode(id); err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("%w: node %s", ErrNotFound, id)
		}
		return err
	}
	var edges []Edge
	for _, d := range []Direction{Out, In} {
		if err := g.Edges(id, d, "", func(e Edge) (bool, error) { edges = append(edges, e); return true, nil }); err != nil {
			return err
		}
	}
	for _, e := range edges {
		if err := g.DeleteEdge(e.From, e.Type, e.To); err != nil && Code(err) != "not_found" {
			return err // a self-loop is listed in both directions
		}
	}
	return g.tx.Delete(bNodes, []byte(id))
}

func (g graphTx) GetEdge(from, typ, to string) (Edge, bool, error) {
	var e Edge
	v, ok, err := g.tx.Get(bOut, edgeKey(from, typ, to))
	if err != nil || !ok {
		return e, false, err
	}
	return e, true, json.Unmarshal(v, &e)
}

func (g graphTx) PutEdge(e Edge, c Cond) (uint64, error) {
	for what, s := range map[string]string{"edge from": e.From, "edge type": e.Type, "edge to": e.To} {
		if err := checkName(what, s); err != nil {
			return 0, err
		}
	}
	for _, id := range []string{e.From, e.To} {
		if _, ok, err := g.GetNode(id); err != nil || !ok {
			if err == nil {
				err = fmt.Errorf("%w: endpoint node %s", ErrNotFound, id)
			}
			return 0, err
		}
	}
	cur, ok, err := g.GetEdge(e.From, e.Type, e.To)
	if err != nil {
		return 0, err
	}
	if err := check("edge "+e.From+"-"+e.Type+"->"+e.To, ok, cur.Rev, c); err != nil {
		return 0, err
	}
	if e.Rev, err = g.next(); err != nil {
		return 0, err
	}
	v, err := json.Marshal(e)
	if err != nil {
		return 0, err
	}
	if err := g.tx.Put(bOut, edgeKey(e.From, e.Type, e.To), v); err != nil {
		return 0, err
	}
	return e.Rev, g.tx.Put(bIn, edgeKey(e.To, e.Type, e.From), []byte{})
}

func (g graphTx) DeleteEdge(from, typ, to string) error {
	if _, ok, err := g.GetEdge(from, typ, to); err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("%w: edge %s-%s->%s", ErrNotFound, from, typ, to)
		}
		return err
	}
	if err := g.tx.Delete(bOut, edgeKey(from, typ, to)); err != nil {
		return err
	}
	return g.tx.Delete(bIn, edgeKey(to, typ, from))
}

func (g graphTx) Edges(id string, d Direction, typ string, fn func(Edge) (bool, error)) error {
	prefix := []byte(id + "\x00")
	if typ != "" {
		prefix = []byte(id + "\x00" + typ + "\x00")
	}
	b := bOut
	if d == In {
		b = bIn
	}
	return g.tx.Scan(b, prefix, func(k, v []byte) (bool, error) {
		if !bytes.HasPrefix(k, prefix) {
			return false, nil
		}
		if d == In {
			parts := strings.SplitN(string(k), "\x00", 3)
			e, ok, err := g.GetEdge(parts[2], parts[1], parts[0])
			if err != nil || !ok {
				return false, err
			}
			return fn(e)
		}
		var e Edge
		if err := json.Unmarshal(v, &e); err != nil {
			return false, err
		}
		return fn(e)
	})
}
