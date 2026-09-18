package resources

import (
	"context"
	"strings"
	"testing"
	"time"
)

func fleetWith(total, busy, idle int, idleAgents ...IdleAgent) *FleetResources {
	return &FleetResources{
		Totals:     FleetTotals{Total: total, Busy: busy, Idle: idle},
		IdleAgents: idleAgents,
	}
}

// The three cases ARE the invariant. Each one asserts the verdict, that the
// notification fires only on the transition into UNDER-UTILIZED, and — for
// the violation — that the verdict names the work and the agent.
func TestUtilizationThreeCases(t *testing.T) {
	at := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	pending := PendingWork{
		Todo: 1, Submitted: 1,
		Items: []PendingItem{
			{Kind: "todo", ID: "t-1", Label: "wire the banner"},
			{Kind: "submitted", ID: "8", Label: "merge me", Band: 4},
		},
	}

	t.Run("case1_under_utilized", func(t *testing.T) {
		fr := fleetWith(2, 1, 1, IdleAgent{Name: "sol", Tool: "codex", Band: 4})
		u := EvaluateUtilization(at, pending, fr)
		if u.Verdict != VerdictUnderUtilized {
			t.Fatalf("verdict = %s, want %s (%s)", u.Verdict, VerdictUnderUtilized, u.Reason)
		}
		if len(u.Assignments) != 1 || u.Assignments[0].Agent.Name != "sol" || u.Assignments[0].Item.ID != "8" {
			t.Fatalf("assignments = %+v, want submitted 8 -> sol", u.Assignments)
		}
		if !strings.Contains(u.Reason, "2 issue(s) pending") || !strings.Contains(u.Reason, "sol") {
			t.Fatalf("reason must name pending count and idle agent: %q", u.Reason)
		}
		var fired int
		w := &UtilizationWatcher{Notify: func(*Utilization) { fired++ }}
		if !w.Observe(u) {
			t.Fatal("first under-utilized reading must be a transition")
		}
		if w.Observe(u) {
			t.Fatal("second identical reading must NOT re-notify")
		}
		if fired != 1 {
			t.Fatalf("notify fired %d times, want exactly 1", fired)
		}
		t.Logf("case1 verdict=%s banner=%s", u.Verdict, u.Banner())
	})

	t.Run("case2_optimal_when_idle_but_no_pending", func(t *testing.T) {
		fr := fleetWith(2, 0, 2, IdleAgent{Name: "sol", Band: 4}, IdleAgent{Name: "ada", Band: 2})
		u := EvaluateUtilization(at, PendingWork{}, fr)
		if u.Verdict != VerdictOptimal {
			t.Fatalf("verdict = %s, want %s", u.Verdict, VerdictOptimal)
		}
		if len(u.Assignments) != 0 {
			t.Fatalf("no pending work must yield no assignments: %+v", u.Assignments)
		}
		w := &UtilizationWatcher{}
		if w.Observe(u) {
			t.Fatal("OPTIMAL must not notify")
		}
		t.Logf("case2 verdict=%s banner=%s", u.Verdict, u.Banner())
	})

	t.Run("case3_saturated_not_a_failure", func(t *testing.T) {
		fr := fleetWith(2, 2, 0)
		u := EvaluateUtilization(at, pending, fr)
		if u.Verdict != VerdictSaturated {
			t.Fatalf("verdict = %s, want %s", u.Verdict, VerdictSaturated)
		}
		if u.UnderUtilized() {
			t.Fatal("SATURATED must not be reported as an invariant violation")
		}
		if !strings.Contains(u.Reason, "waiting on compute") {
			t.Fatalf("reason must read as waiting on compute: %q", u.Reason)
		}
		w := &UtilizationWatcher{}
		if w.Observe(u) {
			t.Fatal("SATURATED must not notify")
		}
		t.Logf("case3 verdict=%s banner=%s", u.Verdict, u.Banner())
	})
}

// Idle capacity that cannot serve the pending band is saturation, not waste.
func TestUtilizationBandMismatchIsSaturated(t *testing.T) {
	u := EvaluateUtilization(time.Time{},
		PendingWork{Submitted: 1, Items: []PendingItem{{Kind: "submitted", ID: "9", Band: 5}}},
		fleetWith(1, 0, 1, IdleAgent{Name: "tiny", Band: 1}))
	if u.Verdict != VerdictSaturated || !strings.Contains(u.Reason, "none band-appropriate") {
		t.Fatalf("verdict = %s (%s), want SATURATED for a band mismatch", u.Verdict, u.Reason)
	}
}

func TestUtilizationWatcherRenotifiesAfterRecovery(t *testing.T) {
	under := EvaluateUtilization(time.Time{},
		PendingWork{Todo: 1, Items: []PendingItem{{Kind: "todo", ID: "t"}}},
		fleetWith(1, 0, 1, IdleAgent{Name: "sol", Band: 4}))
	optimal := EvaluateUtilization(time.Time{}, PendingWork{}, fleetWith(1, 0, 1))
	var fired int
	w := &UtilizationWatcher{Notify: func(*Utilization) { fired++ }}
	for _, u := range []*Utilization{under, under, optimal, under} {
		w.Observe(u)
	}
	if fired != 2 {
		t.Fatalf("notify fired %d times, want 2 (one per entry into UNDER-UTILIZED)", fired)
	}
}

func TestEvaluateOnceJoinsBothProviders(t *testing.T) {
	u, err := EvaluateOnce(context.Background(),
		func(context.Context) (PendingWork, error) {
			return PendingWork{Todo: 1, Items: []PendingItem{{Kind: "todo", ID: "t-1"}}}, nil
		},
		func(context.Context) (*FleetResources, error) {
			return fleetWith(1, 0, 1, IdleAgent{Name: "sol", Band: 4}), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if u.Verdict != VerdictUnderUtilized || u.SchemaVersion != UtilizationSchemaVersion {
		t.Fatalf("EvaluateOnce = %s / %s", u.Verdict, u.SchemaVersion)
	}
}

// The command is the steward's actual front door, so it is exercised as a
// command: real cobra wiring, real flag defaults, real emitted line.
func TestUtilizationCommandPrintsVerdictAndNotifiesOnce(t *testing.T) {
	cmd := NewUtilizationCommand(func(context.Context) (PendingWork, error) { return PendingWork{}, nil })
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "UTILIZATION "+VerdictOptimal) {
		t.Fatalf("command output = %q", out.String())
	}
	t.Logf("live command output: %s", strings.TrimSpace(out.String()))
}
