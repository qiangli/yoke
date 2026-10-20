package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The durable operation journal (S472.13a item 3). Every provider call is
// bracketed by two records: the intent, written BEFORE the call with the
// idempotency key and the resource IDs the caller knew, and the result,
// written AFTER it with the resource IDs the provider reported. A record
// with an intent and no result is exactly the state a crash between the
// two leaves behind; Dispatcher.Resume reconciles it from the provider
// (list by idempotency tag) before anything is retried, and marks it
// needs_manual_reconcile when the outcome cannot be proven.
//
// The journal is append-only. Nothing in it is ever rewritten: a refused
// destroy, a failed call, an expired lease are all further records, so the
// history of a resource is the sequence of records that name it.
//
// Storage is behind JournalStore: the file store below (JSON lines under
// BASHY_HOME) serves local bashy and a satellite; the master supplies its
// own. Everything passes through the dispatcher's redactor first, so a
// store never sees a credential.

// Phase is the stage a journal record describes.
type Phase string

const (
	// PhaseIntent is written before the provider call.
	PhaseIntent Phase = "intent"
	// PhaseResult is written after the provider answered (success or a
	// normalized failure that proves nothing happened).
	PhaseResult Phase = "result"
	// PhaseRefused is a request the dispatcher refused before any
	// provider call (no confirmation, exec disabled, unsupported op).
	PhaseRefused Phase = "refused"
	// PhaseAmbiguous is a call whose outcome is unknown (timeout, lost
	// response, 5xx): the provider may or may not have acted.
	PhaseAmbiguous Phase = "ambiguous"
	// PhaseReconciled closes an intent or ambiguous record from what the
	// provider reports (list by idempotency tag, inspect by ID).
	PhaseReconciled Phase = "reconciled"
	// PhaseNeedsManualReconcile closes nothing: the outcome could not be
	// proven and a human has to look.
	PhaseNeedsManualReconcile Phase = "needs_manual_reconcile"
)

// JournalEntry is one journal record.
type JournalEntry struct {
	Seq            int64          `json:"seq,omitempty"`
	At             time.Time      `json:"at"`
	Provider       string         `json:"provider"`
	Form           Form           `json:"form,omitempty"`
	Op             Op             `json:"op"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
	Phase          Phase          `json:"phase"`
	Attempt        int            `json:"attempt,omitempty"`
	ResourceID     string         `json:"resource_id,omitempty"`        // before the call
	ResultID       string         `json:"result_resource_id,omitempty"` // after the call
	State          State          `json:"state,omitempty"`
	Tags           []string       `json:"tags,omitempty"`
	Spec           *Spec          `json:"spec,omitempty"`
	Authorization  *Authorization `json:"authorization,omitempty"`
	Error          *Error         `json:"error,omitempty"`
	Cost           *CostReport    `json:"cost,omitempty"`
	Note           string         `json:"note,omitempty"`
}

// JournalStore persists journal records for one host. Append must be
// durable before it returns; Load returns every record of provider in
// append order. Implementations must be safe for concurrent use.
type JournalStore interface {
	Append(ctx context.Context, e JournalEntry) error
	Load(ctx context.Context, provider string) ([]JournalEntry, error)
}

// ---- memory store ----

// MemoryJournal is an in-memory JournalStore (tests, the master's cache).
type MemoryJournal struct {
	mu      sync.Mutex
	entries []JournalEntry
}

// NewMemoryJournal returns an empty in-memory store.
func NewMemoryJournal() *MemoryJournal { return &MemoryJournal{} }

// Append implements JournalStore.
func (m *MemoryJournal) Append(_ context.Context, e JournalEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.Seq = int64(len(m.entries) + 1)
	m.entries = append(m.entries, e)
	return nil
}

// Load implements JournalStore.
func (m *MemoryJournal) Load(_ context.Context, provider string) ([]JournalEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []JournalEntry
	for _, e := range m.entries {
		if provider == "" || e.Provider == provider {
			out = append(out, e)
		}
	}
	return out, nil
}

// ---- file store ----

// FileJournal is the local JournalStore: one JSON-lines file per provider
// under dir, appended with O_APPEND so concurrent writers on one host
// interleave whole records. Files are 0600, the directory 0700.
type FileJournal struct {
	dir string
	mu  sync.Mutex
}

var journalFileName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// NewFileJournal returns a file store rooted at dir, creating it.
func NewFileJournal(dir string) (*FileJournal, error) {
	if dir == "" {
		return nil, errors.New("provider journal: directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("provider journal: %w", err)
	}
	return &FileJournal{dir: dir}, nil
}

// DefaultJournalDir is where the local journal lives: BASHY_HOME/provider/
// journal, with BASHY_HOME defaulting to $HOME/.bashy.
func DefaultJournalDir() (string, error) {
	home := os.Getenv("BASHY_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("provider journal: %w", err)
		}
		home = filepath.Join(h, ".bashy")
	}
	return filepath.Join(home, "provider", "journal"), nil
}

// Dir is the store's directory.
func (f *FileJournal) Dir() string { return f.dir }

func (f *FileJournal) path(provider string) string {
	name := journalFileName.ReplaceAllString(provider, "_")
	if name == "" || name == "." || name == ".." {
		name = "_"
	}
	return filepath.Join(f.dir, name+".jsonl")
}

// Append implements JournalStore.
func (f *FileJournal) Append(_ context.Context, e JournalEntry) error {
	if e.Provider == "" {
		return errors.New("provider journal: entry without a provider")
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("provider journal: %w", err)
	}
	if strings.ContainsAny(string(line), "\n") {
		return errors.New("provider journal: record contains a newline")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fh, err := os.OpenFile(f.path(e.Provider), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("provider journal: %w", err)
	}
	defer fh.Close()
	if _, err := fh.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("provider journal: %w", err)
	}
	return fh.Sync()
}

// Load implements JournalStore. A truncated last line (a crash mid-write)
// is ignored; any other malformed line is an error.
func (f *FileJournal) Load(_ context.Context, provider string) ([]JournalEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fh, err := os.Open(f.path(provider))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("provider journal: %w", err)
	}
	defer fh.Close()
	var out []JournalEntry
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	var seq int64
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var e JournalEntry
		if err := json.Unmarshal(line, &e); err != nil {
			// A partial trailing record is the one malformed line a
			// crash can leave; anything before it must be whole.
			if err := sc.Err(); err != nil {
				return nil, fmt.Errorf("provider journal: %w", err)
			}
			return out, nil
		}
		seq++
		e.Seq = seq
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("provider journal: %w", err)
	}
	return out, nil
}

// ---- replay view ----

// Operation is the journal's view of one (op, idempotency key) pair: the
// records that describe it, folded into its current standing.
type Operation struct {
	Op             Op
	IdempotencyKey string
	ResourceID     string // the resource the op addressed or produced
	Attempts       int
	// Pending is true when the latest record is an intent or an ambiguous
	// result: the provider may have acted and nothing proves it either way.
	Pending bool
	// Done is true when a successful result or a reconciled outcome closed
	// the op. Result is that record.
	Done   bool
	Result *JournalEntry
	// NeedsManualReconcile is true when reconciliation could not prove
	// the outcome; a human has to look at the provider.
	NeedsManualReconcile bool
	// Refused counts refusals before any provider call.
	Refused int
	First   time.Time
	Last    time.Time
}

// JournaledResource is the journal's view of one provider resource.
type JournaledResource struct {
	ID        string
	CreateKey string
	Tags      []string
	State     State
	// Temporary is the temporary-lifecycle authorization recorded at
	// create, if any.
	Temporary *Authorization
	// Destroyed is true once a destroy result or reconciliation reported
	// StateDeleted. A refused or failed destroy leaves it false.
	Destroyed bool
	// Cost is the latest cost report (stop, refused destroy, destroy).
	Cost *CostReport
	Last time.Time
}

// JournalView folds a provider's records into operations and resources.
type JournalView struct {
	Operations []Operation
	Resources  []JournaledResource
}

// Replay folds entries (append order) into a view. It is deterministic
// and pure: the same records always give the same view.
func Replay(entries []JournalEntry) JournalView {
	ops := map[string]*Operation{}
	var opOrder []string
	res := map[string]*JournaledResource{}
	var resOrder []string
	resource := func(id string) *JournaledResource {
		r := res[id]
		if r == nil {
			r = &JournaledResource{ID: id, State: StateUnknown}
			res[id] = r
			resOrder = append(resOrder, id)
		}
		return r
	}
	for i := range entries {
		e := &entries[i]
		if e.Op == "" {
			continue
		}
		k := string(e.Op) + "\x00" + e.IdempotencyKey
		o := ops[k]
		if o == nil {
			o = &Operation{Op: e.Op, IdempotencyKey: e.IdempotencyKey, First: e.At}
			ops[k] = o
			opOrder = append(opOrder, k)
		}
		o.Last = e.At
		// A refusal never reached the provider, so it does not say which
		// resource the operation acted on; only attempts and outcomes do.
		if e.ResourceID != "" && e.Phase != PhaseRefused {
			o.ResourceID = e.ResourceID
		}
		switch e.Phase {
		case PhaseIntent:
			o.Attempts++
			o.Pending = true
			o.NeedsManualReconcile = false
			if e.Op == OpCreate && e.Authorization != nil && e.Authorization.Kind == AuthTemporary && e.ResultID != "" {
				resource(e.ResultID).Temporary = e.Authorization
			}
		case PhaseAmbiguous:
			o.Pending = true
		case PhaseRefused:
			o.Refused++
			if e.Op == OpDestroy && e.ResourceID != "" {
				r := resource(e.ResourceID)
				if e.Cost != nil {
					r.Cost = e.Cost
				}
				r.Last = e.At
			}
		case PhaseResult, PhaseReconciled:
			o.Pending = false
			o.NeedsManualReconcile = false
			if e.Error == nil {
				o.Done = true
				o.Result = e
			}
			if e.ResultID != "" {
				o.ResourceID = e.ResultID
			}
		case PhaseNeedsManualReconcile:
			o.Pending = false
			o.NeedsManualReconcile = true
		}
		id := e.ResultID
		if id == "" && e.Op != OpCreate {
			id = e.ResourceID
		}
		if id == "" || (e.Phase != PhaseResult && e.Phase != PhaseReconciled) {
			continue
		}
		r := resource(id)
		r.Last = e.At
		if e.Op == OpCreate && e.Error == nil {
			r.CreateKey = e.IdempotencyKey
			if e.Authorization != nil && e.Authorization.Kind == AuthTemporary {
				r.Temporary = e.Authorization
			}
		}
		if e.Error != nil {
			if e.Cost != nil {
				r.Cost = e.Cost
			}
			continue
		}
		if len(e.Tags) > 0 {
			r.Tags = e.Tags
		}
		if e.State != "" {
			r.State = e.State
		}
		if e.Cost != nil {
			r.Cost = e.Cost
		}
		if e.State == StateDeleted {
			r.Destroyed = true
		}
	}
	v := JournalView{}
	for _, k := range opOrder {
		v.Operations = append(v.Operations, *ops[k])
	}
	for _, id := range resOrder {
		v.Resources = append(v.Resources, *res[id])
	}
	sort.SliceStable(v.Operations, func(i, j int) bool { return v.Operations[i].First.Before(v.Operations[j].First) })
	return v
}

// Find returns the operation for (op, key).
func (v JournalView) Find(op Op, key string) (Operation, bool) {
	for _, o := range v.Operations {
		if o.Op == op && o.IdempotencyKey == key {
			return o, true
		}
	}
	return Operation{}, false
}

// Resource returns the journaled resource with id.
func (v JournalView) Resource(id string) (JournaledResource, bool) {
	for _, r := range v.Resources {
		if r.ID == id {
			return r, true
		}
	}
	return JournaledResource{}, false
}

// Pending returns the operations whose outcome is unknown.
func (v JournalView) Pending() []Operation {
	var out []Operation
	for _, o := range v.Operations {
		if o.Pending {
			out = append(out, o)
		}
	}
	return out
}

// ManualReconcile returns the operations a human has to look at.
func (v JournalView) ManualReconcile() []Operation {
	var out []Operation
	for _, o := range v.Operations {
		if o.NeedsManualReconcile {
			out = append(out, o)
		}
	}
	return out
}
