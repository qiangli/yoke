// Package bonsaidb is the dbstore adapter for Bonsai (the pinned
// github.com/qiangli/bonsai embedded graph database kg and ycode already
// use), serving the graph category. The engine is reused in place through its
// semver-stable pkg/bonsai API; nothing is copied.
//
// Persistent mode is native, with two engine facts the adapter enforces
// rather than hides: Bonsai's worker/posting/schema layers are process
// globals, so one process holds at most one open Bonsai database (a second
// open waits, bounded, then fails busy); and bonsai.Open creates its
// directory, so the adapter checks existence first and opening a missing
// target is not_found. Bonsai has no in-memory mode at the pinned version,
// so memory mode is the supplemental dbstore.MemGraph (typed nodes and edges,
// not DQL).
package bonsaidb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/bonsai/pkg/bonsai"

	"github.com/qiangli/yoke/pkg/dbstore"
)

// Engine is the dbstore engine name.
const Engine = "bonsai"

// Adapter implements dbstore.Adapter.
type Adapter struct{}

var _ dbstore.Adapter = Adapter{}

func (Adapter) Engine() string { return Engine }

func (Adapter) Caps() dbstore.Caps {
	return dbstore.Summarize(dbstore.Caps{
		Engine:     Engine,
		Categories: []string{"graph"},
		Modes: map[dbstore.Mode]dbstore.ModeCaps{
			dbstore.Persistent: {
				Supported: true, Via: "native",
				Sharing: &dbstore.Sharing{NativeMultiprocess: false, OwnerRequiredForMultiprocess: true},
				// Read-only opens bypass Badger's flock: a writer excludes other writers only.
				Exclusion: &dbstore.Exclusion{Scopes: []string{"database"}, EnforcedBy: dbstore.EnforcedByNativeLock, NonBashyClients: dbstore.NonBashyCooperative},
			},
			dbstore.Memory: dbstore.MemoryModeCaps("supplemental-adapter:memgraph"),
		},
		Transactions: dbstore.Transactions{AtomicBatch: true, Isolation: "snapshot"},
		Notes: []string{
			"one open bonsai database per process (process-global engine state); a second open in the process fails busy",
			"a writable open holds Badger's directory flock; read-only opens bypass that lock, so they neither exclude nor are excluded by a writer",
			"memory mode is the supplemental memgraph adapter: typed nodes/edges with revisions, not DQL; memory writers are admitted with a wait bounded by Options.Wait",
			"a closed handle rejects every later View and Update with lifetime_ended",
			"index rebuilds use os.TempDir for scratch files (persistent mode only)",
		},
	})
}

// DB is an open graph database in either mode.
type DB struct {
	id      dbstore.Identity
	ro      bool
	wait    time.Duration
	b       *bonsai.DB
	mem     *dbstore.MemGraph
	guard   dbstore.Guard
	release func()
}

func (d *DB) Identity() dbstore.Identity { return d.id }
func (d *DB) ReadOnly() bool             { return d.ro }

// Bonsai is the native engine (nil in memory mode and after Close).
func (d *DB) Bonsai() *bonsai.DB {
	if d.guard.Closed() {
		return nil
	}
	return d.b
}

// View reads the memory-mode graph (unsupported on a persistent handle,
// which is queried through Bonsai's own API).
func (d *DB) View(ctx context.Context, fn func(dbstore.GraphTx) error) error {
	if err := d.guard.Enter(d.id); err != nil {
		return err
	}
	defer d.guard.Exit()
	if d.mem == nil {
		return fmt.Errorf("%w: %s is a bonsai database; use Bonsai()", dbstore.ErrUnsupported, d.id.ID)
	}
	return d.mem.View(ctx, fn)
}

// Update writes the memory-mode graph atomically.
func (d *DB) Update(ctx context.Context, fn func(dbstore.GraphTx) error) error {
	if err := d.guard.Enter(d.id); err != nil {
		return err
	}
	defer d.guard.Exit()
	if d.ro {
		return fmt.Errorf("%w: %s opened read-only", dbstore.ErrReadOnly, d.id.ID)
	}
	if d.mem == nil {
		return fmt.Errorf("%w: %s is a bonsai database; use Bonsai()", dbstore.ErrUnsupported, d.id.ID)
	}
	return d.mem.UpdateWithin(ctx, d.wait, fn)
}

// Close releases the handle once, after operations in flight.
func (d *DB) Close() error {
	return d.guard.Close(func() error {
		if d.release != nil {
			d.release()
		}
		if d.b != nil {
			err := d.b.Close()
			unclaimProcess()
			return dbstore.Backend(Engine, err)
		}
		return nil
	})
}

// processMu guards the one-database-per-process rule.
var (
	processMu   sync.Mutex
	processOpen bool
)

func claimProcess() error {
	processMu.Lock()
	defer processMu.Unlock()
	if processOpen {
		return fmt.Errorf("%w: this process already has a bonsai database open", dbstore.ErrBusy)
	}
	processOpen = true
	return nil
}

func unclaimProcess() {
	processMu.Lock()
	processOpen = false
	processMu.Unlock()
}

func (Adapter) Create(ctx context.Context, t dbstore.Target, o dbstore.Options) (dbstore.Handle, error) {
	id, err := resolve(t, o, true)
	if err != nil {
		return nil, err
	}
	if id.Mode == dbstore.Memory {
		g := dbstore.NewMemGraph()
		if err := o.Mem.Put(id, g, o.Exclusive); err != nil {
			return nil, err
		}
		return &DB{id: id, wait: o.WaitOrDefault(), mem: g, release: func() { o.Mem.Release(id) }}, nil
	}
	if err := os.Mkdir(id.Path, 0o755); err != nil { // atomic: a racing creator gets exists
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w: %s", dbstore.ErrExists, id.ID)
		}
		return nil, dbstore.Backend(Engine, err)
	}
	b, err := openBonsai(ctx, id, o)
	if err != nil {
		os.RemoveAll(id.Path)
		return nil, err
	}
	return &DB{id: id, b: b}, nil
}

func (Adapter) Open(ctx context.Context, t dbstore.Target, o dbstore.Options) (dbstore.Handle, error) {
	id, err := resolve(t, o, false)
	if err != nil {
		return nil, err
	}
	if id.Mode == dbstore.Memory {
		var v any
		if err := dbstore.Retry(ctx, o.WaitOrDefault(), func() (err error) {
			v, err = o.Mem.Acquire(id, o.Exclusive)
			return err
		}); err != nil {
			return nil, err
		}
		return &DB{id: id, ro: o.ReadOnly, wait: o.WaitOrDefault(), mem: v.(*dbstore.MemGraph), release: func() { o.Mem.Release(id) }}, nil
	}
	if ok, err := dbstore.Exists(filepath.Join(id.Path, "p")); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("%w: %s (no bonsai database there)", dbstore.ErrNotFound, id.ID)
	}
	b, err := openBonsai(ctx, id, o)
	if err != nil {
		return nil, err
	}
	return &DB{id: id, ro: o.ReadOnly, b: b}, nil
}

// Drop removes an idle database: it takes the writer lock (which a live
// writer elsewhere refuses), releases it, and removes the directory.
func (a Adapter) Drop(ctx context.Context, t dbstore.Target, o dbstore.Options) error {
	id, err := resolve(t, o, false)
	if err != nil {
		return err
	}
	if id.Mode == dbstore.Memory {
		return o.Mem.Drop(id)
	}
	o.ReadOnly = false
	h, err := a.Open(ctx, t, o)
	if err != nil {
		return err
	}
	if err := h.Close(); err != nil {
		return err
	}
	return dbstore.Backend(Engine, os.RemoveAll(id.Path))
}

func resolve(t dbstore.Target, o dbstore.Options, create bool) (dbstore.Identity, error) {
	if t.Engine == "" {
		t.Engine = Engine
	}
	if t.Engine != Engine {
		return dbstore.Identity{}, fmt.Errorf("%w: engine %q is not %s", dbstore.ErrInvalidInput, t.Engine, Engine)
	}
	id, err := dbstore.Resolve(t)
	if err != nil {
		return id, err
	}
	return id, o.Validate(id, create)
}

// openBonsai takes the process slot and Badger's directory lock, each wait
// bounded by o.Wait and observing ctx.
func openBonsai(ctx context.Context, id dbstore.Identity, o dbstore.Options) (b *bonsai.DB, err error) {
	err = dbstore.Retry(ctx, o.WaitOrDefault(), func() error {
		if err := claimProcess(); err != nil {
			return err
		}
		b, err = bonsai.Open(bonsai.Options{Dir: id.Path, ReadOnly: o.ReadOnly})
		if err != nil {
			unclaimProcess()
			if strings.Contains(err.Error(), "Another process is using this Badger database") {
				return fmt.Errorf("%w: %s is locked by another process: %w", dbstore.ErrBusy, id.ID, err)
			}
			return dbstore.Backend(Engine, err)
		}
		return nil
	})
	return b, err
}
