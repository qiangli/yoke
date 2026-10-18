package dbstore

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"time"
)

// KV is the key-value surface shared by the bbolt adapter (persistent) and
// MemKV (its in-memory supplement), so the kv category behaves the same in
// both modes. Buckets are one level deep.
//
// View runs fn against a consistent snapshot; any number run concurrently
// with each other and with a writer. Update runs fn as one serializable,
// atomic transaction: writers serialize, and an error, a panic or a cancelled
// ctx (checked before fn and again before commit) rolls back everything fn
// did. Read-modify-write (compare-and-set, increments) is therefore safe
// inside one Update.
type KV interface {
	View(ctx context.Context, fn func(KVTx) error) error
	Update(ctx context.Context, fn func(KVTx) error) error
}

// KVTx is one transaction. Returned slices are copies the caller owns.
// Missing buckets are ErrNotFound; a missing key is (nil, false, nil).
type KVTx interface {
	CreateBucket(name []byte) error
	DeleteBucket(name []byte) error
	Buckets() ([][]byte, error)
	Get(bucket, key []byte) ([]byte, bool, error)
	Put(bucket, key, value []byte) error
	Delete(bucket, key []byte) error
	// Scan visits keys >= start in ascending order until fn returns false.
	Scan(bucket, start []byte, fn func(k, v []byte) (bool, error)) error
	Writable() bool
}

// MemKV is the supplemental in-memory KV adapter (bbolt has no in-memory
// mode: it maps a real file). Every Update commits a new immutable snapshot
// — copy-on-write per touched bucket — so readers never block and never see
// a partial transaction. Writers serialize through a Gate, so a writer's wait
// for admission is bounded and observes ctx. It keeps nothing on disk.
type MemKV struct {
	gate Gate
	snap atomic.Pointer[map[string]map[string][]byte]
}

// NewMemKV returns an empty in-memory KV.
func NewMemKV() *MemKV {
	m := &MemKV{gate: NewGate()}
	empty := map[string]map[string][]byte{}
	m.snap.Store(&empty)
	return m
}

// Close frees the contents.
func (m *MemKV) Close() error {
	empty := map[string]map[string][]byte{}
	m.snap.Store(&empty)
	return nil
}

func (m *MemKV) View(ctx context.Context, fn func(KVTx) error) error {
	if err := ctx.Err(); err != nil {
		return FromContext(err)
	}
	return fn(&memTx{base: *m.snap.Load()})
}

// Update waits at most DefaultWait for writer admission; UpdateWithin takes
// the handle's bound.
func (m *MemKV) Update(ctx context.Context, fn func(KVTx) error) error {
	return m.UpdateWithin(ctx, DefaultWait, fn)
}

// UpdateWithin is Update with an explicit admission wait. An unadmitted
// writer fails with ErrBusy (wait elapsed) or timeout/cancelled (ctx ended).
func (m *MemKV) UpdateWithin(ctx context.Context, wait time.Duration, fn func(KVTx) error) error {
	leave, err := m.gate.Enter(ctx, wait)
	if err != nil {
		return err
	}
	defer leave()
	tx := &memTx{base: *m.snap.Load(), write: true, dirty: map[string]map[string][]byte{}, dropped: map[string]bool{}}
	if err := fn(tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return FromContext(err)
	}
	next := make(map[string]map[string][]byte, len(tx.base)+len(tx.dirty))
	for b, kv := range tx.base {
		if !tx.dropped[b] {
			next[b] = kv
		}
	}
	for b, kv := range tx.dirty {
		next[b] = kv
	}
	m.snap.Store(&next)
	return nil
}

// memTx reads through dirty (buckets this transaction copied or created)
// to base (the snapshot it started from); dropped hides base buckets.
type memTx struct {
	base    map[string]map[string][]byte
	write   bool
	dirty   map[string]map[string][]byte
	dropped map[string]bool
}

func (t *memTx) Writable() bool { return t.write }

func (t *memTx) bucket(name []byte) (map[string][]byte, bool) {
	if b, ok := t.dirty[string(name)]; ok {
		return b, true
	}
	if t.dropped[string(name)] {
		return nil, false
	}
	b, ok := t.base[string(name)]
	return b, ok
}

func (t *memTx) mut(name []byte) (map[string][]byte, error) {
	if !t.write {
		return nil, fmt.Errorf("%w: write in a read transaction", ErrReadOnly)
	}
	if b, ok := t.dirty[string(name)]; ok {
		return b, nil
	}
	b, ok := t.bucket(name)
	if !ok {
		return nil, fmt.Errorf("%w: bucket %q", ErrNotFound, name)
	}
	c := make(map[string][]byte, len(b))
	for k, v := range b {
		c[k] = v
	}
	t.dirty[string(name)] = c
	return c, nil
}

func (t *memTx) CreateBucket(name []byte) error {
	if !t.write {
		return fmt.Errorf("%w: write in a read transaction", ErrReadOnly)
	}
	if len(name) == 0 {
		return fmt.Errorf("%w: empty bucket name", ErrInvalidInput)
	}
	if _, ok := t.bucket(name); ok {
		return fmt.Errorf("%w: bucket %q", ErrExists, name)
	}
	t.dirty[string(name)] = map[string][]byte{}
	return nil
}

func (t *memTx) DeleteBucket(name []byte) error {
	if !t.write {
		return fmt.Errorf("%w: write in a read transaction", ErrReadOnly)
	}
	if _, ok := t.bucket(name); !ok {
		return fmt.Errorf("%w: bucket %q", ErrNotFound, name)
	}
	delete(t.dirty, string(name))
	t.dropped[string(name)] = true
	return nil
}

func (t *memTx) Buckets() ([][]byte, error) {
	seen := map[string]bool{}
	for b := range t.base {
		if !t.dropped[b] {
			seen[b] = true
		}
	}
	for b := range t.dirty {
		seen[b] = true
	}
	names := make([]string, 0, len(seen))
	for b := range seen {
		names = append(names, b)
	}
	sort.Strings(names)
	out := make([][]byte, len(names))
	for i, n := range names {
		out[i] = []byte(n)
	}
	return out, nil
}

func (t *memTx) Get(bucket, key []byte) ([]byte, bool, error) {
	b, ok := t.bucket(bucket)
	if !ok {
		return nil, false, fmt.Errorf("%w: bucket %q", ErrNotFound, bucket)
	}
	v, ok := b[string(key)]
	if !ok {
		return nil, false, nil
	}
	return bytes.Clone(v), true, nil
}

func (t *memTx) Put(bucket, key, value []byte) error {
	if len(key) == 0 {
		return fmt.Errorf("%w: empty key", ErrInvalidInput)
	}
	b, err := t.mut(bucket)
	if err != nil {
		return err
	}
	if value == nil {
		value = []byte{}
	}
	b[string(key)] = bytes.Clone(value)
	return nil
}

func (t *memTx) Delete(bucket, key []byte) error {
	b, err := t.mut(bucket)
	if err != nil {
		return err
	}
	delete(b, string(key))
	return nil
}

func (t *memTx) Scan(bucket, start []byte, fn func(k, v []byte) (bool, error)) error {
	b, ok := t.bucket(bucket)
	if !ok {
		return fmt.Errorf("%w: bucket %q", ErrNotFound, bucket)
	}
	keys := make([]string, 0, len(b))
	for k := range b {
		if k >= string(start) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		more, err := fn([]byte(k), bytes.Clone(b[k]))
		if err != nil || !more {
			return err
		}
	}
	return nil
}
