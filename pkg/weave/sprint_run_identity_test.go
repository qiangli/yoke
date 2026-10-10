package weave

import (
	"testing"
	"time"
)

// A queue reset recycles run numbers. The closed sprint's links name the
// RETIRED generation; the new runs must not be attributed to it, while the
// historical runs it really linked stay attributed.
func TestSprintForRunReusedIDAfterQueueReset(t *testing.T) {
	old := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	reborn := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	links := []SprintRunLink{
		{Sprint: 331, Done: true, UpdatedAt: old.Add(time.Hour), Repo: "yoke", ID: 37, Queue: "yoke-abc", Born: old},
		{Sprint: 331, Done: true, UpdatedAt: old.Add(time.Hour), Repo: "agent-bench", ID: 2, Queue: "agent-bench-1", Born: old},
	}
	if got := SprintForRun(links, "yoke", 37, "yoke-abc", reborn); got != 0 {
		t.Fatalf("reused yoke#37 attributed to closed sprint #%d", got)
	}
	if got := SprintForRun(links, "agent-bench", 2, "agent-bench-1", reborn); got != 0 {
		t.Fatalf("reused agent-bench#2 attributed to closed sprint #%d", got)
	}
	// History is not erased: the retired generation still reads as #331.
	if got := SprintForRun(links, "yoke", 37, "yoke-abc", old); got != 331 {
		t.Fatalf("historical yoke#37 = sprint #%d, want 331", got)
	}

	// The workaround link to the current sprint wins for the new generation.
	links = append(links, SprintRunLink{Sprint: 329, UpdatedAt: reborn.Add(time.Minute), Repo: "yoke", ID: 37, Queue: "yoke-abc", Born: reborn})
	if got := SprintForRun(links, "yoke", 37, "yoke-abc", reborn); got != 329 {
		t.Fatalf("new yoke#37 = sprint #%d, want 329", got)
	}
	if got := SprintForRun(links, "yoke", 37, "yoke-abc", old); got != 331 {
		t.Fatalf("historical yoke#37 = sprint #%d after relink, want 331", got)
	}
	// A same-numbered run in another checkout is a different run.
	if got := SprintForRun(links, "yoke", 37, "yoke-other", reborn); got != 0 {
		t.Fatalf("other-queue yoke#37 = sprint #%d, want 0", got)
	}
}

// Links written before Born existed stay honoured (backward compatibility),
// except when the card provably predates the run.
func TestSprintForRunLegacyLinks(t *testing.T) {
	linkedAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	legacy := []SprintRunLink{{Sprint: 200, Done: true, UpdatedAt: linkedAt, Repo: "yoke", ID: 5}}
	if got := SprintForRun(legacy, "yoke", 5, "yoke-abc", linkedAt.Add(-time.Hour)); got != 200 {
		t.Fatalf("legacy link to its own run = %d, want 200", got)
	}
	if got := SprintForRun(legacy, "yoke", 5, "yoke-abc", linkedAt.Add(time.Hour)); got != 0 {
		t.Fatalf("legacy link from a card untouched since before the run = %d, want 0", got)
	}
	if got := SprintForRun(legacy, "yoke", 5, "", time.Time{}); got != 200 {
		t.Fatalf("legacy link with unknown run generation = %d, want 200", got)
	}
	// A generation-proven link outranks a legacy one; two live legacy
	// claimants are ambiguous and attribute nothing rather than guess.
	born := linkedAt.Add(-time.Hour)
	both := append(legacy, SprintRunLink{Sprint: 210, UpdatedAt: linkedAt, Repo: "yoke", ID: 5, Born: born})
	if got := SprintForRun(both, "yoke", 5, "yoke-abc", born); got != 210 {
		t.Fatalf("exact link vs legacy = %d, want 210", got)
	}
	amb := []SprintRunLink{{Sprint: 1, UpdatedAt: linkedAt, Repo: "yoke", ID: 5}, {Sprint: 2, UpdatedAt: linkedAt, Repo: "yoke", ID: 5}}
	if got := SprintForRun(amb, "yoke", 5, "", born); got != 0 {
		t.Fatalf("ambiguous legacy links = %d, want 0", got)
	}
}
