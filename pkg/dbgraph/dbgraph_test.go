package dbgraph

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/bonsaidb"
)

func call(t *testing.T, h dbstore.Handle, op string, in Input) (dblifecycle.Outcome, error) {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	cat, access, ok, err := dblifecycle.LookupCategoryOp(op, raw)
	if err != nil || !ok {
		t.Fatalf("lookup %s: %v %v", op, ok, err)
	}
	return cat.Run(context.Background(), h, raw, access)
}

func TestMemoryGraphTypedOperationsAndConflicts(t *testing.T) {
	ctx := context.Background()
	space, err := dbstore.NewMemSpace(dbstore.LifetimeMCPSession)
	if err != nil {
		t.Fatal(err)
	}
	defer space.Close()
	target := dbstore.Target{Engine: "bonsai", Mode: dbstore.Memory, Session: space.ID(), Name: "graph"}
	h, err := bonsaidb.Adapter{}.Create(ctx, target, dbstore.Options{Mem: space})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, id := range []string{"a", "b"} {
		if _, err := call(t, h, OpNodePut, Input{Node: &dbstore.Node{ID: id, Type: "person", Props: []byte{0, 255}}, IfAbsent: true}); err != nil {
			t.Fatal(err)
		}
	}
	out, err := call(t, h, OpNodeGet, Input{ID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	n := out.Result.(dbstore.Node)
	if n.Type != "person" || len(n.Props) != 2 || n.Props[1] != 255 {
		t.Fatalf("node: %+v", n)
	}
	if _, err := call(t, h, OpNodePut, Input{Node: &dbstore.Node{ID: "a", Type: "other"}, IfRevision: n.Rev + 1}); !errors.Is(err, dbstore.ErrConflict) {
		t.Fatalf("revision: %v", err)
	}
	if _, err := call(t, h, OpEdgePut, Input{Edge: &dbstore.Edge{From: "a", To: "b", Type: "knows", Props: []byte{1, 2}}}); err != nil {
		t.Fatal(err)
	}
	out, err = call(t, h, OpEdges, Input{ID: "b", Direction: "in"})
	if err != nil {
		t.Fatal(err)
	}
	es := out.Result.(map[string]any)["edges"].([]dbstore.Edge)
	if len(es) != 1 || es[0].From != "a" {
		t.Fatalf("edges: %+v", es)
	}
	if _, err := call(t, h, OpQuery, Input{Query: "{ q(func: uid(1)) { uid } }"}); !errors.Is(err, dbstore.ErrUnsupported) {
		t.Fatalf("memory DQL: %v", err)
	}
}

func TestPersistentDQLAndProcessExclusion(t *testing.T) {
	ctx := context.Background()
	target := dbstore.Target{Engine: "bonsai", Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "graph")}
	h, err := bonsaidb.Adapter{}.Create(ctx, target, dbstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := (bonsaidb.Adapter{}).Open(ctx, target, dbstore.Options{}); !errors.Is(err, dbstore.ErrBusy) {
		t.Fatalf("second native handle: %v", err)
	}
	if _, err := call(t, h, OpSchemaApply, Input{Schema: "name: string @index(exact) ."}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, h, OpMutate, Input{SetNQuads: `_:a <name> "Alice" .`}); err != nil {
		t.Fatal(err)
	}
	out, err := call(t, h, OpQuery, Input{Query: `query q($name: string) { q(func: eq(name, $name)) { name } }`, Variables: map[string]string{"$name": "Alice"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out.Result.(json.RawMessage)), "Alice") {
		t.Fatalf("bound DQL result: %s", out.Result)
	}
}

func TestPersistentTypedGraph(t *testing.T) {
	ctx := context.Background()
	target := dbstore.Target{Engine: "bonsai", Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "typed")}
	h, err := bonsaidb.Adapter{}.Create(ctx, target, dbstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, id := range []string{"a", "isolated", "b"} {
		if _, err := call(t, h, OpNodePut, Input{Node: &dbstore.Node{ID: id, Type: "person", Props: []byte{0, 255}}, IfAbsent: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := call(t, h, OpEdgePut, Input{Edge: &dbstore.Edge{From: "a", Type: "knows", To: "b", Props: []byte{1, 2}}}); err != nil {
		t.Fatal(err)
	}
	out, err := call(t, h, OpNodeGet, Input{ID: "isolated"})
	if err != nil {
		t.Fatal(err)
	}
	n := out.Result.(dbstore.Node)
	if n.ID != "isolated" || len(n.Props) != 2 || n.Props[1] != 255 {
		t.Fatalf("node: %+v", n)
	}
	if _, err := call(t, h, OpMutate, Input{SetNQuads: `_:x <bashy_graph_key> "bad" .`}); !errors.Is(err, dbstore.ErrUnsupported) {
		t.Fatalf("raw mutation bypass: %v", err)
	}
	if _, err := call(t, h, OpNodeRm, Input{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, h, OpEdgeGet, Input{From: "a", Type: "knows", To: "b"}); !errors.Is(err, dbstore.ErrNotFound) {
		t.Fatalf("cascade: %v", err)
	}
}

func TestGraphPortableAcrossModes(t *testing.T) {
	ctx := context.Background()
	space, err := dbstore.NewMemSpace(dbstore.LifetimeMCPSession)
	if err != nil {
		t.Fatal(err)
	}
	defer space.Close()
	memory := func(name string) dbstore.Handle {
		t.Helper()
		h, e := (bonsaidb.Adapter{}).Create(ctx, dbstore.Target{Engine: "bonsai", Mode: dbstore.Memory, Session: space.ID(), Name: name}, dbstore.Options{Mem: space})
		if e != nil {
			t.Fatal(e)
		}
		return h
	}
	src := memory("source")
	defer src.Close()
	for _, id := range []string{"a", "b", "isolated"} {
		if _, err := call(t, src, OpNodePut, Input{Node: &dbstore.Node{ID: id, Type: "person", Props: []byte{0, 255}}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := call(t, src, OpEdgePut, Input{Edge: &dbstore.Edge{From: "a", To: "b", Type: "knows", Props: []byte{1, 2, 255}}}); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(t.TempDir(), "first")
	if _, err := call(t, src, OpExport, Input{Out: first}); err != nil {
		t.Fatal(err)
	}
	p, err := (bonsaidb.Adapter{}).Create(ctx, dbstore.Target{Engine: "bonsai", Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "graph")}, dbstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := call(t, p, OpImport, Input{In: first}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, p, OpImport, Input{In: first}); err != nil {
		t.Fatalf("idempotent import: %v", err)
	}
	if _, err := call(t, p, OpNodePut, Input{Node: &dbstore.Node{ID: "extra", Type: "temporary"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, p, OpImport, Input{In: first, Replace: true}); err != nil {
		t.Fatalf("persistent replace: %v", err)
	}
	if _, err := call(t, p, OpNodeGet, Input{ID: "extra"}); !errors.Is(err, dbstore.ErrNotFound) {
		t.Fatalf("persistent replace left extra node: %v", err)
	}
	second := filepath.Join(t.TempDir(), "second")
	if _, err := call(t, p, OpExport, Input{Out: second}); err != nil {
		t.Fatal(err)
	}
	dst := memory("destination")
	defer dst.Close()
	if _, err := call(t, dst, OpImport, Input{In: second}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "isolated"} {
		a, e := call(t, src, OpNodeGet, Input{ID: id})
		if e != nil {
			t.Fatal(e)
		}
		b, e := call(t, dst, OpNodeGet, Input{ID: id})
		if e != nil {
			t.Fatal(e)
		}
		if !reflect.DeepEqual(a.Result, b.Result) {
			t.Fatalf("node %s: %v != %v", id, a.Result, b.Result)
		}
	}
	a, e := call(t, src, OpEdgeGet, Input{From: "a", Type: "knows", To: "b"})
	if e != nil {
		t.Fatal(e)
	}
	b, e := call(t, dst, OpEdgeGet, Input{From: "a", Type: "knows", To: "b"})
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(a.Result, b.Result) {
		t.Fatalf("edge: %v != %v", a.Result, b.Result)
	}
	if _, err := call(t, dst, OpNodePut, Input{Node: &dbstore.Node{ID: "a", Type: "changed"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, dst, OpImport, Input{In: second}); !errors.Is(err, dbstore.ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	if _, err := call(t, dst, OpImport, Input{In: second, Replace: true}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if _, err := call(t, dst, OpNodeGet, Input{ID: "isolated"}); err != nil {
		t.Fatalf("isolated after replace: %v", err)
	}
}

func TestGraphInterchangeRefusesExternalDQL(t *testing.T) {
	ctx := context.Background()
	h, err := (bonsaidb.Adapter{}).Create(ctx, dbstore.Target{Engine: "bonsai", Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "graph")}, dbstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := call(t, h, OpSchemaApply, Input{Schema: "external_name: string @index(exact) ."}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, h, OpMutate, Input{SetNQuads: `_:a <external_name> "x" .`}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, h, OpExport, Input{Out: filepath.Join(t.TempDir(), "export")}); !errors.Is(err, dbstore.ErrUnsupported) {
		t.Fatalf("external export: %v", err)
	}
	if _, err := call(t, h, OpNodePut, Input{Node: &dbstore.Node{ID: "x"}}); !errors.Is(err, dbstore.ErrUnsupported) {
		t.Fatalf("typed write over external schema: %v", err)
	}
}
