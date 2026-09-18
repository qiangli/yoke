package board

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/resources"
)

func boardWith(t *testing.T, runs []Run, todos []Todo, agents []Agent) *Board {
	t.Helper()
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	b, err := Collect(context.Background(), Options{Now: now}, []Source{SourceFunc{SourceName: "fixture",
		Func: func(_ context.Context, b *Board, _ Options) error {
			b.Runs, b.Todos, b.Agents = runs, todos, agents
			return nil
		}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The board is the work half of the invariant, so the three cases are
// asserted again in board terms — the banner is what a steward actually reads.
func TestBoardUtilizationBannerThreeCases(t *testing.T) {
	idle := []Agent{{Name: "sol", Tool: "codex", Model: "gpt-5.6-sol", Band: 4, Available: true, Found: true, Availability: "available", State: "idle"}}
	busy := []Agent{{Name: "sol", Tool: "codex", Model: "gpt-5.6-sol", Band: 4, Available: true, Found: true, Availability: "available", State: "working"}}
	pendingRun := Run{ID: 8, Label: "merge me", Repo: "bashy", State: "submitted", Tool: "claude", Band: 4}
	busyRun := Run{ID: 7, Label: "in flight", Repo: "coreutils", State: "working", Tool: "codex", Agent: "sol", Model: "gpt-5.6-sol", Band: 4}

	t.Run("case1_pending_plus_free_agent_is_under_utilized", func(t *testing.T) {
		b := boardWith(t, []Run{pendingRun}, nil, idle)
		u := b.Utilization
		if u == nil || u.Verdict != resources.VerdictUnderUtilized {
			t.Fatalf("verdict = %v, want UNDER-UTILIZED", u)
		}
		if len(u.Assignments) == 0 || u.Assignments[0].Agent.Name != "sol" {
			t.Fatalf("verdict must name the idle agent that could take the work: %+v", u.Assignments)
		}
		var fired int
		w := &resources.UtilizationWatcher{Notify: func(*resources.Utilization) { fired++ }}
		if !w.Observe(u) || fired != 1 {
			t.Fatalf("transition into UNDER-UTILIZED must notify exactly once (fired=%d)", fired)
		}
		text, err := (TerminalRenderer{}).Render(b, Options{})
		if err != nil || !strings.Contains(string(text), "UTILIZATION UNDER-UTILIZED") {
			t.Fatalf("banner missing from board render: err=%v\n%s", err, text)
		}
		t.Logf("case1 banner=%s", u.Banner())
	})

	t.Run("case2_no_pending_is_optimal_even_when_idle", func(t *testing.T) {
		b := boardWith(t, []Run{{ID: 1, Label: "merged", Repo: "bashy", State: "done"}}, nil, idle)
		u := b.Utilization
		if u == nil || u.Verdict != resources.VerdictOptimal {
			t.Fatalf("verdict = %v, want OPTIMAL", u)
		}
		if u.Capacity.Idle == 0 {
			t.Fatal("fixture must actually have idle capacity for this case to mean anything")
		}
		t.Logf("case2 banner=%s", u.Banner())
	})

	t.Run("case3_pending_plus_all_busy_is_saturated", func(t *testing.T) {
		b := boardWith(t, []Run{pendingRun, busyRun}, nil, busy)
		u := b.Utilization
		if u == nil || u.Verdict != resources.VerdictSaturated {
			t.Fatalf("verdict = %v, want SATURATED", u)
		}
		if u.UnderUtilized() {
			t.Fatal("SATURATED must not be flagged as a failure")
		}
		t.Logf("case3 banner=%s", u.Banner())
	})
}

func TestBoardPendingItemsCountsThreeBuckets(t *testing.T) {
	b := boardWith(t,
		[]Run{
			{ID: 8, State: "submitted", Band: 4},
			{ID: 9, State: "killed", Salvageable: true, UnmergedCommits: 2},
			{ID: 10, State: "done"},
		},
		[]Todo{{ID: "a", Title: "open", Status: "todo"}, {ID: "b", Title: "closed", Status: "done"}},
		nil)
	got := b.PendingItems()
	if got.Todo != 1 || got.Submitted != 1 || got.Salvageable != 1 || got.Total() != 3 {
		t.Fatalf("pending = %+v, want 1 todo + 1 submitted + 1 salvageable", got)
	}
}
