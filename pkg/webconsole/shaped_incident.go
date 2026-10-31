package webconsole

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	shape "github.com/qiangli/yoke/pkg/shape/state"
)

// An incident is one user-facing error from the principal's active version
// of a shaped app: a 5xx from an api/* unit or the app's declared server, a
// failed asset load, or an uncaught page script error. It is the trigger of
// a self-repair (S549.R1) and is recorded under the principal's own shaping
// state — never another user's data, never credentials.
//
// One incident is one (app, active revision, signature). The same signature
// after a repair activated counts against the incident it came from, so a
// repair that does not fix the error, or that flips back and forth, cannot
// loop: after MaxRepairAttempts the incident is failed and the previous
// working version stays active.

// MaxRepairAttempts bounds repair attempts per incident (operator 2026-10-31).
const MaxRepairAttempts = 2

// IncidentKind is where the error was observed.
type IncidentKind string

const (
	IncidentAPI    IncidentKind = "api"    // api/<unit> returned 5xx
	IncidentServer IncidentKind = "server" // declared server: build/start failure or upstream 5xx
	IncidentAsset  IncidentKind = "asset"  // a page asset failed to load (reported by the page)
	IncidentScript IncidentKind = "script" // uncaught page script error / unhandled rejection
)

// IncidentStatus is the incident's place in the repair lifecycle.
type IncidentStatus string

const (
	IncidentRepairing IncidentStatus = "repairing" // a repair attempt is running
	IncidentActivated IncidentStatus = "activated" // a verified repair is active for this user
	IncidentFailed    IncidentStatus = "failed"    // attempts exhausted; previous version stays active
	IncidentRefused   IncidentStatus = "refused"   // the candidate changes the interface: left for review
	IncidentExternal  IncidentStatus = "external"  // outside bashy's control: reported, not repaired
)

// Incident is the recorded error. Evidence is a bounded, redacted tail
// (stderr of the unit, the page's message/stack); it never carries request
// bodies, cookies, headers or other principals' data.
type Incident struct {
	ID        string          `json:"id"`
	App       string          `json:"app"`
	Kind      IncidentKind    `json:"kind"`
	Path      string          `json:"path"`             // request path below the app mount, no query
	Status    int             `json:"status,omitempty"` // HTTP status when there is one
	Message   string          `json:"message"`          // one line, what the user would see
	Evidence  string          `json:"evidence,omitempty"`
	Signature string          `json:"signature"`
	BaseRev   string          `json:"base_rev"` // active revision when first seen
	State     IncidentStatus  `json:"state"`
	Attempts  []RepairAttempt `json:"attempts,omitempty"`
	FirstSeen time.Time       `json:"first_seen"`
	LastSeen  time.Time       `json:"last_seen"`
	Count     int             `json:"count"`
}

// RepairAttempt is one candidate tried for an incident.
type RepairAttempt struct {
	Candidate string    `json:"candidate"`
	Result    string    `json:"result"` // activated | failed | refused
	Reason    string    `json:"reason,omitempty"`
	Revision  string    `json:"revision,omitempty"` // activated revision
	Version   string    `json:"version,omitempty"`  // patch version tag
	At        time.Time `json:"at"`
}

// incidentEvidenceLimit caps Evidence bytes kept per incident.
const incidentEvidenceLimit = 4 << 10

// IncidentSignature identifies "the same error": kind, path and the first
// line of the message with digits folded, so request ids, ports and line
// numbers do not split one error into many incidents.
func IncidentSignature(kind IncidentKind, path, message string) string {
	line := message
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	var b strings.Builder
	for _, r := range line {
		if r >= '0' && r <= '9' {
			b.WriteByte('#')
			continue
		}
		b.WriteRune(r)
	}
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + path + "\x00" + strings.TrimSpace(b.String())))
	return hex.EncodeToString(sum[:8])
}

// incidentStore keeps one principal's incidents for one app under the
// principal's shaping state directory: <state dir>/incidents/<id>.json.
type incidentStore struct {
	dir string
	mu  sync.Mutex
}

func openIncidents(st *shape.Store) (*incidentStore, error) {
	d := filepath.Join(st.Dir(), "incidents")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return nil, err
	}
	return &incidentStore{dir: d}, nil
}

// ErrAttemptsExhausted is returned when an incident may not try again.
var ErrAttemptsExhausted = errors.New("repair attempts exhausted")

// Observe records an occurrence of an error. A known signature that is still
// open (repairing, activated, failed) updates that incident — an error
// recurring after an activated repair re-opens it as repairing if attempts
// remain, else marks it failed. A new signature starts a new incident.
// The returned incident tells the caller whether to start a repair
// (State == repairing and no attempt in flight).
func (s *incidentStore) Observe(in Incident, now time.Time) (Incident, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.Signature == "" {
		in.Signature = IncidentSignature(in.Kind, in.Path, in.Message)
	}
	if len(in.Evidence) > incidentEvidenceLimit {
		in.Evidence = in.Evidence[len(in.Evidence)-incidentEvidenceLimit:]
	}
	all, err := s.listLocked()
	if err != nil {
		return Incident{}, false, err
	}
	for _, cur := range all {
		if cur.Signature != in.Signature || cur.State == IncidentRefused || cur.State == IncidentExternal {
			continue
		}
		cur.LastSeen = now
		cur.Count++
		start := false
		switch cur.State {
		case IncidentActivated:
			// The repair did not hold: same error again.
			if len(cur.Attempts) < MaxRepairAttempts {
				cur.State = IncidentRepairing
				start = true
			} else {
				cur.State = IncidentFailed
			}
		}
		return cur, start, s.writeLocked(cur)
	}
	in.ID = fmt.Sprintf("inc-%s-%s", now.UTC().Format("20060102T150405"), in.Signature[:8])
	in.FirstSeen, in.LastSeen, in.Count = now, now, 1
	if in.State == "" {
		in.State = IncidentRepairing
	}
	return in, in.State == IncidentRepairing, s.writeLocked(in)
}

// Attempt records the outcome of one repair attempt and moves the incident.
// It refuses a third attempt.
func (s *incidentStore) Attempt(id string, a RepairAttempt) (Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, err := s.getLocked(id)
	if err != nil {
		return Incident{}, err
	}
	if len(in.Attempts) >= MaxRepairAttempts {
		return in, ErrAttemptsExhausted
	}
	in.Attempts = append(in.Attempts, a)
	switch a.Result {
	case "activated":
		in.State = IncidentActivated
	case "refused":
		in.State = IncidentRefused
	default:
		if len(in.Attempts) >= MaxRepairAttempts {
			in.State = IncidentFailed
		} else {
			in.State = IncidentRepairing
		}
	}
	return in, s.writeLocked(in)
}

// Get returns one incident.
func (s *incidentStore) Get(id string) (Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(id)
}

// List returns incidents, newest first.
func (s *incidentStore) List() ([]Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

func (s *incidentStore) getLocked(id string) (Incident, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.HasPrefix(id, ".") {
		return Incident{}, os.ErrNotExist
	}
	b, err := os.ReadFile(filepath.Join(s.dir, id+".json"))
	if err != nil {
		return Incident{}, err
	}
	var in Incident
	return in, json.Unmarshal(b, &in)
}

func (s *incidentStore) listLocked() ([]Incident, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []Incident
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		in, err := s.getLocked(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FirstSeen.After(out[j].FirstSeen) })
	return out, nil
}

func (s *incidentStore) writeLocked(in Incident) error {
	b, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, in.ID+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, in.ID+".json"))
}
