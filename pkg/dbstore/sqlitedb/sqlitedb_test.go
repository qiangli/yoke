package sqlitedb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dbstore"
)

func memSpace(t *testing.T) *dbstore.MemSpace {
	t.Helper()
	s, err := dbstore.NewMemSpace(dbstore.LifetimeOwner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func targets(t *testing.T, s *dbstore.MemSpace) map[string]dbstore.Target {
	return map[string]dbstore.Target{
		"persistent": {Engine: Engine, Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "x.db")},
		"memory":     {Engine: Engine, Mode: dbstore.Memory, Session: s.ID(), Name: "x"},
	}
}

func sqlOf(h dbstore.Handle) *DB { return h.(*DB) }

func TestLifecycleBothModes(t *testing.T) {
	ctx := context.Background()
	a := Adapter{}
	s := memSpace(t)
	for mode, tg := range targets(t, s) {
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
			db := sqlOf(h).SQL()
			for _, q := range []string{
				"create table t(id integer primary key, b blob, r real)",
				"insert into t values(1, x'00ff80', 0.1)",
				"update t set r = 0.30000000000000004 where id = 1",
				"insert into t values(2, x'', 2)",
				"delete from t where id = 2",
			} {
				if _, err := db.ExecContext(ctx, q); err != nil {
					t.Fatal(q, err)
				}
			}
			if _, err := db.ExecContext(ctx, "insert into t values(1, null, 0)"); !errors.Is(Classify(err), dbstore.ErrConflict) {
				t.Fatalf("same-id insert: %v", err)
			}
			// A second client opens the same target; the first one closing
			// does not end the database.
			h2, err := a.Open(ctx, tg, dbstore.Options{Mem: s, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			var b []byte
			var r float64
			if err := sqlOf(h2).SQL().QueryRowContext(ctx, "select b, r from t where id = 1").Scan(&b, &r); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%x %v", b, r) != "00ff80 0.30000000000000004" {
				t.Fatalf("round trip %x %v", b, r)
			}
			if _, err := sqlOf(h2).SQL().ExecContext(ctx, "insert into t values(9, null, 0)"); !errors.Is(Classify(err), dbstore.ErrReadOnly) {
				t.Fatalf("write on read-only handle: %v", err)
			}
			// The owner refuses to drop a memory database with a live handle.
			// An idle SQLite reader holds no file lock, so a persistent drop
			// cannot see it: that guard is the lifecycle layer's db hold.
			if mode == "memory" {
				if err := a.Drop(ctx, tg, o); !errors.Is(err, dbstore.ErrBusy) {
					t.Fatalf("drop while open: %v", err)
				}
			}
			h2.Close()
			if err := a.Drop(ctx, tg, o); err != nil {
				t.Fatal(err)
			}
			if tg.Path != "" {
				for _, f := range []string{tg.Path, tg.Path + "-wal", tg.Path + "-shm"} {
					if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("drop left %s", f)
					}
				}
			}
			if _, err := a.Open(ctx, tg, o); !errors.Is(err, dbstore.ErrNotFound) {
				t.Fatalf("open after drop: %v", err)
			}
		})
	}
}

func TestPersistentSurvivesReopenMemoryEndsWithLifetime(t *testing.T) {
	ctx := context.Background()
	a := Adapter{}
	s := memSpace(t)
	tgs := targets(t, s)
	for _, tg := range tgs {
		h, err := a.Create(ctx, tg, dbstore.Options{Mem: s})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sqlOf(h).SQL().ExecContext(ctx, "create table t(v); insert into t values('kept')"); err != nil {
			t.Fatal(err)
		}
		h.Close()
	}
	h, err := a.Open(ctx, tgs["persistent"], dbstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var v string
	if err := sqlOf(h).SQL().QueryRow("select v from t").Scan(&v); err != nil || v != "kept" {
		t.Fatalf("persistent reopen: %q %v", v, err)
	}
	h.Close()
	// The memory database outlives its handles while the owner lives ...
	h, err = a.Open(ctx, tgs["memory"], dbstore.Options{Mem: s})
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
	// ... and ends with the owner: lifetime_ended, never a fresh empty db.
	s.Close()
	if _, err := a.Open(ctx, tgs["memory"], dbstore.Options{Mem: s}); !errors.Is(err, dbstore.ErrLifetimeEnded) {
		t.Fatalf("after owner close: %v", err)
	}
}

func TestMemoryModeWritesNothingToDisk(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("TMPDIR", dir)
	ctx := context.Background()
	s := memSpace(t)
	h, err := Adapter{}.Create(ctx, dbstore.Target{Mode: dbstore.Memory, Session: s.ID(), Name: "big"}, dbstore.Options{Mem: s})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := sqlOf(h).SQL().ExecContext(ctx, `create table t(v);
		with recursive c(i) as (select 1 union all select i+1 from c where i < 20000)
		insert into t select randomblob(256) from c`); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Fatalf("memory mode wrote %v", ents)
	}
}

func TestExclusiveBlocksOtherClients(t *testing.T) {
	ctx := context.Background()
	a := Adapter{}
	s := memSpace(t)
	for mode, tg := range targets(t, s) {
		t.Run(mode, func(t *testing.T) {
			h, err := a.Create(ctx, tg, dbstore.Options{Mem: s})
			if err != nil {
				t.Fatal(err)
			}
			sqlOf(h).SQL().ExecContext(ctx, "create table t(v)")
			h.Close()
			ex, err := a.Open(ctx, tg, dbstore.Options{Mem: s, Exclusive: true})
			if err != nil {
				t.Fatal(err)
			}
			short := dbstore.Options{Mem: s, ReadOnly: true, Wait: 100 * time.Millisecond}
			if mode == "persistent" {
				// The open succeeds lazily for a reader; the read itself is refused.
				if r, err := a.Open(ctx, tg, short); err == nil {
					err = sqlOf(r).SQL().QueryRow("select count(*) from t").Scan(new(int))
					r.Close()
					if !errors.Is(Classify(err), dbstore.ErrBusy) {
						t.Fatalf("read during exclusive: %v", err)
					}
				} else if !errors.Is(err, dbstore.ErrBusy) {
					t.Fatalf("open during exclusive: %v", err)
				}
			} else if _, err := a.Open(ctx, tg, short); !errors.Is(err, dbstore.ErrBusy) {
				t.Fatalf("open during exclusive: %v", err)
			}
			ex.Close()
			r, err := a.Open(ctx, tg, short)
			if err != nil {
				t.Fatalf("open after release: %v", err)
			}
			r.Close()
		})
	}
}

func TestCancellationAndInvalidOptions(t *testing.T) {
	a := Adapter{}
	tg := dbstore.Target{Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "c.db")}
	h, err := a.Create(context.Background(), tg, dbstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = sqlOf(h).SQL().ExecContext(ctx, `create table t(v);
		with recursive c(i) as (select 1 union all select i+1 from c) insert into t select i from c`)
	if c := dbstore.Code(ClassifyCtx(ctx, err)); c != "timeout" {
		t.Fatalf("runaway statement: %v (%s), want timeout", err, c)
	}
	if _, err := a.Create(context.Background(), tg, dbstore.Options{ReadOnly: true}); !errors.Is(err, dbstore.ErrInvalidInput) {
		t.Fatalf("read-only create: %v", err)
	}
	if _, err := a.Open(context.Background(), dbstore.Target{Engine: "bbolt", Mode: dbstore.Persistent, Path: tg.Path}, dbstore.Options{}); !errors.Is(err, dbstore.ErrInvalidInput) {
		t.Fatalf("wrong engine: %v", err)
	}
}

// TestTwoProcessesShareAPersistentFile runs a genuinely separate process
// writing the same WAL file while this one writes; no write is lost.
func TestTwoProcessesShareAPersistentFile(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a child process")
	}
	ctx := context.Background()
	tg := dbstore.Target{Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "shared.db")}
	h, err := Adapter{}.Create(ctx, tg, dbstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	db := sqlOf(h).SQL()
	if _, err := db.Exec("create table t(who, i)"); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperChildWriter$")
	cmd.Env = append(os.Environ(), "SQLITEDB_CHILD="+tg.Path)
	out := make(chan error, 1)
	go func() {
		b, err := cmd.CombinedOutput()
		if err != nil {
			err = fmt.Errorf("%v: %s", err, b)
		}
		out <- err
	}()
	for i := 0; i < 200; i++ {
		if _, err := db.Exec("insert into t values('parent', ?)", i); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-out; err != nil {
		t.Fatal(err)
	}
	var n int
	db.QueryRow("select count(*) from t").Scan(&n)
	if n != 400 {
		t.Fatalf("rows %d, want 400", n)
	}
}

func TestHelperChildWriter(t *testing.T) {
	p := os.Getenv("SQLITEDB_CHILD")
	if p == "" {
		t.Skip("child helper")
	}
	h, err := Adapter{}.Open(context.Background(), dbstore.Target{Mode: dbstore.Persistent, Path: p}, dbstore.Options{Wait: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for i := 0; i < 200; i++ {
		if _, err := sqlOf(h).SQL().Exec("insert into t values('child', ?)", i); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClosedHandleRejectsUse(t *testing.T) {
	ctx := context.Background()
	s := memSpace(t)
	for mode, tg := range targets(t, s) {
		t.Run(mode, func(t *testing.T) {
			o := dbstore.Options{Mem: s, Wait: 100 * time.Millisecond}
			h, err := Adapter{}.Create(ctx, tg, o)
			if err != nil {
				t.Fatal(err)
			}
			old := sqlOf(h).SQL()
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			if err := h.Close(); err != nil {
				t.Fatalf("second close: %v", err)
			}
			o.Exclusive = true
			ex, err := Adapter{}.Open(ctx, tg, o)
			if err != nil {
				t.Fatalf("exclusive open after release: %v", err)
			}
			defer ex.Close()
			_, err = old.ExecContext(ctx, "create table t(x)")
			if err = Classify(err); !errors.Is(err, dbstore.ErrLifetimeEnded) {
				t.Fatalf("statement on closed handle: %v", err)
			}
		})
	}
	c := Adapter{}.Caps()
	if !c.Modes[dbstore.Persistent].Sharing.NativeMultiprocess || c.Modes[dbstore.Memory].Sharing.NativeMultiprocess ||
		c.Sharing.NativeMultiprocess {
		t.Fatalf("sharing must be per mode: %+v / summary %+v", c.Modes, c.Sharing)
	}
}
