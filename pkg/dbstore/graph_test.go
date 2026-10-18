package dbstore

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestMemGraphLifecycleAndPreconditions(t *testing.T) {
	ctx := context.Background()
	g := NewMemGraph()
	defer g.Close()
	props := []byte{0, 0xff, '"', 0x80}
	var revA uint64
	if err := g.Update(ctx, func(tx GraphTx) error {
		var err error
		if revA, err = tx.PutNode(Node{ID: "a", Type: "person", Props: props}, Cond{IfAbsent: true}); err != nil {
			return err
		}
		for _, id := range []string{"b", "c"} {
			if _, err := tx.PutNode(Node{ID: id, Type: "person"}, Cond{}); err != nil {
				return err
			}
		}
		for _, e := range []Edge{{From: "a", Type: "knows", To: "b"}, {From: "a", Type: "knows", To: "c"}, {From: "c", Type: "likes", To: "a"}, {From: "a", Type: "self", To: "a"}} {
			if _, err := tx.PutEdge(e, Cond{IfAbsent: true}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = g.View(ctx, func(tx GraphTx) error {
		n, ok, err := tx.GetNode("a")
		if err != nil || !ok || string(n.Props) != string(props) || n.Rev != revA || n.Type != "person" {
			t.Fatalf("node %+v %v %v", n, ok, err)
		}
		var out []string
		tx.Edges("a", Out, "knows", func(e Edge) (bool, error) { out = append(out, e.To); return true, nil })
		if len(out) != 2 || out[0] != "b" || out[1] != "c" {
			t.Fatalf("out edges %v", out)
		}
		var in []string
		tx.Edges("a", In, "", func(e Edge) (bool, error) { in = append(in, e.From+":"+e.Type); return true, nil })
		if len(in) != 2 || in[0] != "c:likes" || in[1] != "a:self" {
			t.Fatalf("in edges %v", in)
		}
		if _, err := tx.PutNode(Node{ID: "z"}, Cond{}); !errors.Is(err, ErrReadOnly) {
			t.Fatalf("write in view: %v", err)
		}
		return nil
	})
	err := g.Update(ctx, func(tx GraphTx) error {
		_, err := tx.PutNode(Node{ID: "a", Type: "x"}, Cond{IfAbsent: true})
		return err
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("same-id insert: %v", err)
	}
	err = g.Update(ctx, func(tx GraphTx) error {
		_, err := tx.PutNode(Node{ID: "a", Type: "x"}, Cond{IfRev: revA + 100})
		return err
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	err = g.Update(ctx, func(tx GraphTx) error {
		_, err := tx.PutEdge(Edge{From: "a", Type: "knows", To: "ghost"}, Cond{})
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("dangling edge: %v", err)
	}
	if err := g.Update(ctx, func(tx GraphTx) error { return tx.DeleteNode("a") }); err != nil {
		t.Fatal(err)
	}
	_ = g.View(ctx, func(tx GraphTx) error {
		n := 0
		for _, id := range []string{"b", "c"} {
			for _, d := range []Direction{Out, In} {
				tx.Edges(id, d, "", func(Edge) (bool, error) { n++; return true, nil })
			}
		}
		if n != 0 {
			t.Fatalf("%d edges survived their node", n)
		}
		return nil
	})
}

// Two writers read the same revision and both try to update it: exactly one
// wins, the other gets a conflict instead of silently losing its update.
func TestMemGraphContestedUpdate(t *testing.T) {
	ctx := context.Background()
	g := NewMemGraph()
	var rev uint64
	g.Update(ctx, func(tx GraphTx) (err error) { rev, err = tx.PutNode(Node{ID: "doc"}, Cond{}); return })
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = g.Update(ctx, func(tx GraphTx) error {
				_, err := tx.PutNode(Node{ID: "doc", Props: []byte{byte(i)}}, Cond{IfRev: rev})
				return err
			})
		}()
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrConflict):
			conflict++
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("contested update: %v", results)
	}
}
