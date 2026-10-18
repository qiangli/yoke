package events

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"

	"github.com/qiangli/yoke/external/gws"
)

// CheckpointSchema is the schema_version of the checkpoint file.
const CheckpointSchema = "gws-events-checkpoint-v1"

// DefaultSeenWindow bounds the dedup window (most recent acknowledged ids).
// Duplicates older than the window are not detected: dedup is best-effort
// within a bounded horizon, which is why Handlers stay idempotent.
const DefaultSeenWindow = 1024

// Lease kinds renewed by Renewer.
const (
	// LeaseGmailWatch is a Gmail users.watch registration (expires within
	// 7 days; Google recommends re-watching daily).
	LeaseGmailWatch = "gmail-watch"
	// LeaseEventsSubscription is a Workspace Events subscription (expireTime;
	// renewed by patching its ttl before it lapses).
	LeaseEventsSubscription = "events-subscription"
)

// Lease is a provider registration that lapses unless renewed.
type Lease struct {
	Kind    string    `json:"kind"`
	Name    string    `json:"name"`
	Expires time.Time `json:"expires"`
}

// Checkpoint is the persisted consumer position for one binding.
type Checkpoint struct {
	SchemaVersion string `json:"schema_version"`
	Target        string `json:"target"`
	Query         string `json:"query,omitempty"`
	Revision      string `json:"revision,omitempty"`
	// Cursor is the opaque provider change cursor that closes the last fully
	// acknowledged batch. Never a credential.
	Cursor string `json:"cursor"`
	// Seen is the bounded dedup window of acknowledged event ids, oldest first.
	Seen []string `json:"seen,omitempty"`
	// Leases are the subscription registrations this consumer renews.
	Leases []Lease `json:"leases,omitempty"`
	// Resyncs counts explicit operator resyncs; LastResync records why.
	Resyncs    int       `json:"resyncs,omitempty"`
	LastResync string    `json:"last_resync,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Store is the opt-in durable checkpoint for one binding, held by exactly
// one consumer at a time (a kernel lock beside the file). Every mutation is
// written atomically (temp file + fsync + rename) before it returns.
type Store struct {
	mu     sync.Mutex
	path   string
	lock   *lockfile.Lock
	cp     Checkpoint
	seen   map[string]bool
	window int
}

// Open opens (or creates) the checkpoint for b under dir. dir must be given
// explicitly — there is no default location, which is what makes durable
// state opt-in. A checkpoint written for a different binding fails with a
// typed cursor-mismatch; a second consumer of the same binding is refused.
func Open(dir string, b gws.Binding) (*Store, error) {
	if dir == "" {
		return nil, gws.NewError(gws.CodeInvalidInput, "gws events: durable state is opt-in — pass an explicit checkpoint directory", nil)
	}
	if b.Target == "" {
		return nil, gws.NewError(gws.CodeInvalidInput, "gws events: checkpoint binding needs an explicit target", nil)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(b.Target + "\x00" + b.Query + "\x00" + b.Revision))
	path := filepath.Join(dir, "gws-events-"+hex.EncodeToString(sum[:])[:16]+".json")
	lock, err := lockfile.TryAcquire(path+".lock", lockfile.Holder{Name: "gws-events", Intent: "checkpoint " + b.Target})
	if err != nil {
		if errors.Is(err, lockfile.ErrHeld) {
			return nil, gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("gws events: another consumer holds the checkpoint for target=%q", b.Target), nil)
		}
		return nil, err
	}
	s := &Store{path: path, lock: lock, window: DefaultSeenWindow}
	if err := s.load(b); err != nil {
		_ = lock.Release()
		return nil, err
	}
	return s, nil
}

func (s *Store) load(b gws.Binding) error {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.cp = Checkpoint{SchemaVersion: CheckpointSchema, Target: b.Target, Query: b.Query, Revision: b.Revision}
		s.seen = map[string]bool{}
		return nil
	}
	if err != nil {
		return err
	}
	var cp Checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return gws.NewError(gws.CodeInvalidInput, "gws events: checkpoint is unreadable — refusing to reset it silently", nil)
	}
	if cp.SchemaVersion != CheckpointSchema {
		return gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("gws events: unsupported checkpoint schema_version %q", cp.SchemaVersion), nil)
	}
	if cp.Target != b.Target || cp.Query != b.Query || cp.Revision != b.Revision {
		return gws.NewError(gws.CodeCursorMismatch, fmt.Sprintf("gws events: checkpoint is bound to target=%q query=%q revision=%q — resync explicitly", cp.Target, cp.Query, cp.Revision), nil)
	}
	s.cp = cp
	s.seen = make(map[string]bool, len(cp.Seen))
	for _, id := range cp.Seen {
		s.seen[id] = true
	}
	return nil
}

// Close releases the consumer claim. The checkpoint stays on disk.
func (s *Store) Close() error { return s.lock.Release() }

// Path is the checkpoint file location.
func (s *Store) Path() string { return s.path }

// Checkpoint returns a copy of the current persisted state.
func (s *Store) Checkpoint() Checkpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := s.cp
	cp.Seen = append([]string(nil), s.cp.Seen...)
	cp.Leases = append([]Lease(nil), s.cp.Leases...)
	return cp
}

// Seen reports whether id is inside the dedup window.
func (s *Store) Seen(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[id]
}

// Ack records id as processed and persists before returning.
func (s *Store) Ack(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[id] {
		return nil
	}
	s.cp.Seen = append(s.cp.Seen, id)
	s.seen[id] = true
	if over := len(s.cp.Seen) - s.window; over > 0 {
		for _, old := range s.cp.Seen[:over] {
			delete(s.seen, old)
		}
		s.cp.Seen = append([]string(nil), s.cp.Seen[over:]...)
	}
	return s.saveLocked()
}

// Advance moves the change cursor to next and persists. Callers advance only
// after every event of the closing batch was acknowledged. An empty next is
// refused: a cursor is never reset implicitly.
func (s *Store) Advance(next string) error {
	if next == "" {
		return gws.NewError(gws.CodeProviderFailure, "gws events: provider returned no change cursor — checkpoint left unchanged", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cp.Cursor = next
	return s.saveLocked()
}

// Resync is the explicit operator choice after an expired or mismatched
// cursor: it sets a fresh starting cursor (e.g. a historyId from a new
// watch or a full sync) and records why. The dedup window is kept so a
// replayed overlap is still suppressed.
func (s *Store) Resync(cursor, reason string) error {
	if cursor == "" || reason == "" {
		return gws.NewError(gws.CodeInvalidInput, "gws events: resync needs an explicit cursor and reason", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cp.Cursor = cursor
	s.cp.Resyncs++
	s.cp.LastResync = reason
	return s.saveLocked()
}

// PutLease records (or replaces, by kind+name) a lease and persists.
func (s *Store) PutLease(l Lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, old := range s.cp.Leases {
		if old.Kind == l.Kind && old.Name == l.Name {
			s.cp.Leases[i] = l
			return s.saveLocked()
		}
	}
	s.cp.Leases = append(s.cp.Leases, l)
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	s.cp.UpdatedAt = time.Now().UTC()
	raw, err := json.MarshalIndent(s.cp, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".gws-events-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.path)
}
