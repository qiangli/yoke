package craft

// THE READ SIDE OF THE EVIDENCE LEDGER.
//
// Every `skills run` has appended a receipt to <store>/attest/<name>.jsonl since
// P2 — timestamped, coordinate-stamped, pass/fail, including the failures ("the
// failed baseline is evidence too"). Nothing has ever read it back. No verb, no
// aggregator, no consumer. The self-improving half of the mechanism was built,
// wired to a writer, and left open-circuit.
//
// This file closes it. It is deliberately a READER over the logs that already
// exist rather than a new store: the durable truth stays the append-only JSONL,
// and everything here is derived and rebuildable from it. Adding an eighth
// parallel log to a tree that already has seven disjoint outcome records would
// be the opposite of the point.
//
// What it deliberately does NOT do: decide anything. No retirement, no election,
// no ranking. Those are policy, they need thresholds that only mean something
// against real accumulated evidence, and inventing them before the corpus exists
// is how you get numbers nobody can defend. This layer answers "what happened",
// and stops there.

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/skills"
)

// Observation is one run receipt, flattened for counting.
//
// It is a projection of skills.AttestRecord, not a new record type: the stored JSONL
// stays the source of truth and this is what the index holds in memory.
type Observation struct {
	At         time.Time `json:"at"`
	Name       string    `json:"name"`
	Identity   string    `json:"identity,omitempty"`   // Attest.Skill — the exact version that ran
	Capability string    `json:"capability,omitempty"` // what it guarantees; empty for contract-less or pre-capability records
	Tier       string    `json:"tier,omitempty"`       // executor: "bashy@<version>", "local", a model id
	ContextKey string    `json:"context_key,omitempty"`
	Valid      bool      `json:"valid"`
	Passed     []string  `json:"passed,omitempty"`
	Failed     []string  `json:"failed,omitempty"`
}

// Ledger is the derived index over every stored receipt. Rebuildable in full
// from the JSONL at any time; holding no state the logs do not.
type Ledger struct {
	// Observations, sorted oldest first, then by name for a total order.
	Observations []Observation

	// Malformed counts lines that could not be decoded.
	//
	// Reported rather than swallowed, on purpose. A ledger that silently
	// skipped corrupt records would report shrinking evidence as clean
	// evidence — an absence presented as a fact — which is the failure mode
	// the fleet-evidence invariant exists to forbid. A caller that ignores
	// this field has made that choice explicitly.
	Malformed int

	// Files is the set of attest logs read, for provenance in diagnostics.
	Files []string
}

// Stats summarises one skill's or one capability's history.
type Stats struct {
	Runs        int
	Passed      int
	Failed      int
	First       time.Time
	Last        time.Time
	Coordinates []string // distinct context keys, sorted
	Tiers       []string // distinct executor tiers, sorted
}

// Contribution is the signed success rate in [-1, 1]: +1 every run held, -1
// every run failed, 0 for an even split or no runs.
//
// It is REPORTED here and acted on nowhere. Retirement thresholds belong to the
// maintenance layer, and they need an evidence floor a single host does not
// reach on its own — a couple of runs is not a track record, and treating it as
// one would retire a good skill on noise.
func (s Stats) Contribution() float64 {
	if s.Runs == 0 {
		return 0
	}
	return float64(s.Passed-s.Failed) / float64(s.Runs)
}

// AttestDir is where `skills run` receipts live (<storeDir>/attest) — the one
// place that path is spelled.
func AttestDir(storeDir string) string { return filepath.Join(storeDir, "attest") }

// ReadLedger loads every attest log under <storeDir>/attest.
//
// A missing store is not an error: a host that has never run a contracted skill
// legitimately has no evidence, and that is an empty ledger rather than a
// failure. Callers on the first-hop context path depend on that — skills must
// never fail a read.
func ReadLedger(storeDir string) (*Ledger, error) {
	l := &Ledger{}
	if strings.TrimSpace(storeDir) == "" {
		return l, nil
	}
	dir := AttestDir(storeDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return l, nil
		}
		return l, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		obs, bad, err := readAttestFile(path)
		if err != nil {
			return l, err
		}
		l.Observations = append(l.Observations, obs...)
		l.Malformed += bad
		l.Files = append(l.Files, path)
	}
	sortObservations(l.Observations)
	sort.Strings(l.Files)
	return l, nil
}

// readAttestFile decodes one JSONL log, returning the observations and the count
// of lines that would not decode.
func readAttestFile(path string) ([]Observation, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	// Receipts embed command output, so a line can be far longer than
	// bufio.Scanner's 64KiB default. Raising the cap keeps a big-but-valid
	// record from being miscounted as corruption.
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	// The filename is the fallback name for a record that omits it.
	fallback := strings.TrimSuffix(filepath.Base(path), ".jsonl")

	var out []Observation
	bad := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec skills.AttestRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			bad++
			continue
		}
		out = append(out, observationOf(rec, fallback))
	}
	if err := sc.Err(); err != nil {
		return out, bad, err
	}
	return out, bad, nil
}

// observationOf flattens a stored receipt.
//
// Capability is carried through when the record has it and left EMPTY when it
// does not. Records written before capability keys existed cannot have one
// recovered from the receipt alone — the contract lived in the skill, not the
// receipt — and guessing one would silently file old evidence under a capability
// it was never observed to satisfy. An empty capability is the honest state, and
// callers join against the catalog if they want more.
func observationOf(rec skills.AttestRecord, fallbackName string) Observation {
	name := strings.TrimSpace(rec.Name)
	if name == "" {
		name = fallbackName
	}
	return Observation{
		At:         rec.At,
		Name:       name,
		Identity:   rec.Attest.Skill,
		Capability: rec.Capability,
		Tier:       rec.Tier,
		ContextKey: rec.ContextKey,
		Valid:      rec.Attest.Valid,
		Passed:     rec.Attest.Passed,
		Failed:     rec.Attest.Failed,
	}
}

// sortObservations imposes a total order: oldest first, then name, then
// coordinate. Ties are broken deterministically so two reads of one store always
// agree — the index has to be reproducible to be worth deriving.
func sortObservations(obs []Observation) {
	sort.SliceStable(obs, func(i, j int) bool {
		a, b := obs[i], obs[j]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ContextKey < b.ContextKey
	})
}

// Names returns every skill with recorded evidence, sorted.
func (l *Ledger) Names() []string {
	return distinct(l.Observations, func(o Observation) string { return o.Name })
}

// Capabilities returns every distinct capability with recorded evidence, sorted.
// Contract-less skills contribute nothing here — they have no capability key.
func (l *Ledger) Capabilities() []string {
	return distinct(l.Observations, func(o Observation) string { return o.Capability })
}

// ForSkill summarises one skill by name, across every version and coordinate.
func (l *Ledger) ForSkill(name string) Stats {
	return l.summarise(func(o Observation) bool { return o.Name == name })
}

// ForCapability summarises every implementation of one capability. This is the
// view election will eventually need: the question is not "is this skill good"
// but "which of these interchangeable skills holds up here".
func (l *Ledger) ForCapability(key string) Stats {
	if key == "" {
		return Stats{}
	}
	return l.summarise(func(o Observation) bool { return o.Capability == key })
}

// At narrows the ledger to one coordinate. Evidence gathered on another host's
// coordinate is evidence about that coordinate, not this one.
func (l *Ledger) At(contextKey string) *Ledger {
	out := &Ledger{Malformed: l.Malformed, Files: l.Files}
	for _, o := range l.Observations {
		if o.ContextKey == contextKey {
			out.Observations = append(out.Observations, o)
		}
	}
	return out
}

func (l *Ledger) summarise(match func(Observation) bool) Stats {
	var s Stats
	for _, o := range l.Observations {
		if !match(o) {
			continue
		}
		s.Runs++
		if o.Valid {
			s.Passed++
		} else {
			s.Failed++
		}
		if s.First.IsZero() || o.At.Before(s.First) {
			s.First = o.At
		}
		if o.At.After(s.Last) {
			s.Last = o.At
		}
		s.Coordinates = appendDistinct(s.Coordinates, o.ContextKey)
		s.Tiers = appendDistinct(s.Tiers, o.Tier)
	}
	sort.Strings(s.Coordinates)
	sort.Strings(s.Tiers)
	return s
}

func distinct(obs []Observation, key func(Observation) string) []string {
	var out []string
	for _, o := range obs {
		out = appendDistinct(out, key(o))
	}
	sort.Strings(out)
	return out
}

func appendDistinct(dst []string, v string) []string {
	if v == "" || slices.Contains(dst, v) {
		return dst
	}
	return append(dst, v)
}
