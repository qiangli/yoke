package weave

import (
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
)

func TestSprintUnansweredGateGuidanceNamesBusPending(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	start := time.Now().UTC().Add(-10 * time.Minute)
	owner := "s467-manager"
	p := bus.Pending{
		Seq:  1,
		TS:   start.Add(time.Minute).Format(time.RFC3339),
		To:   "conductor:467",
		Body: "urgent sprint question",
	}
	if err := bus.AppendPending(owner, p); err != nil {
		t.Fatal(err)
	}

	s := &weaveStory{
		ID:    467,
		UUID:  "11111111-2222-4333-8444-555555555555",
		Owner: owner,
		Boxes: []weaveStoryBox{{StartedAt: start}},
	}

	err := sprintUnansweredGate(s, "end")
	if err == nil {
		t.Fatal("expected unanswered gate to block end with unread mail")
	}

	msg := err.Error()
	// Gate message must name bashy bus pending --as SEAT instead of bashy inbox --as SEAT.
	wantCmd := "bashy bus pending --as " + owner
	if !strings.Contains(msg, wantCmd) {
		t.Errorf("gate error %q does not contain %q", msg, wantCmd)
	}
	misleadingCmd := "bashy inbox --as " + owner
	if strings.Contains(msg, misleadingCmd) {
		t.Errorf("gate error %q still contains misleading guidance %q", msg, misleadingCmd)
	}

	// Clearing ReadAt (as done by bashy bus pending --as SEAT) clears the gate.
	if err := bus.MarkRead(owner, p.Seq); err != nil {
		t.Fatal(err)
	}
	if err := sprintUnansweredGate(s, "end"); err != nil {
		t.Fatalf("expected gate to pass after pending marked read, got: %v", err)
	}
}
