package blevedb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blevesearch/bleve/v2"

	"github.com/qiangli/yoke/pkg/dbstore"
)

func setup(t *testing.T) (*dbstore.MemSpace, map[string]dbstore.Target) {
	t.Helper()
	s, err := dbstore.NewMemSpace(dbstore.LifetimeOwner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, map[string]dbstore.Target{
		"persistent": {Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "idx")},
		"memory":     {Mode: dbstore.Memory, Session: s.ID(), Name: "idx"},
	}
}

func hits(t *testing.T, idx bleve.Index, q string) int {
	t.Helper()
	r, err := idx.Search(bleve.NewSearchRequest(bleve.NewMatchQuery(q)))
	if err != nil {
		t.Fatal(err)
	}
	return int(r.Total)
}

func TestLifecycleBothModes(t *testing.T) {
	ctx := context.Background()
	a := Adapter{}
	s, tgs := setup(t)
	for mode, tg := range tgs {
		t.Run(mode, func(t *testing.T) {
			o := dbstore.Options{Mem: s, Wait: 200 * time.Millisecond}
			if _, err := a.Open(ctx, tg, o); !errors.Is(err, dbstore.ErrNotFound) {
				t.Fatalf("open missing: %v", err)
			}
			if tg.Path != "" {
				if _, err := os.Stat(tg.Path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("open of a missing target created it")
				}
			}
			h, err := a.Create(ctx, tg, o)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.Create(ctx, tg, o); !errors.Is(err, dbstore.ErrExists) {
				t.Fatalf("create existing: %v", err)
			}
			idx := h.(*DB).Index()
			if err := idx.Index("d1", map[string]any{"body": "rod not fish"}); err != nil {
				t.Fatal(err)
			}
			if err := idx.Index("d2", map[string]any{"body": "fish market"}); err != nil {
				t.Fatal(err)
			}
			if err := idx.Delete("d2"); err != nil {
				t.Fatal(err)
			}
			if err := idx.Close(); !errors.Is(err, dbstore.ErrInvalidInput) {
				t.Fatalf("closing the engine behind the handle: %v", err)
			}
			if err := a.Drop(ctx, tg, o); !errors.Is(err, dbstore.ErrBusy) {
				t.Fatalf("drop while open: %v", err)
			}
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			r, err := a.Open(ctx, tg, dbstore.Options{Mem: s, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if n := hits(t, r.(*DB).Index(), "fish"); n != 1 {
				t.Fatalf("hits after reopen %d", n)
			}
			if err := r.(*DB).Index().Index("d3", map[string]any{"body": "x"}); !errors.Is(err, dbstore.ErrReadOnly) {
				t.Fatalf("write on read-only: %v", err)
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

func TestWriterLockBoundedAndCancellable(t *testing.T) {
	ctx := context.Background()
	a := Adapter{}
	s, tgs := setup(t)
	for mode, tg := range tgs {
		t.Run(mode, func(t *testing.T) {
			h, err := a.Create(ctx, tg, dbstore.Options{Mem: s, Exclusive: true})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			start := time.Now()
			if _, err := a.Open(ctx, tg, dbstore.Options{Mem: s, Wait: 150 * time.Millisecond}); !errors.Is(err, dbstore.ErrBusy) {
				t.Fatalf("second writer: %v", err)
			}
			cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			defer cancel()
			if _, err := a.Open(cctx, tg, dbstore.Options{Mem: s, Wait: time.Minute}); dbstore.Code(err) != "timeout" {
				t.Fatalf("cancelled wait: %v", err)
			}
			if time.Since(start) > 5*time.Second {
				t.Fatal("waits not bounded")
			}
		})
	}
}

func TestMemoryIndexWritesNothingAndEndsWithOwner(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("TMPDIR", dir)
	ctx := context.Background()
	s, tgs := setup(t)
	h, err := Adapter{}.Create(ctx, tgs["memory"], dbstore.Options{Mem: s})
	if err != nil {
		t.Fatal(err)
	}
	b := h.(*DB).Index().NewBatch()
	for i := 0; i < 2000; i++ {
		b.Index(string(rune('a'+i%26))+time.Duration(i).String(), map[string]any{"body": "memory only"})
	}
	if err := h.(*DB).Index().Batch(b); err != nil {
		t.Fatal(err)
	}
	h.Close()
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("memory index wrote %v", ents)
	}
	s.Close()
	if _, err := (Adapter{}).Open(ctx, tgs["memory"], dbstore.Options{Mem: s}); !errors.Is(err, dbstore.ErrLifetimeEnded) {
		t.Fatalf("after owner close: %v", err)
	}
}

// A closed handle — and any Index() it handed out — cannot reach the shared
// memory index, so a later exclusive holder is alone; writes on one index
// are admitted with a bounded wait.
func TestClosedHandleRejectsUseAndWritesAreGated(t *testing.T) {
	ctx := context.Background()
	s, tgs := setup(t)
	for mode, tg := range tgs {
		t.Run(mode, func(t *testing.T) {
			o := dbstore.Options{Mem: s, Wait: 50 * time.Millisecond}
			h, err := Adapter{}.Create(ctx, tg, o)
			if err != nil {
				t.Fatal(err)
			}
			d := h.(*DB)
			idx := d.Index()
			leave, err := d.gate.Enter(ctx, time.Second) // a writer in progress
			if err != nil {
				t.Fatal(err)
			}
			if err := idx.Index("a", map[string]string{"t": "x"}); !errors.Is(err, dbstore.ErrBusy) {
				t.Fatalf("contended write: %v, want busy", err)
			}
			leave()
			if err := idx.Index("a", map[string]string{"t": "x"}); err != nil {
				t.Fatal(err)
			}
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			o.Exclusive = true
			ex, err := Adapter{}.Open(ctx, tg, o)
			if err != nil {
				t.Fatalf("exclusive open after release: %v", err)
			}
			defer ex.Close()
			if err := idx.Index("b", map[string]string{"t": "y"}); !errors.Is(err, dbstore.ErrLifetimeEnded) {
				t.Fatalf("write through closed handle: %v", err)
			}
			if _, err := idx.DocCount(); !errors.Is(err, dbstore.ErrLifetimeEnded) {
				t.Fatalf("read through closed handle: %v", err)
			}
			if _, err := d.Index().Advanced(); !errors.Is(err, dbstore.ErrLifetimeEnded) {
				t.Fatalf("advanced through closed handle: %v", err)
			}
		})
	}
}

func TestCapsPerMode(t *testing.T) {
	c := Adapter{}.Caps()
	if c.Modes[dbstore.Memory].Exclusion.EnforcedBy != "owner" || c.Modes[dbstore.Persistent].Exclusion.EnforcedBy != "native-lock" ||
		!c.Modes[dbstore.Memory].Sharing.OwnerRequiredForMultiprocess {
		t.Fatalf("per-mode caps %+v", c.Modes)
	}
}
