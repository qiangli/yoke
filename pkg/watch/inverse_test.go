package watch

import (
	"testing"
	"time"
)

// The inverse operator is the one rule a hand-written poll loop cannot express:
// it fires because NOTHING arrived. Every test here runs on a fake clock — the
// engine never sleeps to find out what time it is.

func TestInverseIdleFiresWhileStalledAndClearsOnResume(t *testing.T) {
	r := NewInverse(Inverse{RuleName: "idle", Within: 14 * time.Minute})
	r.Start(at0)

	if got := r.On(Event{At: at0, Source: "run", Locator: "run:repo-7", Kind: KindChanged, Field: "stdout_bytes"}, at0); len(got) != 0 {
		t.Fatalf("activity itself is not an alert: %+v", got)
	}
	if got := r.Tick(at0.Add(13 * time.Minute)); len(got) != 0 {
		t.Fatalf("fired inside the window: %+v", got)
	}

	fire := r.Tick(at0.Add(14 * time.Minute))
	if len(fire) != 1 || fire[0].State != StateFire || fire[0].Reason != ReasonIdle || fire[0].Locator != "run:repo-7" {
		t.Fatalf("alerts = %+v", fire)
	}
	if fire[0].Fields["idle_for"] != "14m0s" {
		t.Fatalf("idle_for = %v, want 14m0s", fire[0].Fields["idle_for"])
	}

	// A persisting condition keeps being reported: dedupe and re-notify are the
	// ENGINE's policy, not the rule's, so a rule that went quiet would take the
	// choice away from it.
	if again := r.Tick(at0.Add(20 * time.Minute)); len(again) != 1 || again[0].State != StateFire {
		t.Fatalf("alerts = %+v, want the condition restated", again)
	}

	resume := r.On(Event{At: at0.Add(21 * time.Minute), Locator: "run:repo-7", Kind: KindChanged, Field: "stdout_bytes"}, at0.Add(21*time.Minute))
	if len(resume) != 1 || resume[0].State != StateClear || resume[0].Fields["idle_for"] != "21m0s" {
		t.Fatalf("un-stall = %+v, want a clear carrying the idle duration", resume)
	}
	if got := r.Tick(at0.Add(22 * time.Minute)); len(got) != 0 {
		t.Fatalf("fired after resume inside the window: %+v", got)
	}
}

func TestInverseLivenessFiresForAHeartbeatThatNeverArrives(t *testing.T) {
	// The case absence-as-silence misses entirely: the locator produced NOTHING,
	// ever, so there is no event to hang a timer on but the expectation itself.
	r := NewInverse(Inverse{
		RuleName: "heartbeat",
		Within:   5 * time.Minute,
		Reason:   ReasonMissing,
		Expect:   []string{"lease:sprint-480", "seat:manager"},
	})
	r.Start(at0)
	if got := r.Tick(at0.Add(4 * time.Minute)); len(got) != 0 {
		t.Fatalf("fired early: %+v", got)
	}
	// One of the two reports in time; the other never does.
	r.On(Event{At: at0.Add(4 * time.Minute), Locator: "seat:manager", Kind: KindSample}, at0.Add(4*time.Minute))
	fire := r.Tick(at0.Add(5 * time.Minute))
	if len(fire) != 1 || fire[0].Locator != "lease:sprint-480" || fire[0].Reason != ReasonMissing {
		t.Fatalf("alerts = %+v, want only the silent locator", fire)
	}
}

func TestInverseActivityPredicateSelectsWhatCounts(t *testing.T) {
	// "Alive but idle is stalled, never healthy": a coach nudge or a status
	// sample is not progress, so it must not reset the idle clock.
	r := NewInverse(Inverse{
		RuleName: "idle",
		Within:   10 * time.Minute,
		Activity: func(ev Event) bool { return ev.Field == "stdout_bytes" || ev.Field == "stdin_bytes" },
		Expect:   []string{"seat:worker"},
	})
	r.Start(at0)
	r.On(Event{At: at0.Add(5 * time.Minute), Locator: "seat:worker", Kind: KindSample, Field: "pid_alive"}, at0.Add(5*time.Minute))
	fire := r.Tick(at0.Add(10 * time.Minute))
	if len(fire) != 1 {
		t.Fatalf("alerts = %+v, want the liveness sample not to count as progress", fire)
	}
	if fire[0].Fields["idle_for"] != "10m0s" {
		t.Fatalf("idle_for = %v, want the clock never to have been reset", fire[0].Fields["idle_for"])
	}
}

func TestInverseLocatorOfGroupsActivity(t *testing.T) {
	r := NewInverse(Inverse{
		RuleName:  "quiet-source",
		Within:    time.Minute,
		Reason:    ReasonAbsent,
		LocatorOf: func(ev Event) string { return ev.Source },
	})
	r.Start(at0)
	r.On(Event{At: at0, Source: "mb", Locator: "mb:12"}, at0)
	fire := r.Tick(at0.Add(time.Minute))
	if len(fire) != 1 || fire[0].Locator != "mb" || fire[0].Reason != ReasonAbsent {
		t.Fatalf("alerts = %+v, want absence reported for the source", fire)
	}
}

func TestInverseArmsLocatorsASourceExpects(t *testing.T) {
	// A poll source pointed at one resource knows its locator before the first
	// cycle, so "this log has not moved since I started watching" is reportable
	// without the rule file having to name it.
	r := NewInverse(Inverse{RuleName: "idle", Within: 14 * time.Minute})
	r.Arm([]string{"run:repo-7"})
	r.Start(at0)
	fire := r.Tick(at0.Add(14 * time.Minute))
	if len(fire) != 1 || fire[0].Locator != "run:repo-7" {
		t.Fatalf("alerts = %+v, want the armed locator reported", fire)
	}
}

func TestInverseScopeKeepsUnrelatedLocatorsOut(t *testing.T) {
	// Without a scope, a rule asking "has this sprint thread gone quiet?" would
	// start a timer on every message id another source mentions, and alarm
	// about all of them 14 minutes later.
	r := NewInverse(Inverse{
		RuleName: "sprint-idle",
		Within:   14 * time.Minute,
		Scope:    func(ev Event) bool { return ev.Source == "sprint-thread" },
		Activity: func(ev Event) bool { return ev.Source == "sprint-thread" },
	})
	r.Start(at0)
	r.On(Event{At: at0, Source: "sprint-thread", Locator: "sprint:480"}, at0)
	r.On(Event{At: at0, Source: "mb", Locator: "mb:m1"}, at0)
	fire := r.Tick(at0.Add(14 * time.Minute))
	if len(fire) != 1 || fire[0].Locator != "sprint:480" {
		t.Fatalf("alerts = %+v, want only the in-scope locator", fire)
	}
}
