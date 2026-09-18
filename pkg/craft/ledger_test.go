package craft

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	dhntskills "github.com/dhnt/dhnt/skills"

	"github.com/qiangli/yoke/pkg/skills"
)

// writeAttest appends receipts to <store>/attest/<name>.jsonl the way a run does.
func writeAttest(t *testing.T, store, name string, recs ...skills.AttestRecord) {
	t.Helper()
	dir := filepath.Join(store, "attest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	path := filepath.Join(dir, name+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		t.Fatal(err)
	}
}

func rec(at time.Time, name, capKey, ctx string, valid bool) skills.AttestRecord {
	return skills.AttestRecord{
		At:         at,
		Name:       name,
		Tier:       "bashy@test",
		ContextKey: ctx,
		Capability: capKey,
		Attest:     dhntskills.Attestation{Skill: "h" + name, Valid: valid},
	}
}

// A host that never ran a contracted skill has no evidence, and that is an empty
// ledger — not an error. Skills reads sit on the first-hop context path and must
// never fail.
func TestReadLedger_MissingStoreIsEmptyNotAnError(t *testing.T) {
	l, err := ReadLedger(filepath.Join(t.TempDir(), "nonexistent"))
	if err != nil {
		t.Fatalf("ReadLedger on a missing store: %v", err)
	}
	if len(l.Observations) != 0 {
		t.Errorf("got %d observations from a missing store", len(l.Observations))
	}

	if l, err := ReadLedger(""); err != nil || len(l.Observations) != 0 {
		t.Errorf("ReadLedger(\"\") = %v, %v; want an empty ledger and no error", l, err)
	}
}

func TestReadLedger_CountsPassesAndFailures(t *testing.T) {
	store := t.TempDir()
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	writeAttest(t, store, "go-repo-health",
		rec(base, "go-repo-health", "kbuild", "cdarwin", true),
		rec(base.Add(time.Hour), "go-repo-health", "kbuild", "cdarwin", false),
		rec(base.Add(2*time.Hour), "go-repo-health", "kbuild", "clinux", true),
	)

	l, err := ReadLedger(store)
	if err != nil {
		t.Fatal(err)
	}

	s := l.ForSkill("go-repo-health")
	if s.Runs != 3 || s.Passed != 2 || s.Failed != 1 {
		t.Errorf("Stats = %+v, want 3 runs / 2 pass / 1 fail", s)
	}
	if got, want := s.Contribution(), 1.0/3.0; got < want-0.001 || got > want+0.001 {
		t.Errorf("Contribution = %v, want ~%v", got, want)
	}
	if !slices.Equal(s.Coordinates, []string{"cdarwin", "clinux"}) {
		t.Errorf("Coordinates = %v", s.Coordinates)
	}
	if !s.First.Equal(base) || !s.Last.Equal(base.Add(2*time.Hour)) {
		t.Errorf("First/Last = %v / %v", s.First, s.Last)
	}
}

// The payoff of the second identity: evidence pools across interchangeable
// implementations. Two differently-named skills making the same guarantee are
// one capability, and asking "does this promise hold here" must see both.
func TestLedger_PoolsEvidenceAcrossImplementations(t *testing.T) {
	store := t.TempDir()
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	writeAttest(t, store, "go-repo-health",
		rec(base, "go-repo-health", "kbuildtest", "cdarwin", true))
	writeAttest(t, store, "rust-repo-health",
		rec(base.Add(time.Minute), "rust-repo-health", "kbuildtest", "clinux", true),
		rec(base.Add(2*time.Minute), "rust-repo-health", "kbuildtest", "clinux", false))

	l, err := ReadLedger(store)
	if err != nil {
		t.Fatal(err)
	}

	// Per name, each is a thin record.
	if s := l.ForSkill("go-repo-health"); s.Runs != 1 {
		t.Errorf("go-repo-health runs = %d, want 1", s.Runs)
	}
	// Per capability, the promise has a real track record.
	s := l.ForCapability("kbuildtest")
	if s.Runs != 3 || s.Passed != 2 || s.Failed != 1 {
		t.Errorf("capability stats = %+v, want 3/2/1", s)
	}
	if !slices.Equal(l.Capabilities(), []string{"kbuildtest"}) {
		t.Errorf("Capabilities = %v", l.Capabilities())
	}
}

// A capability key of "" must never act as a bucket that silently merges every
// contract-less skill.
func TestLedger_EmptyCapabilityIsNotABucket(t *testing.T) {
	store := t.TempDir()
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	writeAttest(t, store, "prose-a", rec(base, "prose-a", "", "cdarwin", true))
	writeAttest(t, store, "prose-b", rec(base, "prose-b", "", "cdarwin", true))

	l, err := ReadLedger(store)
	if err != nil {
		t.Fatal(err)
	}
	if s := l.ForCapability(""); s.Runs != 0 {
		t.Errorf("ForCapability(\"\") pooled %d runs; contract-less skills must not merge", s.Runs)
	}
	if got := l.Capabilities(); len(got) != 0 {
		t.Errorf("Capabilities = %v, want none", got)
	}
}

// Evidence gathered elsewhere is evidence about elsewhere.
func TestLedger_AtNarrowsToOneCoordinate(t *testing.T) {
	store := t.TempDir()
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	writeAttest(t, store, "s",
		rec(base, "s", "k1", "cdarwin", true),
		rec(base.Add(time.Hour), "s", "k1", "clinux", false),
	)

	l, err := ReadLedger(store)
	if err != nil {
		t.Fatal(err)
	}
	if s := l.At("cdarwin").ForSkill("s"); s.Runs != 1 || s.Passed != 1 {
		t.Errorf("darwin stats = %+v, want 1 run / 1 pass", s)
	}
	if s := l.At("clinux").ForSkill("s"); s.Runs != 1 || s.Failed != 1 {
		t.Errorf("linux stats = %+v, want 1 run / 1 fail", s)
	}
}

// Corruption must be COUNTED, not swallowed. A summary that quietly dropped
// unreadable records would present missing evidence as clean evidence — the one
// thing the fleet-evidence invariant forbids.
func TestReadLedger_ReportsMalformedLines(t *testing.T) {
	store := t.TempDir()
	dir := filepath.Join(store, "attest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	good, err := json.Marshal(rec(time.Now().UTC(), "s", "k1", "c1", true))
	if err != nil {
		t.Fatal(err)
	}
	body := string(good) + "\n" + "{not json\n" + "\n" + string(good) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := ReadLedger(store)
	if err != nil {
		t.Fatalf("a corrupt line must not fail the read: %v", err)
	}
	if len(l.Observations) != 2 {
		t.Errorf("got %d observations, want the 2 readable ones", len(l.Observations))
	}
	if l.Malformed != 1 {
		t.Errorf("Malformed = %d, want 1 (blank lines are not corruption)", l.Malformed)
	}
}

// The index is derived, so two reads of one store must agree exactly — otherwise
// nothing built on top of it is reproducible.
func TestReadLedger_DeterministicOrder(t *testing.T) {
	store := t.TempDir()
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	// Same timestamp across two files, to force the tiebreak to do work.
	writeAttest(t, store, "bravo", rec(base, "bravo", "k", "c2", true))
	writeAttest(t, store, "alpha", rec(base, "alpha", "k", "c1", true))

	first, err := ReadLedger(store)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ReadLedger(store)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(observedNames(first.Observations), observedNames(second.Observations)) {
		t.Fatalf("two reads disagreed: %v vs %v", observedNames(first.Observations), observedNames(second.Observations))
	}
	if got := observedNames(first.Observations); !slices.Equal(got, []string{"alpha", "bravo"}) {
		t.Errorf("order = %v, want name as the tiebreak on equal timestamps", got)
	}
}

// observedNames extracts observation names; distinct from source_test.go's
// names(), which reads Listings.
func observedNames(obs []Observation) []string {
	out := make([]string, 0, len(obs))
	for _, o := range obs {
		out = append(out, o.Name)
	}
	return out
}

// A receipt written before the Capability field existed is still valid evidence,
// and must not be assigned a fabricated capability.
func TestReadLedger_LegacyRecordsKeepEmptyCapability(t *testing.T) {
	store := t.TempDir()
	dir := filepath.Join(store, "attest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"at":"2026-07-01T00:00:00Z","name":"old","tier":"bashy@old","context_key":"c1","attest":{"Skill":"hold","Valid":true}}`
	if err := os.WriteFile(filepath.Join(dir, "old.jsonl"), []byte(legacy+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := ReadLedger(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Observations) != 1 {
		t.Fatalf("got %d observations, want 1", len(l.Observations))
	}
	o := l.Observations[0]
	if o.Capability != "" {
		t.Errorf("legacy record got capability %q; it must stay empty rather than be inferred", o.Capability)
	}
	if !o.Valid || o.Name != "old" || o.Identity != "hold" {
		t.Errorf("legacy record decoded wrong: %+v", o)
	}
}
