package bonsaidb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/qiangli/bonsai/pkg/bonsai"

	"github.com/qiangli/yoke/pkg/dbstore"
)

func TestPersistentLifecycle(t *testing.T) {
	ctx := context.Background()
	a := Adapter{}
	tg := dbstore.Target{Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "g")}
	o := dbstore.Options{Wait: 200 * time.Millisecond}
	if _, err := a.Open(ctx, tg, o); !errors.Is(err, dbstore.ErrNotFound) {
		t.Fatalf("open missing: %v", err)
	}
	if _, err := os.Stat(tg.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("open of a missing target created it")
	}
	h, err := a.Create(ctx, tg, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Create(ctx, tg, o); !errors.Is(err, dbstore.ErrExists) {
		t.Fatalf("create existing: %v", err)
	}
	// Process-global engine: a second open in this process is busy, bounded.
	if _, err := a.Open(ctx, tg, o); !errors.Is(err, dbstore.ErrBusy) {
		t.Fatalf("second open in process: %v", err)
	}
	b := h.(*DB).Bonsai()
	if err := b.Alter(ctx, "name: string @index(exact) ."); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Mutate(ctx, &api.Mutation{SetNquads: []byte(`_:a <name> "Alice" .`), CommitNow: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.(*DB).Update(ctx, func(dbstore.GraphTx) error { return nil }); !errors.Is(err, dbstore.ErrUnsupported) {
		t.Fatalf("memgraph surface on bonsai: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := a.Open(ctx, tg, dbstore.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.(*DB).Bonsai().Query(ctx, `{ q(func: eq(name, "Alice")) { name } }`)
	if err != nil || !strings.Contains(string(resp.Json), "Alice") {
		t.Fatalf("query after reopen: %s %v", resp.GetJson(), err)
	}
	if _, err := r.(*DB).Bonsai().Mutate(ctx, &api.Mutation{SetNquads: []byte(`_:b <name> "Bob" .`), CommitNow: true}); !errors.Is(err, bonsai.ErrReadOnly) {
		t.Fatalf("mutate on read-only: %v", err)
	}
	r.Close()
	if err := a.Drop(ctx, tg, o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tg.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("drop left the directory")
	}
}

func TestMemoryModeIsTheMemGraphSupplement(t *testing.T) {
	ctx := context.Background()
	a := Adapter{}
	s, err := dbstore.NewMemSpace(dbstore.LifetimeMCPSession)
	if err != nil {
		t.Fatal(err)
	}
	tg := dbstore.Target{Mode: dbstore.Memory, Session: s.ID(), Name: "g"}
	o := dbstore.Options{Mem: s, Wait: 100 * time.Millisecond}
	if _, err := a.Open(ctx, tg, o); !errors.Is(err, dbstore.ErrNotFound) {
		t.Fatalf("open missing: %v", err)
	}
	h, err := a.Create(ctx, tg, o)
	if err != nil {
		t.Fatal(err)
	}
	if h.(*DB).Bonsai() != nil {
		t.Fatal("memory mode opened a bonsai engine")
	}
	if err := h.(*DB).Update(ctx, func(tx dbstore.GraphTx) error {
		if _, err := tx.PutNode(dbstore.Node{ID: "a", Type: "person"}, dbstore.Cond{}); err != nil {
			return err
		}
		_, err := tx.PutNode(dbstore.Node{ID: "b", Type: "person"}, dbstore.Cond{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h.Close()
	r, err := a.Open(ctx, tg, dbstore.Options{Mem: s, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.(*DB).View(ctx, func(tx dbstore.GraphTx) error {
		if _, ok, err := tx.GetNode("b"); !ok || err != nil {
			return fmt.Errorf("node b: %v %v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.(*DB).Update(ctx, func(dbstore.GraphTx) error { return nil }); !errors.Is(err, dbstore.ErrReadOnly) {
		t.Fatalf("update on read-only: %v", err)
	}
	if err := a.Drop(ctx, tg, o); !errors.Is(err, dbstore.ErrBusy) {
		t.Fatalf("drop while open: %v", err)
	}
	r.Close()
	s.Close()
	if _, err := a.Open(ctx, tg, o); !errors.Is(err, dbstore.ErrLifetimeEnded) {
		t.Fatalf("after session end: %v", err)
	}
}

// TestSecondProcessWriterIsRefused proves Badger's directory flock excludes
// a writer in a genuinely separate process, bounded by the wait.
func TestSecondProcessWriterIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a child process")
	}
	ctx := context.Background()
	tg := dbstore.Target{Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "held")}
	h, err := Adapter{}.Create(ctx, tg, dbstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperChildWriter$", "-test.v")
	cmd.Env = append(os.Environ(), "BONSAIDB_CHILD="+tg.Path)
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "child: busy") {
		t.Fatalf("child writer while held: %s", out)
	}
}

func TestHelperChildWriter(t *testing.T) {
	p := os.Getenv("BONSAIDB_CHILD")
	if p == "" {
		t.Skip("child helper")
	}
	h, err := Adapter{}.Open(context.Background(), dbstore.Target{Mode: dbstore.Persistent, Path: p}, dbstore.Options{Wait: 300 * time.Millisecond})
	if err != nil {
		fmt.Println("child:", dbstore.Code(err))
		return
	}
	h.Close()
	fmt.Println("child: opened")
}

func TestClosedMemoryHandleRejectsUse(t *testing.T) {
	ctx := context.Background()
	s, err := dbstore.NewMemSpace(dbstore.LifetimeOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tg := dbstore.Target{Mode: dbstore.Memory, Session: s.ID(), Name: "g"}
	o := dbstore.Options{Mem: s, Wait: 50 * time.Millisecond}
	h, err := Adapter{}.Create(ctx, tg, o)
	if err != nil {
		t.Fatal(err)
	}
	old := h.(*DB)
	old.Close()
	o.Exclusive = true
	ex, err := Adapter{}.Open(ctx, tg, o)
	if err != nil {
		t.Fatalf("exclusive open after release: %v", err)
	}
	defer ex.Close()
	if err := old.Update(ctx, func(tx dbstore.GraphTx) error {
		_, err := tx.PutNode(dbstore.Node{ID: "a", Type: "t"}, dbstore.Cond{})
		return err
	}); !errors.Is(err, dbstore.ErrLifetimeEnded) {
		t.Fatalf("update on closed handle: %v", err)
	}
	if err := old.View(ctx, func(dbstore.GraphTx) error { return nil }); !errors.Is(err, dbstore.ErrLifetimeEnded) {
		t.Fatalf("view on closed handle: %v", err)
	}
	c := Adapter{}.Caps()
	if c.Modes[dbstore.Memory].Exclusion.NonBashyClients != "blocked" || c.Modes[dbstore.Persistent].Exclusion.NonBashyClients != "cooperative" ||
		c.Exclusion.NonBashyClients != "cooperative" {
		t.Fatalf("caps exclusion %+v / summary %+v", c.Modes, c.Exclusion)
	}
}
