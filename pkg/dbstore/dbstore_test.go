package dbstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveAliasesShareOneIdentity(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "notes.db")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alias.db")
	if err := os.Symlink(f, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	spellings := []string{f, link, filepath.Join(dir, ".", "sub", "..", "notes.db")}
	if _, err := os.Stat(filepath.Join(dir, "NOTES.DB")); err == nil {
		spellings = append(spellings, filepath.Join(dir, "NOTES.DB")) // case-insensitive volume
	}
	want := ""
	for _, p := range spellings {
		id, err := Resolve(Target{Engine: "sqlite", Mode: Persistent, Path: p})
		if err != nil {
			t.Fatal(err)
		}
		if want == "" {
			want = id.ID
		}
		if id.ID != want {
			t.Fatalf("%s resolved to %s, want %s", p, id.ID, want)
		}
	}
	if !strings.HasPrefix(want, "db:sqlite:file:") || !strings.HasSuffix(want, "notes.db") {
		t.Fatalf("id %q", want)
	}
	if k, n := (Identity{ID: want}).ClaimUse(); k != "db" || n != want {
		t.Fatalf("claim use %s %s", k, n)
	}
}

func TestResolveRejectsAmbiguousTargets(t *testing.T) {
	for _, tt := range []Target{
		{Engine: "sqlite", Mode: "", Path: "x"},
		{Engine: "", Mode: Persistent, Path: "x"},
		{Engine: "sqlite", Mode: Persistent},
		{Engine: "sqlite", Mode: Persistent, Path: "x", Session: "s"},
		{Engine: "sqlite", Mode: Memory, Name: "n"},
		{Engine: "sqlite", Mode: Memory, Session: "s", Name: "n", Path: "x"},
	} {
		if _, err := Resolve(tt); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%+v: err %v, want invalid_input", tt, err)
		}
	}
	id, err := Resolve(Target{Engine: "bbolt", Mode: Memory, Session: "s-1", Name: "cache"})
	if err != nil || id.ID != "db:bbolt:mem:s-1/cache" {
		t.Fatalf("memory id %q %v", id.ID, err)
	}
}

func TestMemSpaceLifecycle(t *testing.T) {
	s, err := NewMemSpace(LifetimeOwner)
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := NewMemSpace(LifetimeOwner)
	if s.ID() == s2.ID() {
		t.Fatal("session ids reused")
	}
	id, _ := Resolve(Target{Engine: "bbolt", Mode: Memory, Session: s.ID(), Name: "a"})
	if _, err := s.Acquire(id, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("acquire missing: %v", err)
	}
	kv := NewMemKV()
	if err := s.Put(id, kv, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(id, kv, false); !errors.Is(err, ErrExists) {
		t.Fatalf("second put: %v", err)
	}
	if _, err := s.Acquire(id, false); err != nil { // second client
		t.Fatal(err)
	}
	if _, err := s.Acquire(id, true); !errors.Is(err, ErrBusy) {
		t.Fatalf("exclusive while shared: %v", err)
	}
	s.Release(id) // first client disconnects; second keeps the database
	if err := s.Drop(id); !errors.Is(err, ErrBusy) {
		t.Fatalf("drop with a live handle: %v", err)
	}
	s.Release(id)
	if _, err := s.Acquire(id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(id, false); !errors.Is(err, ErrBusy) {
		t.Fatalf("shared while exclusive: %v", err)
	}
	s.Release(id)
	if got := s.Names("bbolt"); len(got) != 1 || got[0] != "a" {
		t.Fatalf("names %v", got)
	}
	if err := s.Drop(id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(id, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after drop: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(id, false); !errors.Is(err, ErrLifetimeEnded) {
		t.Fatalf("after close: %v", err)
	}
	if err := (Options{Mem: s}).Validate(id, false); !errors.Is(err, ErrLifetimeEnded) {
		t.Fatalf("validate after close: %v", err)
	}
	if err := (Options{Mem: s2}).Validate(id, false); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("foreign session: %v", err)
	}
}

func TestMemKVAtomicAndIsolated(t *testing.T) {
	ctx := context.Background()
	kv := NewMemKV()
	b := []byte("b")
	if err := kv.Update(ctx, func(tx KVTx) error { return tx.CreateBucket(b) }); err != nil {
		t.Fatal(err)
	}
	// A failed transaction leaves nothing behind.
	boom := errors.New("boom")
	if err := kv.Update(ctx, func(tx KVTx) error {
		if err := tx.Put(b, []byte("k"), []byte("v")); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	// A cancelled ctx at commit rolls back.
	cctx, cancel := context.WithCancel(ctx)
	err := kv.Update(cctx, func(tx KVTx) error {
		cancel()
		return tx.Put(b, []byte("k"), []byte("v"))
	})
	if Code(err) != "cancelled" {
		t.Fatalf("cancel: %v", err)
	}
	_ = kv.View(ctx, func(tx KVTx) error {
		if _, ok, _ := tx.Get(b, []byte("k")); ok {
			t.Fatal("rolled-back write visible")
		}
		return nil
	})
	// Binary values round-trip; readers see a stable snapshot mid-write.
	bin := []byte{0, 0xff, 0x80, 0}
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error)
	go func() {
		done <- kv.Update(ctx, func(tx KVTx) error {
			if err := tx.Put(b, []byte("bin"), bin); err != nil {
				return err
			}
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	_ = kv.View(ctx, func(tx KVTx) error {
		if _, ok, _ := tx.Get(b, []byte("bin")); ok {
			t.Fatal("uncommitted write visible to a reader")
		}
		return nil
	})
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_ = kv.View(ctx, func(tx KVTx) error {
		v, ok, _ := tx.Get(b, []byte("bin"))
		if !ok || string(v) != string(bin) {
			t.Fatalf("binary %x", v)
		}
		if err := tx.Put(b, []byte("x"), nil); !errors.Is(err, ErrReadOnly) {
			t.Fatalf("write in view: %v", err)
		}
		return nil
	})
	if err := kv.View(ctx, func(tx KVTx) error { _, _, err := tx.Get([]byte("nope"), []byte("k")); return err }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing bucket: %v", err)
	}
}

func TestRetryIsBounded(t *testing.T) {
	start := time.Now()
	err := Retry(context.Background(), 60*time.Millisecond, func() error { return ErrBusy })
	if !errors.Is(err, ErrBusy) || time.Since(start) > 2*time.Second {
		t.Fatalf("retry %v after %v", err, time.Since(start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Retry(ctx, time.Minute, func() error { return ErrBusy }); Code(err) != "cancelled" {
		t.Fatalf("cancelled retry: %v", err)
	}
}

func TestCode(t *testing.T) {
	for err, want := range map[error]string{
		nil:                              "",
		fmt.Errorf("x: %w", ErrConflict): "conflict",
		context.DeadlineExceeded:         "timeout",
		errors.New("disk on fire"):       "backend_error",
		Backend("e", errors.New("boom")): "backend_error",
		Backend("e", ErrNotFound):        "not_found",
		FromContext(context.Canceled):    "cancelled",
	} {
		if got := Code(err); got != want {
			t.Errorf("Code(%v) = %q, want %q", err, got, want)
		}
	}
}
