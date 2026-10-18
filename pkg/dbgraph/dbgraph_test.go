package dbgraph

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
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
