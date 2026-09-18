package weave

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/bus"
)

// captureAnnounce replaces the board send with a recorder, so these assertions
// are about what WOULD go out and never touch the operator's real board.
func captureAnnounce(t *testing.T) *[]string {
	t.Helper()
	var sent []string
	orig := announceStageFn
	origEnabled := announceEnabled
	announceStageFn = func(from, body string) error {
		sent = append(sent, from+"|"+body)
		return nil
	}
	announceEnabled = true
	t.Setenv("BASHY_SPRINT_ANNOUNCE", "")
	t.Cleanup(func() { announceStageFn = orig; announceEnabled = origEnabled; lastAnnounceErr = nil })
	return &sent
}

// A TRANSITION ANNOUNCES; A NOTE DOES NOT.
//
// The filter is the thread entry's KIND, and this is the test that makes that
// taxonomy real rather than a comment. Everything a conductor does all day —
// checkpoints, comments, ordinary system notes — is silent, or the board
// becomes a log nobody reads and the announcement is worth nothing.
func TestOnlyStageEntriesAnnounce(t *testing.T) {
	sent := captureAnnounce(t)
	s := &weaveStory{ID: 42, Title: "A sprint"}

	weaveStoryAppend(s, "pm", kindStage, "moved backlog → doing")
	weaveStoryAppend(s, "pm", kindSystem, "resumed own conductor lease and delivery stream")
	weaveStoryAppend(s, "pm", kindProgress, "checkpoint")
	weaveStoryAppend(s, "pm", kindDecision, "chose the cheaper agent")
	weaveStoryAppend(s, "pm", "note", "an ordinary note")

	if len(*sent) != 1 {
		t.Fatalf("announced %d entries, want exactly the stage one: %v", len(*sent), *sent)
	}
	got := (*sent)[0]
	// The message must carry the sprint the peer is being told about; they have
	// no other way to know which one.
	if !strings.Contains(got, "#42") || !strings.Contains(got, "A sprint") ||
		!strings.Contains(got, "moved backlog → doing") {
		t.Errorf("announcement = %q", got)
	}
	if !strings.HasPrefix(got, "pm|") {
		t.Errorf("announcement sender = %q, want the actor who made the change", got)
	}
	// The thread still records everything. Announcing is a VIEW of the log, not
	// a replacement for it.
	if len(s.Thread) != 5 {
		t.Errorf("thread has %d entries, want all 5 recorded regardless of announcement", len(s.Thread))
	}
}

// RE-TAKING YOUR OWN LEASE IS NOT A TRANSITION, and this is the case the whole
// kind split exists for.
//
// `sprint take` is also RECOVERY — its own help says a stale lease is taken
// directly after a SIGKILL or token exhaustion — so a conductor crash-looping
// takes repeatedly. The board deletes nothing, so announcing per call would
// make a crash loop permanent noise. Taking from SOMEBODY ELSE is a real
// handover and announces once.
func TestReclaimingYourOwnLeaseIsSilentWhileAHandoverIsNot(t *testing.T) {
	sent := captureAnnounce(t)
	s := &weaveStory{ID: 7, Title: "Flapping"}

	for range 5 {
		weaveStoryAppend(s, "pm", kindSystem, "resumed own conductor lease and delivery stream")
	}
	if len(*sent) != 0 {
		t.Fatalf("a re-take announced %d times; a crash loop must stay silent: %v", len(*sent), *sent)
	}

	weaveStoryAppend(s, "successor", kindStage, "took STALE conductor lease from pm (recovery)")
	if len(*sent) != 1 {
		t.Fatalf("a real handover announced %d times, want 1", len(*sent))
	}
}

// THE OFF SWITCH, both spellings.
func TestAnnouncementCanBeTurnedOff(t *testing.T) {
	sent := captureAnnounce(t)
	s := &weaveStory{ID: 1, Title: "Quiet"}

	announceEnabled = false
	weaveStoryAppend(s, "pm", kindStage, "created in backlog")
	announceEnabled = true

	t.Setenv("BASHY_SPRINT_ANNOUNCE", "0")
	weaveStoryAppend(s, "pm", kindStage, "moved backlog → doing")

	if len(*sent) != 0 {
		t.Errorf("announced %d entries with announcing off: %v", len(*sent), *sent)
	}
}

// THE STATE CHANGE IS THE TRANSACTION.
//
// A failed post must never fail the verb that was reporting it: the lease
// really was claimed, and an exit code saying otherwise makes an operator undo
// work that succeeded. But the failure must be VISIBLE — an announcement that
// silently did not happen is the same defect one layer down.
func TestAFailedAnnouncementIsReportedAndNeverFatal(t *testing.T) {
	captureAnnounce(t)
	announceStageFn = func(string, string) error { return errBoardDown }
	s := &weaveStory{ID: 3, Title: "Board is down"}

	weaveStoryAppend(s, "pm", kindStage, "handed off — released conductor lease")

	// The record was still written.
	if len(s.Thread) != 1 {
		t.Fatalf("thread = %d entries, want the change recorded despite the failed post", len(s.Thread))
	}
	if lastAnnounceErr == nil {
		t.Fatal("a failed announcement left no error to report")
	}
	var sb strings.Builder
	reportAnnounceFailure(&sb, "sprint handoff")
	out := sb.String()
	if !strings.Contains(out, "the state change was recorded") || !strings.Contains(out, "board is down") {
		t.Errorf("report = %q, want it to say the change stuck and why the post did not", out)
	}
	// Cleared, so the next verb does not re-report a stale failure.
	if lastAnnounceErr != nil {
		t.Error("the reported error was not cleared")
	}
}

// The body is capped because the board caps one at 1024 bytes and never
// truncates or splits — an over-long post would be refused outright.
func TestAnnouncementFitsTheBoardsLimit(t *testing.T) {
	s := &weaveStory{ID: 9, Title: strings.Repeat("long title ", 40)}
	msg := stageMessage(s, strings.Repeat("body ", 400))
	if len(msg) > 1024 {
		t.Errorf("message is %d bytes, over the board's 1024 cap", len(msg))
	}
	if !strings.Contains(msg, "#9") {
		t.Errorf("the sprint id was truncated away: %q", msg)
	}
}

type boardDownErr struct{}

func (boardDownErr) Error() string { return "board is down" }

var errBoardDown = boardDownErr{}

// THE ANNOUNCEMENT IS ADDRESSED, NOT BROADCAST — the sprint #139 payoff.
//
// Stage changes are for the OTHER seated managers: they act on a peer being
// started, handed off, or stopped. A broadcast made every agent on the host
// scan them under the cap while the peers still had to declare the concern to
// see them in full. Addressing the post to the live conductors gives members
// the uncapped tier by membership itself — and a board with nobody seated
// still records the transition, because history is not delivery.
func TestStageAnnouncementIsAddressedToTheConductors(t *testing.T) {
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	bus.FleetSelect = func(a bus.Audience) ([]string, error) {
		if a.Role != "conductor" {
			t.Errorf("selector = %+v, want the conductor role", a)
			return nil, nil
		}
		return []string{"peer-a", "peer-b"}, nil
	}
	t.Cleanup(func() { bus.FleetSelect = nil })

	if err := postStageToBoard("pm", "sprint #42 A sprint — moved backlog → doing"); err != nil {
		t.Fatalf("postStageToBoard: %v", err)
	}
	posts, err := bus.Posts()
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 || posts[0].Audience == nil || posts[0].Audience.Role != "conductor" {
		t.Fatalf("stage post = %+v; it must carry Audience{role conductor} so seated managers, and only they, are its uncapped readers", posts)
	}

	// Nobody seated: FleetSelect resolves to an EMPTY roster, which is not the
	// unresolvable case — the sprint still happened and the board still owes
	// its history. The append must succeed.
	bus.FleetSelect = func(bus.Audience) ([]string, error) { return nil, nil }
	if err := postStageToBoard("pm", "sprint #42 A sprint — stopped"); err != nil {
		t.Fatalf("zero managers must not fail the announcement: %v", err)
	}
	posts, err = bus.Posts()
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 2 || posts[1].Audience == nil || posts[1].Audience.Role != "conductor" {
		t.Fatalf("zero-manager stage post = %+v; an empty roster is honest history, not a send failure", posts)
	}
}
