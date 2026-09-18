// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package execlog

import (
	"testing"
	"time"
)

type step struct {
	episode string
	pid     int
	argv    []string
	exit    int
	benign  bool
}

func seedSteps(t *testing.T, steps []step) string {
	t.Helper()
	root := t.TempDir()
	w := Open(root)
	defer w.Close()

	now := time.Now().UTC()
	// Seq is per-Writer, so a single writer gives each record an ascending seq
	// exactly as production does.
	for i, s := range steps {
		exit := s.exit
		body := Scrub(nil, s.argv, "/w", TemplateOpts{})
		if err := w.Append(Record{
			At: now.Add(time.Duration(i) * time.Second), Cmd: s.argv[0],
			PID: s.pid, Observed: true, Exit: &exit, Benign: s.benign,
		}, body, s.episode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func edge(t *testing.T, ts []Transition, src, dst string) Transition {
	t.Helper()
	for _, x := range ts {
		if x.Src == src && x.Dst == dst {
			return x
		}
	}
	t.Fatalf("no transition %q -> %q; got %+v", src, dst, ts)
	return Transition{}
}

func TestTransitionsCountBigrams(t *testing.T) {
	root := seedSteps(t, []step{
		{episode: "ep-a", pid: 1, argv: []string{"go", "build", "./..."}},
		{episode: "ep-a", pid: 1, argv: []string{"go", "test", "./..."}},
		{episode: "ep-a", pid: 1, argv: []string{"go", "build", "./..."}},
		{episode: "ep-a", pid: 1, argv: []string{"go", "test", "./..."}},
	})
	ts, _, err := Transitions(root, Query{})
	if err != nil {
		t.Fatal(err)
	}
	e := edge(t, ts, "go build ./...", "go test ./...")
	if e.N != 2 {
		t.Errorf("want the bigram counted twice as ONE edge, got n=%d", e.N)
	}
	if e.OK != 2 {
		t.Errorf("want ok=2, got %d", e.OK)
	}
}

// TestNeverBridgesConcurrentShells is the rule that keeps this from inventing
// causality. BASHY_EPISODE is inherited, so two shells share an episode and
// interleave in one file — and a chain across that merge looks entirely
// plausible while being pure fiction.
func TestTransitionsNeverBridgeConcurrentShells(t *testing.T) {
	root := seedSteps(t, []step{
		{episode: "ep-a", pid: 1, argv: []string{"go", "build", "./..."}},
		{episode: "ep-a", pid: 2, argv: []string{"npm", "install"}},
		{episode: "ep-a", pid: 1, argv: []string{"go", "test", "./..."}},
	})
	ts, _, _ := Transitions(root, Query{})
	for _, e := range ts {
		if e.Src == "go build ./..." && e.Dst == "npm install" {
			t.Errorf("bridged two concurrent shells: %+v", e)
		}
		if e.Src == "npm install" && e.Dst == "go test ./..." {
			t.Errorf("bridged two concurrent shells: %+v", e)
		}
	}
}

// TestNeverBridgesEpisodes — two sessions have no sequential relationship.
func TestTransitionsNeverBridgeEpisodes(t *testing.T) {
	root := seedSteps(t, []step{
		{episode: "ep-a", pid: 1, argv: []string{"go", "build", "./..."}},
		{episode: "ep-b", pid: 1, argv: []string{"go", "test", "./..."}},
	})
	ts, _, _ := Transitions(root, Query{})
	if len(ts) != 0 {
		t.Errorf("no transition may cross an episode boundary: %+v", ts)
	}
}

// TestRecoveredIsTheRemediationSignal — "when X breaks, Y fixes it" is the one
// transition worth more than its frequency.
func TestTransitionsRecovered(t *testing.T) {
	root := seedSteps(t, []step{
		{episode: "ep-a", pid: 1, argv: []string{"go", "build", "./..."}, exit: 1},
		{episode: "ep-a", pid: 1, argv: []string{"go", "mod", "tidy"}},
		{episode: "ep-a", pid: 1, argv: []string{"go", "build", "./..."}, exit: 1},
		{episode: "ep-a", pid: 1, argv: []string{"go", "mod", "tidy"}},
	})
	ts, _, _ := Transitions(root, Query{})
	e := edge(t, ts, "go build ./...", "go mod tidy")
	if e.Recovered != 2 {
		t.Errorf("want 2 recoveries, got %d (%+v)", e.Recovered, e)
	}
}

// TestBenignIsNotABreak — recovering from `grep` finding nothing is not a
// remediation, and counting it would fill the recovery signal with noise.
func TestTransitionsBenignIsNotABreak(t *testing.T) {
	root := seedSteps(t, []step{
		{episode: "ep-a", pid: 1, argv: []string{"grep", "needle", "f"}, exit: 1, benign: true},
		{episode: "ep-a", pid: 1, argv: []string{"echo", "ok"}},
	})
	ts, _, _ := Transitions(root, Query{})
	e := edge(t, ts, "grep needle f", "echo ok")
	if e.Recovered != 0 {
		t.Errorf("a benign exit is not a break to recover from: %+v", e)
	}
}

// TestGapIsNotAnAdjacency — a hole in the sequence means records died before
// flush. Bridging it invents an adjacency nobody observed.
func TestTransitionsGapIsNotAnAdjacency(t *testing.T) {
	root := t.TempDir()
	w := Open(root)
	now := time.Now().UTC()
	mk := func(seq uint64, argv []string) Record {
		exit := 0
		return Record{At: now, Cmd: argv[0], PID: 1, Seq: seq, Observed: true, Exit: &exit}
	}
	_ = mk // Seq is assigned by Append; the gap is produced by writing directly.
	_ = w.Close()

	// Write two records with a deliberate hole between their sequences.
	day := time.Now().UTC().Format("2006-01-02")
	writeRaw(t, root, day, "ep-a", []string{
		`{"schema":"bashy-execlog-v1","episode":"ep-a","pid":1,"seq":1,"cmd":"go","template":"go build ./...","observed":true,"exit":0}`,
		`{"schema":"bashy-execlog-v1","episode":"ep-a","pid":1,"seq":9,"cmd":"go","template":"go test ./...","observed":true,"exit":0}`,
	})

	ts, cov, _ := Transitions(root, Query{})
	if len(ts) != 0 {
		t.Errorf("a sequence gap must not become an adjacency: %+v", ts)
	}
	if cov.Lost == 0 {
		t.Error("the gap must be reported as lost records")
	}
}

// TestSelfTransitionIsNotARecovery — re-running the same thing until it works
// is a RETRY. Calling it a recovery renders "when X breaks, X fixes it", which
// is useless and confidently stated.
//
// It fires more than it looks like it should: the canonicaliser collapses
// absolute paths outside the repo, so `ls /nope` and `ls /etc/hosts` are one
// template. That collapse is deliberate, and this is its cost.
func TestTransitionsSelfIsNotARecovery(t *testing.T) {
	root := seedSteps(t, []step{
		{episode: "ep-a", pid: 1, argv: []string{"ls", "/nope"}, exit: 2},
		{episode: "ep-a", pid: 1, argv: []string{"ls", "/etc/hosts"}},
	})
	ts, _, _ := Transitions(root, Query{})
	for _, e := range ts {
		if e.Src == e.Dst && e.Recovered > 0 {
			t.Errorf("a template must not recover from itself: %+v", e)
		}
	}
}
