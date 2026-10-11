package weave

import (
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
)

func TestSprintUnansweredCountsOnlyCurrentSprintMail(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	start := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	for _, p := range []bus.Pending{
		{TS: start.Add(time.Minute).Format(time.RFC3339), To: "conductor:419", Body: "please review"},
		{TS: start.Add(2 * time.Minute).Format(time.RFC3339), Body: "Sprint 318 request"},
		{TS: start.Add(-time.Minute).Format(time.RFC3339), To: "conductor:419", Body: "before sprint start"},
		{TS: start.Add(3 * time.Minute).Format(time.RFC3339), Body: "unrelated run-4190 reference"},
	} {
		if err := bus.AppendPending("reused-seat", p); err != nil {
			t.Fatal(err)
		}
	}
	s := &weaveStory{ID: 419, UUID: "11111111-2222-4333-8444-555555555555", Boxes: []weaveStoryBox{{StartedAt: start}}}
	count, oldest := sprintOwnerUnansweredForSprint("reused-seat", s)
	if count != 1 {
		t.Fatalf("scoped unanswered count = %d, want only the one addressed message", count)
	}
	if oldest == 0 {
		t.Fatal("oldest age is zero; the accepted message timestamp was not counted")
	}
}
