package craft

// FACTS — the PARTICULAR half of what a host learns.
//
// Knowledge an agent picks up splits two ways, and the split is the whole
// design rather than a filing convention:
//
//	GENERALISABLE  "mDNS is unreliable on Windows, resolve by IP instead"
//	               keyed on a space-time COORDINATE, contains no identity by
//	               construction, freely shareable — a FOLD.
//	PARTICULAR     "on that box the login is svc-build, and it answers at .41"
//	               bound to an ENTITY, true nowhere else, and identity-laden —
//	               a FACT. This file.
//
// Both are needed and confusing them is how a shared catalog fills with claims
// true on exactly one machine.
//
// # Local ring only, and that is structural
//
// A fact is by definition a statement about someone's machine: a hostname, a
// login, an address. It is exactly the material the OSS hygiene rule forbids in
// anything shared, so facts NEVER leave the host. Not scrubbed-then-shared —
// not shared. Values are stored raw because a fact whose value is redacted is
// useless (the point of remembering the login is to use it), which is precisely
// why the boundary has to hold at the store rather than at the reader.
//
// # Append-only, bi-temporal, and never edited
//
// Invalidating a fact appends a new record; nothing is rewritten. That is the
// same supersede-not-delete discipline the kb uses, and it buys the thing an
// edit-in-place store cannot: you can ask what this host BELIEVED last Tuesday,
// which is the only way to explain why an agent did what it did.
//
// # The failure this file exists to prevent
//
// A learned address goes stale silently. The host moves, the fact stays, and
// the next agent is handed it with full confidence — so the living skill
// teaches something false rather than teaching nothing. That is worse than an
// empty store, because nothing reports it.
//
// So Invalidate is not an afterthought: a first failure after a run of
// successes must invalidate, not merely be logged.

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// EntityKind is what a fact is about.
type EntityKind string

const (
	EntityHost     EntityKind = "host"
	EntityService  EntityKind = "service"
	EntityAccount  EntityKind = "account"
	EntityEndpoint EntityKind = "endpoint"

	// The kinds below name the rest of the environment a command reveals.
	// They exist for the SPACE graph — the relations BETWEEN entities — rather
	// than for facts, which are attributes bound to a single entity.
	//
	// EntityPath is deliberately coarse: a repo-relative path or an abstracted
	// class, never an absolute one. An absolute path outside the repo is
	// somebody's home directory, which is identity rather than structure.
	EntityPath EntityKind = "path"
	EntityRepo EntityKind = "repo"

	// EntityNet is a network LOCALITY, not a network address. Its name is a
	// place fingerprint, so "the same wifi as before" is expressible without
	// anything ever recording which wifi.
	EntityNet EntityKind = "net"
)

// Entity is the thing a fact is bound to.
type Entity struct {
	Kind EntityKind `json:"kind"`
	Name string     `json:"name"`
}

// ID is the entity's stable key.
func (e Entity) ID() string { return string(e.Kind) + ":" + strings.ToLower(strings.TrimSpace(e.Name)) }

// Valid reports a usable entity.
func (e Entity) Valid() bool {
	return e.Kind != "" && strings.TrimSpace(e.Name) != ""
}

// Fact is one particular thing learned about one entity.
type Fact struct {
	Entity Entity `json:"entity"`
	Key    string `json:"key"`
	Value  string `json:"value"`

	// ObservedAt is when this host recorded the claim (ingestion time).
	ObservedAt time.Time `json:"observed_at"`
	// ValidFrom / ValidUntil are when the claim held in the WORLD. The second
	// timeline is what makes "what did we believe then" answerable — a single
	// timestamp can only say when a row was written.
	ValidFrom  time.Time  `json:"valid_from"`
	ValidUntil *time.Time `json:"valid_until,omitempty"`

	// Source is what learned it — a skill name, a run. Provenance is not
	// decoration: a wrong fact has to be traceable to whatever asserted it.
	Source string `json:"source,omitempty"`
	// Coordinate is where it was learned. A fact learned on one coordinate is
	// not automatically true on another, and recording it is what lets that be
	// checked rather than assumed.
	Coordinate string `json:"coordinate,omitempty"`
}

// Live reports whether the fact is still believed at t.
func (f Fact) Live(t time.Time) bool {
	if f.ValidUntil != nil && !t.Before(*f.ValidUntil) {
		return false
	}
	return !t.Before(f.ValidFrom)
}

// FactStore is the append-only, host-local record.
//
// There is deliberately no Export, no Sync, and no marshaller that emits the
// whole set: the absence of an export path is the enforcement. A store that
// could be shipped would eventually be shipped.
type FactStore struct {
	path string
}

// factFileMode keeps facts readable only by their owner. They are identity, and
// the default 0644 would put a machine's logins in every backup and every
// container image built from the home directory.
const factFileMode = 0o600

// OpenFacts opens (without creating) the fact log under a store directory.
func OpenFacts(storeDir string) *FactStore {
	if strings.TrimSpace(storeDir) == "" {
		return &FactStore{}
	}
	return &FactStore{path: filepath.Join(storeDir, "facts.jsonl")}
}

// Path is the log's location, for diagnostics.
func (s *FactStore) Path() string { return s.path }

// ErrNoStore reports a store with nowhere to write.
var ErrNoStore = errors.New("craft: no fact store directory configured")

// Record appends one fact.
//
// Recording the SAME key for an entity again supersedes the previous value:
// the old record is closed off rather than removed, so the history stays
// answerable. This is why Record takes the whole fact and not a patch.
func (s *FactStore) Record(f Fact) error {
	if s.path == "" {
		return ErrNoStore
	}
	if !f.Entity.Valid() {
		return errors.New("craft: fact has no entity")
	}
	if strings.TrimSpace(f.Key) == "" {
		return errors.New("craft: fact has no key")
	}
	now := time.Now().UTC()
	if f.ObservedAt.IsZero() {
		f.ObservedAt = now
	}
	if f.ValidFrom.IsZero() {
		f.ValidFrom = f.ObservedAt
	}

	// Close off any live record of the same (entity, key) first, so a reader
	// never sees two live values for one slot and has to guess.
	if prev, ok := s.current(f.Entity, f.Key, now); ok && prev.Value != f.Value {
		if err := s.invalidate(prev, f.ValidFrom); err != nil {
			return err
		}
	}
	return s.append(f)
}

// Invalidate marks what is currently believed about (entity, key) as no longer
// true, without asserting a replacement.
//
// The distinction from Record matters: "this is now wrong" and "this is now X"
// are different claims, and a run that fails has learned only the first. Forcing
// a caller to invent a replacement value is how a guess enters the store.
func (s *FactStore) Invalidate(e Entity, key string, at time.Time) error {
	if s.path == "" {
		return ErrNoStore
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	cur, ok := s.current(e, key, at)
	if !ok {
		return nil // nothing believed; invalidating is a no-op, not an error
	}
	return s.invalidate(cur, at)
}

func (s *FactStore) invalidate(f Fact, at time.Time) error {
	f.ValidUntil = &at
	f.ObservedAt = time.Now().UTC()
	return s.append(f)
}

func (s *FactStore) append(f Fact) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, factFileMode)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewEncoder(file).Encode(f)
}

// All returns every record in the log, oldest first.
func (s *FactStore) All() []Fact {
	if s.path == "" {
		return nil
	}
	file, err := os.Open(s.path)
	if err != nil {
		// A store never written to is empty, not broken. Reads happen on the
		// compose path, which must not fail because nothing has been learned yet.
		return nil
	}
	defer file.Close()

	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var out []Fact
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var f Fact
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			continue // one bad line must not hide the rest
		}
		out = append(out, f)
	}
	return out
}

// current returns the live record for (entity, key) at t.
//
// The log is replayed and the LAST live record wins, which is what makes
// append-only work: correction is expressed by writing again, never by editing.
func (s *FactStore) current(e Entity, key string, t time.Time) (Fact, bool) {
	var found Fact
	var ok bool
	for _, f := range s.All() {
		if f.Entity.ID() != e.ID() || f.Key != key {
			continue
		}
		if f.Live(t) {
			found, ok = f, true
			continue
		}
		// An invalidation closes the slot; a later record may reopen it.
		ok = false
	}
	return found, ok
}

// For returns everything currently believed about an entity, sorted by key.
func (s *FactStore) For(e Entity) []Fact {
	return s.forAt(e, time.Now().UTC())
}

// AsOf returns what was believed about an entity at a past instant — the
// question a single-timestamp store cannot answer, and the only way to explain
// after the fact why an agent did what it did.
func (s *FactStore) AsOf(e Entity, t time.Time) []Fact { return s.forAt(e, t) }

func (s *FactStore) forAt(e Entity, t time.Time) []Fact {
	live := map[string]Fact{}
	for _, f := range s.All() {
		if f.Entity.ID() != e.ID() {
			continue
		}
		if f.Live(t) {
			live[f.Key] = f
			continue
		}
		delete(live, f.Key)
	}
	out := make([]Fact, 0, len(live))
	for _, f := range live {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Entities lists every entity with at least one live fact.
func (s *FactStore) Entities() []Entity {
	now := time.Now().UTC()
	seen := map[string]Entity{}
	for _, f := range s.All() {
		if f.Live(now) {
			seen[f.Entity.ID()] = f.Entity
		}
	}
	out := make([]Entity, 0, len(seen))
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}
