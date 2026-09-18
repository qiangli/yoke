package chat

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

// isolateRoom points the room (and therefore the bus timeline) at a tempdir.
func isolateRoom(t *testing.T) {
	t.Helper()
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
}

func unresolvedNotifications(t *testing.T) []room.Event {
	t.Helper()
	events, err := room.Timeline(0)
	if err != nil {
		t.Fatal(err)
	}
	var out []room.Event
	for _, e := range events {
		if e.Type == room.EventNotify && e.Topic == "fleet.unresolved" {
			out = append(out, e)
		}
	}
	return out
}

// The coach's whole reason to exist is that a loop bound silently. Recording that
// as OTel tells a human reading a dashboard LATER; by then the looping agent has
// burned the budget the alert was warning about. The bus is the half that reaches
// the conductor/steward/foreman who could actually kill, reassign or re-scope.
func TestCoachPublishesSupervisorAlertWhenTheLadderIsExhausted(t *testing.T) {
	isolateRoom(t)

	pol := DefaultCoachPolicy()
	pol.MaxSteers = 2
	pol.Cooldown = 0
	pol.EscalateAfter = 0 // reflex only; we are testing the top of the ladder
	c := newCoach(pol)

	same := toolCall("go_test", `{"pkg":"./..."}`)
	feed(c, []Event{same, same, same, same, same, same, same, same, same, same})

	got := unresolvedNotifications(t)
	if len(got) == 0 {
		t.Fatal("the exhausted ladder raised no bus notification — only OTel, which no agent reads")
	}
	e := got[0]
	if e.Principal != "coach" {
		t.Errorf("principal = %q, want coach — a subscriber authorizes interrupts BY NAME", e.Principal)
	}
	if e.Priority != "interrupt" {
		t.Errorf("priority = %q, want interrupt: a loop the reflex and agent-coach both failed to resolve is a direction-changer", e.Priority)
	}
	if !strings.Contains(e.Body, "supervisor attention needed") {
		t.Errorf("body = %q", e.Body)
	}
}

// The latch. len(steers) >= MaxSteers stays true forever once crossed, so an
// unlatched publish would spray the bus for as long as the agent kept looping —
// the pollution failure mode, produced by the very signal meant to rescue it.
//
// The OTel BoundHit still fires every time, and that asymmetry is the point: a
// metric wants every occurrence, a notification wants the transition.
func TestCoachPublishesTheSupervisorAlertOnlyOnce(t *testing.T) {
	isolateRoom(t)

	pol := DefaultCoachPolicy()
	pol.MaxSteers = 1 // crossed almost immediately, then true for every later trip
	pol.Cooldown = 0
	pol.EscalateAfter = 0
	c := newCoach(pol)

	same := toolCall("go_test", `{"pkg":"./..."}`)
	var calls []Event
	for range 30 {
		calls = append(calls, same)
	}
	feed(c, calls)

	if n := len(unresolvedNotifications(t)); n != 1 {
		t.Errorf("published %d supervisor alerts, want exactly 1 — a stuck agent must not spray the bus", n)
	}
}

// A healthy run must put nothing on the bus at all. The bus makes a fleet worse
// if it carries traffic nobody needed.
func TestHealthyRunPublishesNothing(t *testing.T) {
	isolateRoom(t)

	c := newCoach(DefaultCoachPolicy())
	var calls []Event
	for i := range 12 {
		calls = append(calls, toolCall("read_file", `{"path":"f`+string(rune('a'+i))+`.go"}`))
	}
	feed(c, calls)

	if n := len(unresolvedNotifications(t)); n != 0 {
		t.Errorf("a healthy run published %d notifications, want 0", n)
	}
}

// The alert must fire in BOTH detection modes. It used to live inline in the
// pty-scrape trip, so an events-mode agent — a first-party harness reporting
// tool.call as data, the BETTER-instrumented case — could loop past MaxSteers and
// raise nothing at all: no telemetry, no notification, while Unresolved() happily
// reported true. The condition is mode-independent, so the alert is too.
func TestSupervisorAlertFiresInPtyModeToo(t *testing.T) {
	isolateRoom(t)

	pol := DefaultCoachPolicy()
	pol.MaxSteers = 1
	pol.Cooldown = 0
	pol.EscalateAfter = 0
	c := newCoach(pol)

	// A collapsed novelty window. The lines must ROTATE: feedPty dedups
	// consecutive identical lines (a spinner repainting is not new work), so
	// feeding one line 200 times collapses to a single entry and the window never
	// fills. Three rotating lines pass that filter while driving novelty far below
	// the floor — which is what a real churning loop looks like.
	churn := []string{
		"running the same failing check again aaaaaaa",
		"reading the same file again bbbbbbbbbbbbbbbb",
		"trying the same edit again cccccccccccccccc",
	}
	for i := range 200 {
		c.feedPty(churn[i%len(churn)])
	}

	got := unresolvedNotifications(t)
	if len(got) != 1 {
		t.Fatalf("pty mode raised %d supervisor alerts, want exactly 1", len(got))
	}
	if got[0].Topic != "fleet.unresolved" || got[0].Principal != "coach" {
		t.Errorf("event = %+v", got[0])
	}
}
