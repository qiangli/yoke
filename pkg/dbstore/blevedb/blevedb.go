// Package blevedb is the dbstore adapter for Bleve (the pinned
// github.com/blevesearch/bleve/v2 module ycode's memex already uses), serving
// the fts category. The engine is reused as is; this package only enforces
// the dbstore open/create/drop contract around it.
//
// Both modes are native: a persistent index is a scorch directory, a memory
// index is bleve.NewMemOnly (upsidedown over the in-process gtreap store — no
// files). A writable persistent open holds scorch's root.bolt flock for the
// handle's life, so one process writes or several read; multi-process writers
// go through an owner session.
package blevedb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	index "github.com/blevesearch/bleve_index_api"
	berrors "go.etcd.io/bbolt/errors"

	"github.com/qiangli/yoke/pkg/dbstore"
)

// Engine is the dbstore engine name.
const Engine = "bleve"

// Adapter implements dbstore.Adapter. Mapping is used by Create; nil means
// bleve's default mapping. Open always uses the mapping stored in the index.
type Adapter struct {
	Mapping mapping.IndexMapping
}

var _ dbstore.Adapter = Adapter{}

func (Adapter) Engine() string { return Engine }

func (Adapter) Caps() dbstore.Caps {
	return dbstore.Summarize(dbstore.Caps{
		Engine:     Engine,
		Categories: []string{"fts"},
		Modes: map[dbstore.Mode]dbstore.ModeCaps{
			dbstore.Persistent: {
				Supported: true, Via: "native",
				Sharing:   &dbstore.Sharing{NativeMultiprocess: false, OwnerRequiredForMultiprocess: true},
				Exclusion: &dbstore.Exclusion{Scopes: []string{"database"}, EnforcedBy: dbstore.EnforcedByNativeLock, NonBashyClients: dbstore.NonBashyBlocked},
			},
			dbstore.Memory: dbstore.MemoryModeCaps("native"),
		},
		Transactions: dbstore.Transactions{AtomicBatch: true, Isolation: "snapshot"},
		Notes: []string{
			"a writable open holds scorch's root.bolt flock; read-only opens share it and exclude writers",
			"scorch accepts writes on a read-only open without persisting them, so read-only is enforced by this handle",
			"persistent drop releases the flock before removing the directory; the lifecycle db hold covers that window",
			"index writes are admitted one at a time per index with a wait bounded by Options.Wait; bleve's API takes no ctx, so a write in progress is not cancellable",
			"a closed handle, and every Index() it returned, rejects later use with lifetime_ended",
		},
	})
}

// memIndex is what a MemSpace holds for a memory index: the index and the
// writer gate every handle on it shares.
type memIndex struct {
	bleve.Index
	gate dbstore.Gate
}

// DB is an open index in either mode.
type DB struct {
	id      dbstore.Identity
	ro      bool
	idx     bleve.Index
	wait    time.Duration
	gate    dbstore.Gate // one per index: a memory index shares it across handles
	guard   dbstore.Guard
	release func()
	own     bool // the handle closes idx (persistent); memory indexes belong to the MemSpace
}

func (d *DB) Identity() dbstore.Identity { return d.id }
func (d *DB) ReadOnly() bool             { return d.ro }

// Index is the engine handle for the fts operations. Its Close is refused —
// the dbstore handle owns the lifetime, and a memory index is shared by every
// handle in the owner. After the handle closes, every error-returning method
// fails with dbstore.ErrLifetimeEnded; on a read-only handle every mutator
// returns dbstore.ErrReadOnly.
func (d *DB) Index() bleve.Index { return guarded{d} }

// Close releases the handle once, after index calls in flight.
func (d *DB) Close() error {
	return d.guard.Close(func() error {
		if d.release != nil {
			d.release()
		}
		if d.own {
			return dbstore.Backend(Engine, d.idx.Close())
		}
		return nil
	})
}

func (a Adapter) Create(ctx context.Context, t dbstore.Target, o dbstore.Options) (dbstore.Handle, error) {
	id, err := resolve(t, o, true)
	if err != nil {
		return nil, err
	}
	m := a.Mapping
	if m == nil {
		m = bleve.NewIndexMapping()
	}
	if id.Mode == dbstore.Memory {
		idx, err := bleve.NewMemOnly(m)
		if err != nil {
			return nil, dbstore.Backend(Engine, err)
		}
		mi := &memIndex{Index: idx, gate: dbstore.NewGate()}
		if err := o.Mem.Put(id, mi, o.Exclusive); err != nil {
			idx.Close()
			return nil, err
		}
		return &DB{id: id, wait: o.WaitOrDefault(), idx: idx, gate: mi.gate, release: func() { o.Mem.Release(id) }}, nil
	}
	if ok, err := dbstore.Exists(id.Path); err != nil {
		return nil, err
	} else if ok {
		return nil, fmt.Errorf("%w: %s", dbstore.ErrExists, id.ID)
	}
	idx, err := bleve.New(id.Path, m) // index_meta.json is written O_EXCL: racing creators lose with exists
	if err != nil {
		return nil, classify(id, err)
	}
	return &DB{id: id, wait: o.WaitOrDefault(), idx: idx, gate: dbstore.NewGate(), own: true}, nil
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
		mi := v.(*memIndex)
		return &DB{id: id, ro: o.ReadOnly, wait: o.WaitOrDefault(), idx: mi.Index, gate: mi.gate, release: func() { o.Mem.Release(id) }}, nil
	}
	idx, err := openIndex(ctx, id, o)
	if err != nil {
		return nil, err
	}
	return &DB{id: id, ro: o.ReadOnly, wait: o.WaitOrDefault(), idx: idx, gate: dbstore.NewGate(), own: true}, nil
}

func (Adapter) Drop(ctx context.Context, t dbstore.Target, o dbstore.Options) error {
	id, err := resolve(t, o, false)
	if err != nil {
		return err
	}
	if id.Mode == dbstore.Memory {
		return o.Mem.Drop(id)
	}
	o.ReadOnly = false
	idx, err := openIndex(ctx, id, o)
	if err != nil {
		return err
	}
	if err := idx.Close(); err != nil {
		return dbstore.Backend(Engine, err)
	}
	return dbstore.Backend(Engine, os.RemoveAll(id.Path))
}

// openIndex opens an existing index (bleve.Open never creates). scorch's
// flock wait cannot be cancelled, so each attempt waits one short slice and
// dbstore.Retry bounds the total and observes ctx.
func openIndex(ctx context.Context, id dbstore.Identity, o dbstore.Options) (idx bleve.Index, err error) {
	cfg := map[string]interface{}{"bolt_timeout": "50ms"}
	if o.ReadOnly {
		cfg["read_only"] = true
	}
	err = dbstore.Retry(ctx, o.WaitOrDefault(), func() error {
		idx, err = bleve.OpenUsing(id.Path, cfg)
		return classify(id, err)
	})
	return idx, err
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

func classify(id dbstore.Identity, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, bleve.ErrorIndexPathDoesNotExist), errors.Is(err, bleve.ErrorIndexMetaMissing):
		return fmt.Errorf("%w: %s", dbstore.ErrNotFound, id.ID)
	case errors.Is(err, bleve.ErrorIndexPathExists):
		return fmt.Errorf("%w: %s", dbstore.ErrExists, id.ID)
	case errors.Is(err, berrors.ErrTimeout):
		return fmt.Errorf("%w: %s is locked by another handle: %w", dbstore.ErrBusy, id.ID, err)
	}
	return dbstore.Backend(Engine, err)
}

// guarded is the bleve.Index a DB hands out: each call is admitted by the
// handle's guard, so a closed handle cannot reach a shared memory index, and
// mutators are refused on a read-only handle.
type guarded struct{ d *DB }

var _ bleve.Index = guarded{}

func (g guarded) enter(write bool) error {
	if err := g.d.guard.Enter(g.d.id); err != nil {
		return err
	}
	if write && g.d.ro {
		g.d.guard.Exit()
		return fmt.Errorf("%w: index opened read-only", dbstore.ErrReadOnly)
	}
	return nil
}

// write runs one mutation: admitted by the guard, then by the index's writer
// gate with the handle's bounded wait (bleve's API takes no ctx, and its own
// writer serialization has no wait bound).
func (g guarded) write(fn func() error) error {
	if err := g.enter(true); err != nil {
		return err
	}
	defer g.d.guard.Exit()
	leave, err := g.d.gate.Enter(context.Background(), g.d.wait)
	if err != nil {
		return err
	}
	defer leave()
	return fn()
}

func (g guarded) Index(id string, data interface{}) error {
	return g.write(func() error { return g.d.idx.Index(id, data) })
}

func (g guarded) Delete(id string) error {
	return g.write(func() error { return g.d.idx.Delete(id) })
}

func (g guarded) NewBatch() *bleve.Batch { return g.d.idx.NewBatch() }

func (g guarded) Batch(b *bleve.Batch) error {
	return g.write(func() error { return g.d.idx.Batch(b) })
}

func (g guarded) Document(id string) (index.Document, error) {
	if err := g.enter(false); err != nil {
		return nil, err
	}
	defer g.d.guard.Exit()
	return g.d.idx.Document(id)
}

func (g guarded) DocCount() (uint64, error) {
	if err := g.enter(false); err != nil {
		return 0, err
	}
	defer g.d.guard.Exit()
	return g.d.idx.DocCount()
}

func (g guarded) Search(req *bleve.SearchRequest) (*bleve.SearchResult, error) {
	return g.SearchInContext(context.Background(), req)
}

func (g guarded) SearchInContext(ctx context.Context, req *bleve.SearchRequest) (*bleve.SearchResult, error) {
	if err := g.enter(false); err != nil {
		return nil, err
	}
	defer g.d.guard.Exit()
	return g.d.idx.SearchInContext(ctx, req)
}

func (g guarded) Fields() ([]string, error) {
	if err := g.enter(false); err != nil {
		return nil, err
	}
	defer g.d.guard.Exit()
	return g.d.idx.Fields()
}

func (g guarded) FieldDict(field string) (index.FieldDict, error) {
	if err := g.enter(false); err != nil {
		return nil, err
	}
	defer g.d.guard.Exit()
	return g.d.idx.FieldDict(field)
}

func (g guarded) FieldDictRange(field string, start, end []byte) (index.FieldDict, error) {
	if err := g.enter(false); err != nil {
		return nil, err
	}
	defer g.d.guard.Exit()
	return g.d.idx.FieldDictRange(field, start, end)
}

func (g guarded) FieldDictPrefix(field string, prefix []byte) (index.FieldDict, error) {
	if err := g.enter(false); err != nil {
		return nil, err
	}
	defer g.d.guard.Exit()
	return g.d.idx.FieldDictPrefix(field, prefix)
}

func (guarded) Close() error {
	return fmt.Errorf("%w: close the dbstore handle, not the index", dbstore.ErrInvalidInput)
}

func (g guarded) Mapping() mapping.IndexMapping    { return g.d.idx.Mapping() }
func (g guarded) Stats() *bleve.IndexStat          { return g.d.idx.Stats() }
func (g guarded) StatsMap() map[string]interface{} { return g.d.idx.StatsMap() }
func (g guarded) Name() string                     { return g.d.idx.Name() }
func (g guarded) SetName(n string) {
	if g.enter(true) == nil {
		defer g.d.guard.Exit()
		g.d.idx.SetName(n)
	}
}

func (g guarded) GetInternal(key []byte) ([]byte, error) {
	if err := g.enter(false); err != nil {
		return nil, err
	}
	defer g.d.guard.Exit()
	return g.d.idx.GetInternal(key)
}

func (g guarded) SetInternal(key, val []byte) error {
	return g.write(func() error { return g.d.idx.SetInternal(key, val) })
}

func (g guarded) DeleteInternal(key []byte) error {
	return g.write(func() error { return g.d.idx.DeleteInternal(key) })
}

// Advanced exposes the raw index, which no guard can cover, so it is refused
// on a read-only handle and after Close.
func (g guarded) Advanced() (index.Index, error) {
	if err := g.enter(true); err != nil {
		return nil, err
	}
	defer g.d.guard.Exit()
	return g.d.idx.Advanced()
}
