// Package bboltdb is the dbstore adapter for bbolt (go.etcd.io/bbolt, the
// pinned module already in yoke's graph), serving the kv category.
//
// Persistent mode is native. bbolt takes an exclusive flock for a writable
// open and a shared one for a read-only open, so one process writes OR many
// processes read: multi-process writers go through an owner session. bbolt
// has no in-memory mode (it memory-maps a real file), so memory mode is the
// supplemental dbstore.MemKV behind the same dbstore.KV interface — no temp
// file.
package bboltdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	bolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"

	"github.com/qiangli/yoke/pkg/dbstore"
)

// Engine is the dbstore engine name.
const Engine = "bbolt"

// Adapter implements dbstore.Adapter.
type Adapter struct{}

var _ dbstore.Adapter = Adapter{}

func (Adapter) Engine() string { return Engine }

func (Adapter) Caps() dbstore.Caps {
	return dbstore.Summarize(dbstore.Caps{
		Engine:     Engine,
		Categories: []string{"kv", "objects"},
		Modes: map[dbstore.Mode]dbstore.ModeCaps{
			dbstore.Persistent: {
				Supported: true, Via: "native",
				Sharing:   &dbstore.Sharing{NativeMultiprocess: false, OwnerRequiredForMultiprocess: true},
				Exclusion: &dbstore.Exclusion{Scopes: []string{"database"}, EnforcedBy: dbstore.EnforcedByNativeLock, NonBashyClients: dbstore.NonBashyBlocked},
			},
			dbstore.Memory: dbstore.MemoryModeCaps("supplemental-adapter:memkv"),
		},
		Transactions: dbstore.Transactions{AtomicBatch: true, Isolation: "serializable"},
		Notes: []string{
			"a writable open holds an exclusive flock for the handle's life; read-only opens share a flock and exclude writers",
			"writers on one handle are admitted one at a time with a wait bounded by Options.Wait that observes ctx; Bolt() bypasses that admission",
			"a commit that must grow the file waits for read transactions in flight to end; keep reads short",
			"memory mode is the supplemental memkv adapter: copy-on-write snapshots, readers never block, writers serialize",
			"no TTL or expiry: values live until deleted, a ttl request is refused as unsupported; no Redis compatibility",
			"a closed handle rejects every later View and Update with lifetime_ended",
			"objects: db obj buckets and revisioned objects with sha256 checksums; db obj export|import is the object interchange",
		},
	})
}

// DB is an open kv database in either mode.
type DB struct {
	id      dbstore.Identity
	ro      bool
	wait    time.Duration
	bolt    *bolt.DB
	mem     *dbstore.MemKV
	gate    dbstore.Gate // admits this handle's bolt writers, bounded and ctx-aware
	guard   dbstore.Guard
	release func()
}

var _ dbstore.KV = (*DB)(nil)

func (d *DB) Identity() dbstore.Identity { return d.id }
func (d *DB) ReadOnly() bool             { return d.ro }

// Bolt is the native engine handle (nil in memory mode). Writes made through
// it bypass the handle's bounded writer admission.
func (d *DB) Bolt() *bolt.DB { return d.bolt }

// Close releases the handle once, after operations in flight; a memory
// database stays in the MemSpace until Drop.
func (d *DB) Close() error {
	return d.guard.Close(func() error {
		if d.release != nil {
			d.release()
		}
		if d.bolt != nil {
			return dbstore.Backend(Engine, d.bolt.Close())
		}
		return nil
	})
}

func (d *DB) View(ctx context.Context, fn func(dbstore.KVTx) error) error {
	if err := d.guard.Enter(d.id); err != nil {
		return err
	}
	defer d.guard.Exit()
	if d.mem != nil {
		return d.mem.View(ctx, fn)
	}
	if err := ctx.Err(); err != nil {
		return dbstore.FromContext(err)
	}
	return classify(d.bolt.View(func(tx *bolt.Tx) error { return fn(boltTx{tx}) }))
}

// Update runs fn as one atomic transaction. Writer admission waits at most
// the handle's Options.Wait and observes ctx (bbolt's own writer mutex does
// neither); ctx is checked again before commit.
func (d *DB) Update(ctx context.Context, fn func(dbstore.KVTx) error) error {
	if err := d.guard.Enter(d.id); err != nil {
		return err
	}
	defer d.guard.Exit()
	if d.ro {
		return fmt.Errorf("%w: %s opened read-only", dbstore.ErrReadOnly, d.id.ID)
	}
	if d.mem != nil {
		return d.mem.UpdateWithin(ctx, d.wait, fn)
	}
	leave, err := d.gate.Enter(ctx, d.wait)
	if err != nil {
		return err
	}
	defer leave()
	return classify(d.bolt.Update(func(tx *bolt.Tx) error {
		if err := fn(boltTx{tx}); err != nil {
			return err
		}
		return dbstore.FromContext(ctx.Err()) // cancelled before commit: roll back
	}))
}

func (Adapter) Create(ctx context.Context, t dbstore.Target, o dbstore.Options) (dbstore.Handle, error) {
	id, err := resolve(t, o, true)
	if err != nil {
		return nil, err
	}
	if id.Mode == dbstore.Memory {
		m := dbstore.NewMemKV()
		if err := o.Mem.Put(id, m, o.Exclusive); err != nil {
			return nil, err
		}
		return &DB{id: id, wait: o.WaitOrDefault(), mem: m, release: func() { o.Mem.Release(id) }}, nil
	}
	b, err := openBolt(ctx, id, o, os.O_EXCL)
	if err != nil {
		return nil, err
	}
	return &DB{id: id, wait: o.WaitOrDefault(), bolt: b, gate: dbstore.NewGate()}, nil
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
		return &DB{id: id, ro: o.ReadOnly, wait: o.WaitOrDefault(), mem: v.(*dbstore.MemKV), release: func() { o.Mem.Release(id) }}, nil
	}
	b, err := openBolt(ctx, id, o, 0)
	if err != nil {
		return nil, err
	}
	return &DB{id: id, ro: o.ReadOnly, wait: o.WaitOrDefault(), bolt: b, gate: dbstore.NewGate()}, nil
}

// Drop removes an idle database. The persistent file is removed under the
// exclusive flock (released first on Windows, where an open file cannot be
// deleted); a live handle elsewhere makes the flock wait fail with busy.
func (a Adapter) Drop(ctx context.Context, t dbstore.Target, o dbstore.Options) error {
	id, err := resolve(t, o, false)
	if err != nil {
		return err
	}
	if id.Mode == dbstore.Memory {
		return o.Mem.Drop(id)
	}
	o.ReadOnly = false
	b, err := openBolt(ctx, id, o, 0)
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		b.Close()
		return dbstore.Backend(Engine, os.Remove(id.Path))
	}
	err = os.Remove(id.Path)
	return dbstore.Backend(Engine, errors.Join(err, b.Close()))
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

// openBolt opens the file without ever creating it, except that Create
// passes O_EXCL so creation is race-free. The flock wait is bounded by
// o.Wait and observes ctx: bbolt's own wait cannot be cancelled, so each
// attempt waits one short slice and dbstore.Retry loops.
func openBolt(ctx context.Context, id dbstore.Identity, o dbstore.Options, excl int) (b *bolt.DB, err error) {
	err = dbstore.Retry(ctx, o.WaitOrDefault(), func() error {
		b, err = openBoltOnce(id, o, excl)
		return err
	})
	return b, err
}

func openBoltOnce(id dbstore.Identity, o dbstore.Options, excl int) (*bolt.DB, error) {
	b, err := bolt.Open(id.Path, 0o600, &bolt.Options{
		Timeout:  50 * time.Millisecond,
		ReadOnly: o.ReadOnly,
		OpenFile: func(name string, flag int, perm os.FileMode) (*os.File, error) {
			if excl == 0 {
				flag &^= os.O_CREATE
			} else {
				flag |= os.O_CREATE | excl
			}
			return os.OpenFile(name, flag, perm)
		},
	})
	switch {
	case err == nil:
		return b, nil
	case errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("%w: %s", dbstore.ErrNotFound, id.ID)
	case errors.Is(err, os.ErrExist):
		return nil, fmt.Errorf("%w: %s", dbstore.ErrExists, id.ID)
	case errors.Is(err, berrors.ErrTimeout):
		return nil, fmt.Errorf("%w: %s is locked by another handle: %w", dbstore.ErrBusy, id.ID, err)
	}
	return nil, dbstore.Backend(Engine, err)
}

func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, berrors.ErrDatabaseReadOnly), errors.Is(err, berrors.ErrTxNotWritable):
		return fmt.Errorf("%w: %w", dbstore.ErrReadOnly, err)
	}
	return dbstore.Backend(Engine, err)
}

type boltTx struct{ tx *bolt.Tx }

func (t boltTx) Writable() bool { return t.tx.Writable() }

func (t boltTx) bucket(name []byte) (*bolt.Bucket, error) {
	b := t.tx.Bucket(name)
	if b == nil {
		return nil, fmt.Errorf("%w: bucket %q", dbstore.ErrNotFound, name)
	}
	return b, nil
}

func (t boltTx) CreateBucket(name []byte) error {
	if len(name) == 0 {
		return fmt.Errorf("%w: empty bucket name", dbstore.ErrInvalidInput)
	}
	_, err := t.tx.CreateBucket(name)
	if errors.Is(err, berrors.ErrBucketExists) {
		return fmt.Errorf("%w: bucket %q", dbstore.ErrExists, name)
	}
	return classify(err)
}

func (t boltTx) DeleteBucket(name []byte) error {
	err := t.tx.DeleteBucket(name)
	if errors.Is(err, berrors.ErrBucketNotFound) {
		return fmt.Errorf("%w: bucket %q", dbstore.ErrNotFound, name)
	}
	return classify(err)
}

func (t boltTx) Buckets() ([][]byte, error) {
	var out [][]byte
	err := t.tx.ForEach(func(name []byte, _ *bolt.Bucket) error {
		out = append(out, bytes.Clone(name))
		return nil
	})
	return out, classify(err)
}

func (t boltTx) Get(bucket, key []byte) ([]byte, bool, error) {
	b, err := t.bucket(bucket)
	if err != nil {
		return nil, false, err
	}
	v := b.Get(key)
	if v == nil {
		return nil, false, nil
	}
	return bytes.Clone(v), true, nil
}

func (t boltTx) Put(bucket, key, value []byte) error {
	if len(key) == 0 {
		return fmt.Errorf("%w: empty key", dbstore.ErrInvalidInput)
	}
	b, err := t.bucket(bucket)
	if err != nil {
		return err
	}
	if value == nil {
		value = []byte{}
	}
	return classify(b.Put(key, value))
}

func (t boltTx) Delete(bucket, key []byte) error {
	b, err := t.bucket(bucket)
	if err != nil {
		return err
	}
	return classify(b.Delete(key))
}

func (t boltTx) Scan(bucket, start []byte, fn func(k, v []byte) (bool, error)) error {
	b, err := t.bucket(bucket)
	if err != nil {
		return err
	}
	c := b.Cursor()
	for k, v := c.Seek(start); k != nil; k, v = c.Next() {
		if v == nil {
			continue // nested bucket: not part of the flat kv surface
		}
		more, err := fn(bytes.Clone(k), bytes.Clone(v))
		if err != nil || !more {
			return err
		}
	}
	return nil
}
