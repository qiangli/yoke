package dbstore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Lifetimes an in-memory database can declare (§3.3).
const (
	LifetimeInvocation = "invocation"
	LifetimeMCPSession = "mcp-session"
	LifetimeOwner      = "owner"
)

// MemSpace holds the in-memory databases of one owner: a CLI invocation, an
// MCP session, or an explicitly started owner process. Contents live only in
// this process's memory — nothing is written to disk — and end with Close.
// Sharing across processes goes through the owner that holds the MemSpace;
// two processes never share data through their own MemSpaces.
//
// The session ID is minted here, random and never reused, so a request naming
// an ended session gets ErrLifetimeEnded, never a fresh empty database.
type MemSpace struct {
	id       string
	lifetime string

	mu     sync.Mutex
	closed bool
	dbs    map[string]*memEntry
}

type memEntry struct {
	v         io.Closer
	refs      int
	exclusive bool
}

// NewMemSpace mints a session with the declared lifetime.
func NewMemSpace(lifetime string) (*MemSpace, error) {
	switch lifetime {
	case LifetimeInvocation, LifetimeMCPSession, LifetimeOwner:
	default:
		return nil, fmt.Errorf("%w: lifetime %q", ErrInvalidInput, lifetime)
	}
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, fmt.Errorf("%w: mint session id: %v", ErrBackend, err)
	}
	return &MemSpace{id: "s-" + hex.EncodeToString(b[:]), lifetime: lifetime, dbs: map[string]*memEntry{}}, nil
}

func (s *MemSpace) ID() string       { return s.id }
func (s *MemSpace) Lifetime() string { return s.lifetime }

func (s *MemSpace) check(session string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("%w: session %s", ErrLifetimeEnded, s.id)
	}
	if session != s.id {
		return fmt.Errorf("%w: session %s is not held by this owner (%s)", ErrInvalidInput, session, s.id)
	}
	return nil
}

func key(id Identity) string { return id.Engine + "\x00" + id.Name }

// Put registers a newly created database and takes the first reference.
func (s *MemSpace) Put(id Identity, v io.Closer, exclusive bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("%w: session %s", ErrLifetimeEnded, s.id)
	}
	if _, ok := s.dbs[key(id)]; ok {
		return fmt.Errorf("%w: %s", ErrExists, id.ID)
	}
	s.dbs[key(id)] = &memEntry{v: v, refs: 1, exclusive: exclusive}
	return nil
}

// Acquire takes a reference to an existing database. An exclusive acquire
// needs no other reference and blocks every later one until released; both
// fail with ErrBusy (callers bound the wait with Retry).
func (s *MemSpace) Acquire(id Identity, exclusive bool) (io.Closer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("%w: session %s", ErrLifetimeEnded, s.id)
	}
	e, ok := s.dbs[key(id)]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id.ID)
	}
	if e.exclusive || (exclusive && e.refs > 0) {
		return nil, fmt.Errorf("%w: %s is held exclusively or in use", ErrBusy, id.ID)
	}
	e.refs++
	e.exclusive = exclusive
	return e.v, nil
}

// Release drops one reference. The database stays until Drop or Close: one
// client closing its handle never destroys another client's database.
func (s *MemSpace) Release(id Identity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.dbs[key(id)]; ok && e.refs > 0 {
		e.refs--
		e.exclusive = false
	}
}

// Drop removes an idle database and frees it. A database with open handles
// fails with ErrBusy.
func (s *MemSpace) Drop(id Identity) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("%w: session %s", ErrLifetimeEnded, s.id)
	}
	e, ok := s.dbs[key(id)]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, id.ID)
	}
	if e.refs > 0 {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s has %d open handle(s)", ErrBusy, id.ID, e.refs)
	}
	delete(s.dbs, key(id))
	s.mu.Unlock()
	return e.v.Close()
}

// Names lists the databases of one engine, sorted.
func (s *MemSpace) Names(engine string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.dbs {
		if e, n, ok := strings.Cut(k, "\x00"); ok && e == engine {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Close ends the lifetime: every database is freed, and every later request
// gets ErrLifetimeEnded.
func (s *MemSpace) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	dbs := s.dbs
	s.dbs = nil
	s.mu.Unlock()
	var errs []error
	for _, e := range dbs {
		errs = append(errs, e.v.Close())
	}
	return errors.Join(errs...)
}
