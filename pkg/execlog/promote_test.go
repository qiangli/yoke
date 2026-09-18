// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package execlog

import (
	"testing"
	"time"
)

type ev struct {
	day     int // days ago
	episode string
	argv    []string
	exit    int
	benign  bool
	opaque  bool
	dim     string
}

func seed(t *testing.T, evs []ev) string {
	t.Helper()
	root := t.TempDir()
	w := Open(root)
	defer w.Close()

	now := time.Now().UTC()
	for i, e := range evs {
		at := now.AddDate(0, 0, -e.day).Add(time.Duration(i) * time.Minute)
		body := Scrub(nil, e.argv, "/w", TemplateOpts{})
		exit := e.exit
		rec := Record{
			At: at, Cmd: e.argv[0], PID: 1, Observed: true, Exit: &exit,
			Benign: e.benign, Opaque: e.opaque, Dimension: e.dim,
		}
		if err := w.Append(rec, body, e.episode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func only(t *testing.T, ps []Pitfall, template string) Pitfall {
	t.Helper()
	for _, p := range ps {
		if p.Template == template {
			return p
		}
	}
	t.Fatalf("no pitfall for %q; got %+v", template, ps)
	return Pitfall{}
}

func TestPromoteAdmitsRepeatedFailure(t *testing.T) {
	root := seed(t, []ev{
		{day: 4, episode: "ep-a", argv: []string{"go", "test", "./hub/..."}, exit: 1, dim: "compute"},
		{day: 2, episode: "ep-b", argv: []string{"go", "test", "./hub/..."}, exit: 1, dim: "compute"},
		{day: 1, episode: "ep-c", argv: []string{"go", "test", "./hub/..."}, exit: 1, dim: "compute"},
	})
	ps, _, err := Promote(root, PromoteDefaults())
	if err != nil {
		t.Fatal(err)
	}
	p := only(t, ps, "go test ./hub/...")
	if p.Episodes != 3 || p.Days != 3 || p.Failures != 3 {
		t.Errorf("evidence wrong: %+v", p)
	}
	if p.Dimension != "compute" {
		t.Errorf("dimension must carry through, got %q", p.Dimension)
	}
}

// TestOneSessionIsOneObservation is the threshold that matters most. An agent
// retrying a doomed command eleven times in one session has observed it ONCE;
// counting executions would let a single bad afternoon mint a permanent claim.
func TestPromoteOneSessionIsOneObservation(t *testing.T) {
	var evs []ev
	for i := 0; i < 11; i++ {
		evs = append(evs, ev{day: 1, episode: "ep-same", argv: []string{"go", "test", "./..."}, exit: 1})
	}
	ps, _, err := Promote(seed(t, evs), PromoteDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 0 {
		t.Errorf("11 failures in ONE episode on ONE day must not promote: %+v", ps)
	}
}

// TestSpacingIsRequired — three episodes on one day is a transient (a broken
// VPN, a full disk, a service restarting), not a property of the command.
func TestPromoteSpacingIsRequired(t *testing.T) {
	ps, _, _ := Promote(seed(t, []ev{
		{day: 1, episode: "ep-a", argv: []string{"go", "test", "./..."}, exit: 1},
		{day: 1, episode: "ep-b", argv: []string{"go", "test", "./..."}, exit: 1},
		{day: 1, episode: "ep-c", argv: []string{"go", "test", "./..."}, exit: 1},
	}), PromoteDefaults())
	if len(ps) != 0 {
		t.Errorf("three episodes on one day must not promote: %+v", ps)
	}
}

// TestSuccessSupersedes — the store self-corrects on POSITIVE evidence. A
// failure does not say which of its parts was at fault; a success does.
func TestPromoteSuccessSupersedes(t *testing.T) {
	ps, _, _ := Promote(seed(t, []ev{
		{day: 5, episode: "ep-a", argv: []string{"make", "install"}, exit: 2},
		{day: 4, episode: "ep-b", argv: []string{"make", "install"}, exit: 2},
		{day: 3, episode: "ep-c", argv: []string{"make", "install"}, exit: 2},
		{day: 1, episode: "ep-d", argv: []string{"make", "install"}, exit: 0},
	}), PromoteDefaults())
	if len(ps) != 0 {
		t.Errorf("a more recent success must close the claim: %+v", ps)
	}
}

// TestBenignNeverPromotes — grep finding nothing is 40% of every exit-1 in a
// real corpus. Admitting these yields "grep is unreliable here": false, and
// confident.
func TestPromoteBenignNeverPromotes(t *testing.T) {
	ps, _, _ := Promote(seed(t, []ev{
		{day: 4, episode: "ep-a", argv: []string{"grep", "needle", "f"}, exit: 1, benign: true},
		{day: 3, episode: "ep-b", argv: []string{"grep", "needle", "f"}, exit: 1, benign: true},
		{day: 1, episode: "ep-c", argv: []string{"grep", "needle", "f"}, exit: 1, benign: true},
	}), PromoteDefaults())
	if len(ps) != 0 {
		t.Errorf("benign exits must never promote: %+v", ps)
	}
}

// TestOpaqueNeverPromotes — `make test` fails in a child this process never
// saw, so blaming `make test` blames the one thing that was not measured.
func TestPromoteOpaqueNeverPromotes(t *testing.T) {
	ps, _, _ := Promote(seed(t, []ev{
		{day: 4, episode: "ep-a", argv: []string{"make", "test"}, exit: 2, opaque: true},
		{day: 3, episode: "ep-b", argv: []string{"make", "test"}, exit: 2, opaque: true},
		{day: 1, episode: "ep-c", argv: []string{"make", "test"}, exit: 2, opaque: true},
	}), PromoteDefaults())
	if len(ps) != 0 {
		t.Errorf("opaque commands must never promote: %+v", ps)
	}
}

// TestEvidenceIsCarried — a reader must be able to disagree with the threshold,
// not just with the verdict.
func TestPromoteEvidenceIsCarried(t *testing.T) {
	ps, _, _ := Promote(seed(t, []ev{
		{day: 9, episode: "ep-x", argv: []string{"ssh", "host"}, exit: 0},
		{day: 4, episode: "ep-a", argv: []string{"ssh", "host"}, exit: 255, dim: "network"},
		{day: 3, episode: "ep-b", argv: []string{"ssh", "host"}, exit: 255, dim: "network"},
		{day: 1, episode: "ep-c", argv: []string{"ssh", "host"}, exit: 255, dim: "network"},
	}), PromoteDefaults())
	p := only(t, ps, "ssh host")
	if p.Successes != 1 {
		t.Errorf("the earlier success must still be reported, got %d", p.Successes)
	}
	if p.LastSuccess.IsZero() {
		t.Error("LastSuccess must be carried so a reader sees it used to work")
	}
	if p.FirstSeen.IsZero() || p.LastSeen.IsZero() {
		t.Errorf("the failure window must be reported: %+v", p)
	}
}

// TestUnobservedIsNotEvidence — it may never have run.
func TestPromoteUnobservedIsNotEvidence(t *testing.T) {
	root := t.TempDir()
	w := Open(root)
	now := time.Now().UTC()
	for i, d := range []int{4, 3, 1} {
		body := Scrub(nil, []string{"ssh", "host"}, "/w", TemplateOpts{})
		_ = w.Append(Record{
			At:  now.AddDate(0, 0, -d).Add(time.Duration(i) * time.Minute),
			Cmd: "ssh", PID: 1, Observed: false,
		}, body, "ep-"+string(rune('a'+i)))
	}
	_ = w.Close()

	ps, _, _ := Promote(root, PromoteDefaults())
	if len(ps) != 0 {
		t.Errorf("unobserved records are not evidence of failure: %+v", ps)
	}
}

func TestExitClass(t *testing.T) {
	cases := map[int]string{
		1: "generic", 2: "generic", 126: "not-executable", 127: "not-found",
		137: "signal:9", 143: "signal:15",
	}
	for in, want := range cases {
		if got := exitClass(in); got != want {
			t.Errorf("exitClass(%d) = %q want %q", in, got, want)
		}
	}
}
