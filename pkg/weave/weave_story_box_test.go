package weave

import (
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/role"
)

// THE REQUIREMENT THAT SHAPES THE DESIGN: several sprints run at once, each on
// its own clock. The box lives on the CARD, so there is no global "current
// sprint" that concurrent work would have to take turns with.
//
// This is the test that would fail if anyone ever hoisted the box to a single
// shared slot — which is the obvious simplification and the wrong one.
func TestBox_ConcurrentSprintsKeepIndependentClocks(t *testing.T) {
	base := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)

	quickFix := &weaveStory{ID: 1, Column: "doing",
		Boxes: []weaveStoryBox{{StartedAt: base, Cutoff: base.Add(45 * time.Minute), Planned: 45 * time.Minute}}}
	migration := &weaveStory{ID: 2, Column: "doing",
		Boxes: []weaveStoryBox{{StartedAt: base, Cutoff: base.Add(4 * time.Hour), Planned: 4 * time.Hour}}}
	yesterday := &weaveStory{ID: 3, Column: "doing",
		Boxes: []weaveStoryBox{{StartedAt: base.Add(-20 * time.Hour), Cutoff: base.Add(-18 * time.Hour), Planned: 2 * time.Hour}}}
	unboxed := &weaveStory{ID: 4, Column: "backlog"}

	now := base.Add(90 * time.Minute)

	// Three different cadences, three different verdicts, at one instant.
	if !quickFix.currentBox().Overdue(now) {
		t.Error("the 45m box must be overdue 90m in")
	}
	if migration.currentBox().Overdue(now) {
		t.Error("the 4h box must NOT be overdue 90m in — one sprint's cutoff says nothing about another's")
	}
	if !yesterday.currentBox().Overdue(now) {
		t.Error("a box opened yesterday and never stopped is still running, and long overdue")
	}
	if unboxed.currentBox().Running() {
		t.Error("a sprint with no box is not running")
	}
	if got := unboxed.currentBox().Status(now); got != "" {
		t.Errorf("an unboxed sprint must read exactly as before this feature existed, got %q", got)
	}

	// Stopping one leaves the others untouched — separate start/stop cycles.
	stop := now
	quickFix.currentBox().StoppedAt = &stop
	if quickFix.currentBox().Running() {
		t.Error("stopped box still reports running")
	}
	if !migration.currentBox().Running() || !yesterday.currentBox().Running() {
		t.Error("stopping one sprint must not close another's box")
	}
}

// The cutoff is a DECISION POINT, not a kill switch: nothing about being
// overdue stops a box being open, because refusing to work past it would
// abandon a run that may be one fix from green.
func TestBox_OverdueStaysRunning(t *testing.T) {
	base := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	b := &weaveStoryBox{StartedAt: base, Cutoff: base.Add(time.Hour), Planned: time.Hour}
	late := base.Add(3 * time.Hour)
	if !b.Overdue(late) || !b.Running() {
		t.Error("an overdue box must still be RUNNING — the cutoff informs, it does not halt")
	}
	if got := b.Status(late); got == "" || !strings.Contains(got, "OVERDUE by 2h") {
		t.Errorf("status = %q, want it to state how far past the cutoff it is", got)
	}
}

// Extending must not rewrite the original estimate. The gap between what was
// promised and what was taken is the entire signal a cadence is tuned from; if
// extending moved Planned too, every box would retroactively look perfect.
func TestBox_ExtendKeepsTheOriginalEstimate(t *testing.T) {
	base := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	b := &weaveStoryBox{StartedAt: base, Cutoff: base.Add(time.Hour), Planned: time.Hour}
	b.Cutoff = b.Cutoff.Add(90 * time.Minute) // what `sprint extend` does
	if b.Planned != time.Hour {
		t.Errorf("Planned = %s, want the original 1h to survive an extension", b.Planned)
	}
	// And the honest verdict at stop is still measured against the promise.
	stop := base.Add(2 * time.Hour)
	b.StoppedAt = &stop
	if b.Elapsed(stop) <= b.Planned {
		t.Error("a 2h run against a 1h promise must read as over, extension notwithstanding")
	}
}

func TestBox_StatusReadsForEachPhase(t *testing.T) {
	base := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	b := &weaveStoryBox{StartedAt: base, Cutoff: base.Add(2 * time.Hour), Planned: 2 * time.Hour}
	if got := b.Status(base.Add(30 * time.Minute)); !strings.Contains(got, "left of 2h") {
		t.Errorf("running status = %q", got)
	}
	stop := base.Add(105 * time.Minute)
	b.StoppedAt = &stop
	if got := b.Status(stop); !strings.Contains(got, "stopped after") || !strings.Contains(got, "planned 2h") {
		t.Errorf("stopped status = %q, want actual AND planned — one without the other tunes nothing", got)
	}
}

// ONE CONDUCTOR IS ACCOUNTABLE FOR EVERY IN-PROGRESS SPRINT. `start` enforces
// that by claiming the lease — but a lease is a heartbeat with a TTL, so a
// running box can outlive its conductor. That state must be distinguishable
// from healthy work, because it is the only one where nothing improves until a
// person acts.
func TestBox_RunningSprintCanOutliveItsConductor(t *testing.T) {
	base := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	s := &weaveStory{
		ID: 1, Column: "doing",
		Boxes: []weaveStoryBox{{StartedAt: base, Cutoff: base.Add(4 * time.Hour), Planned: 4 * time.Hour}},
		Lease: &weaveStoryLease{Holder: "some-conductor", At: base},
	}
	// Well inside the box, but far past the lease TTL.
	now := base.Add(2 * time.Hour)
	if !s.currentBox().Running() {
		t.Fatal("box should still be running")
	}
	if s.currentBox().Overdue(now) {
		t.Fatal("box is not overdue — this is precisely the case that would hide")
	}
	_, stale, free := weaveStoryLeaseState(s)
	if !stale || free {
		t.Errorf("lease should read STALE %v/%v after %s with a %s TTL", stale, free, 2*time.Hour, SprintLeaseTTL)
	}
}

// The three seats now answer "is this held, and by whom" with ONE rule. The
// property worth pinning is the one the sprint lease could not express before:
// a lease with no timestamp is UNKNOWN, not silently fresh.
func TestSprintSeat_MissingHeartbeatIsUnknownNotFresh(t *testing.T) {
	s := &weaveStory{ID: 1, Lease: &weaveStoryLease{Holder: "a"}} // no At
	if got := s.seat().Live(time.Now()); got != role.LivenessUnknown {
		t.Errorf("a lease with no heartbeat = %v, want unknown — comparing a zero "+
			"time against a TTL used to read as fresh, which keeps a dead holder alive forever", got)
	}
	// And a real heartbeat still reads live.
	s.Lease.At = time.Now()
	if got := s.seat().Live(time.Now()); got != role.LivenessLive {
		t.Errorf("a fresh lease = %v, want live", got)
	}
}
