package weave

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func healthTestItem() *weaveItem {
	return &weaveItem{
		ID: 7, State: "working", Owner: "agent-7", Tool: "codex",
		Workspace: "/workspace/7", StartedAt: time.Unix(100, 0).UTC(),
		LaunchSpec: &weaveLaunchSpec{Tool: "codex", MaxRuntime: 30 * time.Minute},
		WrapperPid: 42,
	}
}

func healthTestProbe(now time.Time, alive bool) weaveHealthProbe {
	return weaveHealthProbe{
		Now: now, PIDAlive: func(int) bool { return alive },
		WorkspaceExists: func(string) bool { return true },
	}
}

func TestWeaveHealthDeadProcessIsStale(t *testing.T) {
	it := healthTestItem()
	now := time.Unix(200, 0).UTC()
	s := weaveHealthSnapshotFor(it, healthTestProbe(now, false))
	got := weaveClassifyHealth(s, it, now)
	if got.Health != weaveHealthStale || !strings.Contains(got.Next, "--resume") {
		t.Fatalf("dead process health = %+v, want stale + resume action", got)
	}
}

func TestWeaveHealthFreshLeaseWithoutProgressIsIdle(t *testing.T) {
	it := healthTestItem()
	now := it.StartedAt.Add(time.Minute)
	s := weaveHealthSnapshotFor(it, healthTestProbe(now, true))
	got := weaveClassifyHealth(s, it, now)
	if got.Health != weaveHealthIdle || !strings.Contains(got.Reason, "recorded no progress") {
		t.Fatalf("fresh no-progress health = %+v, want idle", got)
	}
}

func TestWeaveHealthOverDeadlineIsWedged(t *testing.T) {
	it := healthTestItem()
	now := it.StartedAt.Add(31 * time.Minute)
	it.Comments = []weaveComment{{At: now.Add(-time.Minute), Kind: "progress", Body: "still working"}}
	s := weaveHealthSnapshotFor(it, healthTestProbe(now, true))
	got := weaveClassifyHealth(s, it, now)
	if got.Health != weaveHealthWedged || !strings.Contains(got.Reason, "deadline") {
		t.Fatalf("over-deadline health = %+v, want wedged", got)
	}
}

func TestWeaveHealthContradictoryOwnerSpecIsInconsistent(t *testing.T) {
	it := healthTestItem()
	it.LaunchSpec.Tool = "claude"
	now := it.StartedAt.Add(time.Minute)
	s := weaveHealthSnapshotFor(it, healthTestProbe(now, true))
	got := weaveClassifyHealth(s, it, now)
	if got.Health != weaveHealthInconsistent || !strings.Contains(got.Reason, "contradict") {
		t.Fatalf("contradictory record health = %+v, want inconsistent", got)
	}
}

func TestWeaveHealthSubmittedRequiresCommitAndTestEvidence(t *testing.T) {
	now := time.Unix(200, 0).UTC()
	for _, tc := range []struct {
		name string
		item *weaveItem
		want weaveHealth
	}{
		{"missing commit", &weaveItem{ID: 1, State: "submitted", FinishedAt: now}, weaveHealthInconsistent},
		{"missing test", &weaveItem{ID: 2, State: "submitted", FinishedAt: now, CommitsAhead: 1, VerifyCommand: "go test ./..."}, weaveHealthInconsistent},
		{"coherent", &weaveItem{ID: 3, State: "submitted", FinishedAt: now, CommitsAhead: 1, VerifyCommand: "go test ./...", VerifyExit: intPtr(0)}, weaveHealthHealthy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := weaveHealthSnapshotFor(tc.item, healthTestProbe(now, false))
			if got := weaveClassifyHealth(s, tc.item, now).Health; got != tc.want {
				t.Fatalf("health = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWeaveHealthTerminalFailureWithEvidenceIsCoherent(t *testing.T) {
	now := time.Unix(200, 0).UTC()
	it := &weaveItem{ID: 4, State: "failed", FinishedAt: now, ExitCode: intPtr(1)}
	s := weaveHealthSnapshotFor(it, healthTestProbe(now, false))
	if got := weaveClassifyHealth(s, it, now).Health; got != weaveHealthHealthy {
		t.Fatalf("recorded failed terminal health = %q, want healthy supervision state", got)
	}
}

func TestWeaveHealthTerminalStateWithoutEvidenceIsInconsistent(t *testing.T) {
	now := time.Unix(200, 0).UTC()
	it := &weaveItem{ID: 5, State: "failed"}
	s := weaveHealthSnapshotFor(it, healthTestProbe(now, false))
	if got := weaveClassifyHealth(s, it, now).Health; got != weaveHealthInconsistent {
		t.Fatalf("evidence-free terminal health = %q, want inconsistent", got)
	}
}

func TestWeaveHealthJSONAndHumanUseSameVerdict(t *testing.T) {
	now := time.Unix(200, 0).UTC()
	it := healthTestItem()
	s := weaveHealthSnapshotFor(it, healthTestProbe(now, false))
	report := weaveClassifyHealth(s, it, now)
	blob, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"health":"stale"`) || !strings.Contains(string(blob), report.Next) {
		t.Fatalf("JSON report does not carry the same verdict/action: %s", blob)
	}
	if report.Health != weaveHealthStale || report.Reason == "" {
		t.Fatalf("human report lost verdict: %+v", report)
	}
}

func TestWeaveHealthSnapshotJSONOmitsAbsentTimestamps(t *testing.T) {
	zero, err := json.Marshal(weaveHealthSnapshot{Issue: 1, State: "working"})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"started_at", "last_progress_at", "deadline_at", "finished_at"} {
		if strings.Contains(string(zero), `"`+field+`"`) {
			t.Fatalf("zero snapshot serialized absent %s: %s", field, zero)
		}
	}
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	populated, err := json.Marshal(weaveHealthSnapshot{
		Issue:          1,
		State:          "working",
		StartedAt:      now,
		LastProgressAt: now.Add(time.Minute),
		DeadlineAt:     now.Add(time.Hour),
		FinishedAt:     now.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"started_at", "last_progress_at", "deadline_at", "finished_at"} {
		if !strings.Contains(string(populated), `"`+field+`"`) {
			t.Fatalf("populated snapshot omitted %s: %s", field, populated)
		}
	}
}

func TestWeaveHealthReadDoesNotMutateQueue(t *testing.T) {
	now := time.Unix(200, 0).UTC()
	it := healthTestItem()
	q := &weaveQueue{Items: []*weaveItem{it}}
	before, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	s := weaveHealthSnapshotFor(it, healthTestProbe(now, false))
	_ = weaveClassifyHealth(s, it, now)
	after, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("health read mutated queue:\nbefore %s\nafter %s", before, after)
	}
}

func TestWeaveStatusReportsStaleWithoutMutatingQueue(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	dir, _ := weaveQueueDir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID: 1, Title: "stalled worker", State: "working", Owner: "agent-1", Tool: "codex",
		Workspace: root, WrapperPid: 2147483647, StartedAt: time.Now().UTC().Add(-time.Minute),
		LaunchSpec: &weaveLaunchSpec{Tool: "codex", MaxRuntime: 30 * time.Minute},
	}}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(dir + "/queue.json")
	if err != nil {
		t.Fatal(err)
	}
	out, code := runWeave(t, "status", "1", "--json")
	if code != 0 || !strings.Contains(out, `"health": "stale"`) || !strings.Contains(out, `"state": "working"`) {
		t.Fatalf("JSON status = exit %d %s, want stale", code, out)
	}
	after, err := os.ReadFile(dir + "/queue.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("status mutated queue; repair belongs to weave doctor/heartbeat")
	}
	out, code = runWeave(t, "status", "1", "--plain")
	if code != 0 || !strings.Contains(out, "health:   stale") {
		t.Fatalf("second human status = exit %d %s, want stable stale observation", code, out)
	}
}

func intPtr(n int) *int { return &n }
