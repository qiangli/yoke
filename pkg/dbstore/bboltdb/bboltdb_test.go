package bboltdb

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

	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/dbstoretest"
)

func setup(t *testing.T) (*dbstore.MemSpace, map[string]dbstore.Target) {
	t.Helper()
	s, err := dbstore.NewMemSpace(dbstore.LifetimeOwner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, map[string]dbstore.Target{
		"persistent": {Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "kv.db")},
		"memory":     {Mode: dbstore.Memory, Session: s.ID(), Name: "kv"},
	}
}

func TestLifecycleBothModes(t *testing.T) {
	ctx := context.Background()
	a := Adapter{}
	s, tgs := setup(t)
	for mode, tg := range tgs {
		t.Run(mode, func(t *testing.T) {
			o := dbstore.Options{Mem: s, Wait: 100 * time.Millisecond}
			if _, err := a.Open(ctx, tg, o); !errors.Is(err, dbstore.ErrNotFound) {
				t.Fatalf("open missing: %v", err)
			}
			if tg.Path != "" {
				if _, err := os.Stat(tg.Path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("open of a missing target created it")
				}
			}
			// The handle's own wait bounds writer admission: give the eight
			// contending conformance writers room to queue behind fsyncs.
			h, err := a.Create(ctx, tg, dbstore.Options{Mem: s, Wait: 10 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.Create(ctx, tg, o); !errors.Is(err, dbstore.ErrExists) {
				t.Fatalf("create existing: %v", err)
			}
			kv := h.(*DB)
			dbstoretest.RunKVConformance(t, kv)
			bin := []byte{0, 0xff, 0}
			if err := kv.Update(ctx, func(tx dbstore.KVTx) error {
				if err := tx.CreateBucket([]byte("b")); err != nil {
					return err
				}
				return tx.Put([]byte("b"), []byte("k"), bin)
			}); err != nil {
				t.Fatal(err)
			}
			if err := a.Drop(ctx, tg, o); !errors.Is(err, dbstore.ErrBusy) {
				t.Fatalf("drop while open: %v", err)
			}
			h.Close()
			r, err := a.Open(ctx, tg, dbstore.Options{Mem: s, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			_ = r.(*DB).View(ctx, func(tx dbstore.KVTx) error {
				v, ok, err := tx.Get([]byte("b"), []byte("k"))
				if err != nil || !ok || string(v) != string(bin) {
					t.Fatalf("reopen get %x %v %v", v, ok, err)
				}
				return nil
			})
			if err := r.(*DB).Update(ctx, func(tx dbstore.KVTx) error { return nil }); !errors.Is(err, dbstore.ErrReadOnly) {
				t.Fatalf("update on read-only: %v", err)
			}
			r.Close()
			if err := a.Drop(ctx, tg, o); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Open(ctx, tg, o); !errors.Is(err, dbstore.ErrNotFound) {
				t.Fatalf("open after drop: %v", err)
			}
		})
	}
}

func TestExclusiveAndLockWaitBounded(t *testing.T) {
	ctx := context.Background()
	a := Adapter{}
	s, tgs := setup(t)
	for mode, tg := range tgs {
		t.Run(mode, func(t *testing.T) {
			h, err := a.Create(ctx, tg, dbstore.Options{Mem: s, Exclusive: true})
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			_, err = a.Open(ctx, tg, dbstore.Options{Mem: s, ReadOnly: true, Wait: 150 * time.Millisecond})
			if !errors.Is(err, dbstore.ErrBusy) {
				t.Fatalf("reader during exclusive: %v", err)
			}
			if time.Since(start) > 3*time.Second {
				t.Fatal("lock wait not bounded")
			}
			cctx, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := a.Open(cctx, tg, dbstore.Options{Mem: s, Wait: time.Minute}); err == nil {
				t.Fatal("open with cancelled ctx succeeded during exclusive hold")
			}
			h.Close()
			r, err := a.Open(ctx, tg, dbstore.Options{Mem: s, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			r.Close()
		})
	}
}

func TestMemoryEndsWithOwner(t *testing.T) {
	ctx := context.Background()
	s, tgs := setup(t)
	h, err := Adapter{}.Create(ctx, tgs["memory"], dbstore.Options{Mem: s})
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
	s.Close()
	if _, err := (Adapter{}).Open(ctx, tgs["memory"], dbstore.Options{Mem: s}); !errors.Is(err, dbstore.ErrLifetimeEnded) {
		t.Fatalf("after owner close: %v", err)
	}
}

// TestSecondProcessIsRefusedWhileWriterHolds proves the flock is a real
// cross-process exclusion, bounded by the wait, and released on close.
func TestSecondProcessIsRefusedWhileWriterHolds(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a child process")
	}
	ctx := context.Background()
	tg := dbstore.Target{Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "held.db")}
	h, err := Adapter{}.Create(ctx, tg, dbstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	child := func() string {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperChildOpen$", "-test.v")
		cmd.Env = append(os.Environ(), "BBOLTDB_CHILD="+tg.Path)
		b, _ := cmd.CombinedOutput()
		return string(b)
	}
	if out := child(); !strings.Contains(out, "child: busy") {
		t.Fatalf("child while held: %s", out)
	}
	h.Close()
	if out := child(); !strings.Contains(out, "child: opened") {
		t.Fatalf("child after release: %s", out)
	}
}

func TestHelperChildOpen(t *testing.T) {
	p := os.Getenv("BBOLTDB_CHILD")
	if p == "" {
		t.Skip("child helper")
	}
	h, err := Adapter{}.Open(context.Background(), dbstore.Target{Mode: dbstore.Persistent, Path: p}, dbstore.Options{Wait: 200 * time.Millisecond})
	if err != nil {
		fmt.Println("child:", dbstore.Code(err))
		return
	}
	h.Close()
	fmt.Println("child: opened")
}

// A writer waiting behind another writer on the same handle is admitted
// within Options.Wait or fails busy, and observes ctx — bbolt's own writer
// mutex would wait forever.
func TestWriterAdmissionBoundedAndCancellable(t *testing.T) {
	ctx := context.Background()
	s, tgs := setup(t)
	for mode, tg := range tgs {
		t.Run(mode, func(t *testing.T) {
			h, err := Adapter{}.Create(ctx, tg, dbstore.Options{Mem: s, Wait: 50 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			kv := h.(*DB)
			hold, done := make(chan struct{}), make(chan struct{})
			go kv.Update(ctx, func(dbstore.KVTx) error { close(hold); <-done; return nil })
			<-hold
			start := time.Now()
			if err := kv.Update(ctx, func(dbstore.KVTx) error { return nil }); !errors.Is(err, dbstore.ErrBusy) {
				t.Fatalf("contended writer: %v, want busy", err)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("admission wait not bounded: %s", d)
			}
			cctx, cancel := context.WithCancel(ctx)
			cancel()
			if err := kv.Update(cctx, func(dbstore.KVTx) error { return nil }); dbstore.Code(err) != "cancelled" {
				t.Fatalf("cancelled writer: %v", err)
			}
			if err := kv.View(ctx, func(dbstore.KVTx) error { return nil }); err != nil {
				t.Fatalf("reader blocked by a writer: %v", err)
			}
			close(done)
		})
	}
}

// A closed memory handle cannot reach the shared database, so an exclusive
// holder admitted after its Release really is alone.
func TestClosedHandleRejectsUse(t *testing.T) {
	ctx := context.Background()
	s, tgs := setup(t)
	for mode, tg := range tgs {
		t.Run(mode, func(t *testing.T) {
			o := dbstore.Options{Mem: s, Wait: 50 * time.Millisecond}
			h, err := Adapter{}.Create(ctx, tg, o)
			if err != nil {
				t.Fatal(err)
			}
			old := h.(*DB)
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			if err := old.Close(); err != nil {
				t.Fatalf("second close: %v", err)
			}
			o.Exclusive = true
			ex, err := Adapter{}.Open(ctx, tg, o)
			if err != nil {
				t.Fatalf("exclusive open after release: %v", err)
			}
			defer ex.Close()
			if err := old.Update(ctx, func(tx dbstore.KVTx) error { return tx.CreateBucket([]byte("x")) }); !errors.Is(err, dbstore.ErrLifetimeEnded) {
				t.Fatalf("update on closed handle: %v", err)
			}
			if err := old.View(ctx, func(dbstore.KVTx) error { return nil }); !errors.Is(err, dbstore.ErrLifetimeEnded) {
				t.Fatalf("view on closed handle: %v", err)
			}
		})
	}
}

func TestCapsPerMode(t *testing.T) {
	c := Adapter{}.Caps()
	p, m := c.Modes[dbstore.Persistent], c.Modes[dbstore.Memory]
	if p.Exclusion.EnforcedBy != "native-lock" || m.Exclusion.EnforcedBy != "owner" || m.Via != "supplemental-adapter:memkv" {
		t.Fatalf("per-mode exclusion %+v / %+v", p.Exclusion, m.Exclusion)
	}
	if c.Sharing.NativeMultiprocess || !c.Sharing.OwnerRequiredForMultiprocess || c.Exclusion.EnforcedBy != "owner+native-lock" {
		t.Fatalf("summary %+v %+v", c.Sharing, c.Exclusion)
	}
}
