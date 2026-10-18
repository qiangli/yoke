package kb

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
)

// The `sense:` observation contract (Sprint 284 M1, decided by the sprint
// manager; the reading schema waits on Sprint 303 S1).
//
// `sense:` is NOT a pkg/ref vocabulary kind and never a KB store: no live
// telemetry is ingested. `kb show` special-cases the `sense:` prefix and
// calls the injected resolver below; the reading contributes at most an
// observation — source ref, observed_at, freshness/status and snapshot
// digest — never a stored record.
//
// A reading on the wire:
//
//	{
//	  "schema_version": "bashy-sense-reading-v1",
//	  "sense_id": "cpu-0",
//	  "subject": "host:buildbox",
//	  "source": "sense:cpu-0",
//	  "observed_at": "2026-09-13T08:14:00Z",
//	  "cadence": "60s",
//	  "status": "ok",
//	  "value": {"load": 0.4},
//	  "snapshot": "sha256:<hex of the value bytes>",
//	  "principals": ["dhnt:agent/007"]
//	}
//
// Freshness is computed at read time, never stored: fresh means status ok and
// 0 <= now-observed_at <= cadence; stale means status ok and now-observed_at
// > cadence; unknown is everything else (status not ok, missing or invalid
// observed_at or cadence, observed_at in the future). Fresh maps to `found`
// (content = value); stale or unknown maps to `expired` with freshness set
// and NO content — never a current fact. A resolver ref.ErrNotFound maps to
// `not-found`; a requester principal outside `principals` maps to `denied`;
// any other resolver error maps to `error`.

// SenseSchemaVersion is the discriminator every sense reading carries.
const SenseSchemaVersion = "bashy-sense-reading-v1"

// Sense status values.
const (
	SenseStatusOK          = "ok"
	SenseStatusUnavailable = "unavailable"
	SenseStatusUnknown     = "unknown"
	SenseStatusError       = "error"
)

// Sense freshness values, computed at read time.
const (
	SenseFreshnessFresh   = "fresh"
	SenseFreshnessStale   = "stale"
	SenseFreshnessUnknown = "unknown"
)

// SenseReading is one sense observation: the only fields a reading may
// contribute to `kb show` (§5.1). Value is raw JSON, present only when Status
// is ok. Principals empty means any principal may read; otherwise the
// requester principal (RequestPrincipal) must be listed.
type SenseReading struct {
	SchemaVersion string          `json:"schema_version"`
	SenseID       string          `json:"sense_id"`
	Subject       string          `json:"subject"`
	Source        string          `json:"source"`
	ObservedAt    string          `json:"observed_at"`
	Cadence       string          `json:"cadence"`
	Status        string          `json:"status"`
	Value         json.RawMessage `json:"value,omitempty"`
	Snapshot      string          `json:"snapshot,omitempty"`
	Principals    []string        `json:"principals,omitempty"`
}

// SenseResolver supplies one reading by sense id. ref.ErrNotFound reports a
// missing reading; any other error is a store failure.
type SenseResolver func(senseID string) (SenseReading, error)

var (
	senseResolver SenseResolver
	senseClock    = time.Now
)

// SetSenseResolver installs the sense consumer's resolver; nil uninstalls it.
// With no resolver wired, `kb show sense:<id>` reports outcome `error`.
func SetSenseResolver(r SenseResolver) { senseResolver = r }

// SetSenseClock installs the clock freshness is computed against; nil
// restores time.Now. Tests pin "now".
func SetSenseClock(f func() time.Time) {
	if f == nil {
		f = time.Now
	}
	senseClock = f
}

// RequestPrincipal is the principal the read is for, from $BASHY_PRINCIPAL.
func RequestPrincipal() string { return strings.TrimSpace(os.Getenv("BASHY_PRINCIPAL")) }

// SenseFreshness computes fresh|stale|unknown for rd at now.
func SenseFreshness(rd SenseReading, now time.Time) string {
	if rd.Status != SenseStatusOK {
		return SenseFreshnessUnknown
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(rd.ObservedAt))
	if err != nil {
		return SenseFreshnessUnknown
	}
	cadence, err := time.ParseDuration(strings.TrimSpace(rd.Cadence))
	if err != nil {
		return SenseFreshnessUnknown
	}
	age := now.Sub(at.UTC())
	if age < 0 {
		return SenseFreshnessUnknown
	}
	if age > cadence {
		return SenseFreshnessStale
	}
	return SenseFreshnessFresh
}

// senseAllowed reports whether the requester may read rd: an empty
// principals list admits any principal, otherwise the requester must be
// listed.
func senseAllowed(rd SenseReading, principal string) bool {
	if len(rd.Principals) == 0 {
		return true
	}
	for _, p := range rd.Principals {
		if p == principal {
			return true
		}
	}
	return false
}

// errEmptySenseID reports `kb show sense:` with no id; it surfaces as an
// outcome-`error` envelope, like ref.ErrEmptyID does for vocabulary kinds.
var errEmptySenseID = errors.New("sense: a ref needs an id after the kind — sense:<id>, as in sense:cpu-0")
