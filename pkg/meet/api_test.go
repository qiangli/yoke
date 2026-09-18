package meet

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/chat"
)

// api.go is the ONE implementation both surfaces call. These tests hold that
// claim two ways: the exported funcs behave, and the cobra commands reach them
// rather than keeping a private copy of the same act.

// Every ref-taking verb accepts what a human types at `meet list`: a room number,
// a full id, or an unambiguous prefix. That is resolveMeeting's job, reused —
// a second resolver would drift on which spellings work, and the failure mode is
// acting on the wrong meeting.
func TestAPIAcceptsEveryRoomReference(t *testing.T) {
	st := newRoom(t)
	st.Room = 1
	if err := st.save(); err != nil {
		t.Fatal(err)
	}

	for _, ref := range []string{"1", "#1", st.ID, st.ID[:12]} {
		got, _, err := Room(ref)
		if err != nil {
			t.Fatalf("Room(%q): %v", ref, err)
		}
		if got.ID != st.ID {
			t.Errorf("Room(%q) = %s, want %s", ref, got.ID, st.ID)
		}
		if _, err := Post(ref, "qiangli", "by "+ref); err != nil {
			t.Errorf("Post(%q): %v", ref, err)
		}
	}

	events, _ := readTranscript(st.ID)
	if len(events) != 4 {
		t.Fatalf("want one post per reference spelling: %+v", events)
	}
}

// A miss is ErrNoRoom, whatever the spelling — the sentinel a transport
// classifies on. The human message is preserved, because it is the one that says
// what to do about it.
func TestAPIUnknownReferenceIsErrNoRoom(t *testing.T) {
	newRoom(t)

	for _, ref := range []string{"nope", "2026-01-01-nothing-here-0000", "99"} {
		if _, _, err := Room(ref); !errors.Is(err, ErrNoRoom) {
			t.Errorf("Room(%q) = %v, want ErrNoRoom", ref, err)
		}
		if _, err := Post(ref, "qiangli", "hi"); !errors.Is(err, ErrNoRoom) {
			t.Errorf("Post(%q) = %v, want ErrNoRoom", ref, err)
		}
		if err := Close(ref, "qiangli"); !errors.Is(err, ErrNoRoom) {
			t.Errorf("Close(%q) = %v, want ErrNoRoom", ref, err)
		}
	}

	// The message a human reads still points at the fix.
	_, _, err := Room("nope")
	if !strings.Contains(err.Error(), "meet list") {
		t.Errorf("the miss must name the recovery: %v", err)
	}
}

// Create builds through sessionFlags.newState, so a room opened from a browser is
// held to exactly the invariants `meet start` enforces.
func TestCreateHoldsTheRoleInvariants(t *testing.T) {
	newRoom(t)

	if _, err := Create(CreateOptions{Participants: []string{"codex"}}); err == nil {
		t.Error("a room needs a topic")
	}
	// The secretary records the decisions and must not have a stake in them —
	// enforced by Validate, not re-checked here.
	_, err := Create(CreateOptions{
		Topic: "x", Participants: []string{"codex"}, Secretary: "codex",
	})
	if err == nil {
		t.Error("the secretary must not also be a participant")
	}

	st, err := Create(CreateOptions{
		Name:   "sprint 123",
		Topic:  "Should the cache be write-through?",
		Agenda: []string{"the write path"}, Participants: []string{"codex"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if st.Room < 1 {
		t.Error("a created room needs a door to attach to")
	}
	if st.Name != "sprint 123" {
		t.Errorf("room name = %q, want the durable object's name", st.Name)
	}
	reloaded, _, err := Room(st.ID)
	if err != nil || reloaded.Name != "sprint 123" {
		t.Fatalf("reloaded room name = %q, %v", reloaded.Name, err)
	}
	if st.Status != "open" || st.Out != "docs" || st.TurnTimeout == "" {
		t.Errorf("defaults did not land: %+v", st)
	}
	// The agenda is recorded as well as stored — a round reads the header, and a
	// reader of the minutes reads the transcript.
	events, _ := readTranscript(st.ID)
	if countKind(events, "agenda") != 1 {
		t.Errorf("agenda events = %+v", events)
	}
}

// Create seats the identity its caller authenticated, and does not require that
// identity to look like an OS username.
//
// This is the api.go half of the tunnel defect: an email is a legitimate
// principal, and Validate's "the initiator must be at this meeting" holds because
// the caller IS at the meeting — it is the human seat — not because the check was
// waived for the creation path.
func TestCreateSeatsTheAuthenticatedCaller(t *testing.T) {
	newRoom(t) // isolate the store; the OS user here is "qiangli"

	const cloudUser = "qiangli@example.com"
	st, err := Create(CreateOptions{Topic: "tunnel verification", Human: cloudUser})
	if err != nil {
		t.Fatalf("a cloud-authenticated create must succeed: %v", err)
	}
	if st.Human != cloudUser || st.Initiator != cloudUser {
		t.Fatalf("human = %q initiator = %q, want %q", st.Human, st.Initiator, cloudUser)
	}
	// Seated means seated: the room's member list has them in it, which is what
	// every later check reads.
	if !contains(st.attendees(), cloudUser) {
		t.Errorf("attendees = %v, want the creator among them", st.attendees())
	}
	// And the organizer privilege follows the recorded name, not the OS user.
	if err := requireOrganizer(st, cloudUser); err != nil {
		t.Errorf("the creator must be the organizer: %v", err)
	}
	if err := requireOrganizer(st, "qiangli"); !errors.Is(err, ErrNotOrganizer) {
		t.Errorf("the host's OS user did not convene this room: %v", err)
	}

	// The CLI/loopback path is unchanged: no Human means the OS user.
	local, err := Create(CreateOptions{Topic: "loopback control"})
	if err != nil {
		t.Fatalf("the loopback create must still work: %v", err)
	}
	if local.Human != "qiangli" || local.Initiator != "qiangli" {
		t.Errorf("human = %q initiator = %q, want the OS user", local.Human, local.Initiator)
	}

	// An explicitly named initiator still wins, and is still held to being someone
	// at the table — seating the creator widened who is at the table, nothing else.
	seatEverything(t)
	agentLed, err := Create(CreateOptions{
		Topic: "an agent convened this", Human: cloudUser,
		Participants: []string{"codex"}, Initiator: "codex",
	})
	if err != nil {
		t.Fatalf("naming a seated agent as initiator: %v", err)
	}
	if agentLed.Initiator != "codex" || agentLed.Human != cloudUser {
		t.Errorf("initiator = %q human = %q", agentLed.Initiator, agentLed.Human)
	}
	if _, err := Create(CreateOptions{
		Topic: "a ghost convened this", Human: cloudUser,
		Participants: []string{"codex"}, Initiator: "ghost",
	}); err == nil {
		t.Error("an initiator who is at no seat must still be refused")
	}
}

// The refusal is read by somebody who has never seen the CLI. It names no flag —
// a web user has no `--initiator` to correct — and lists who is actually in the
// room, which is the one fact that makes it actionable from either surface.
func TestInitiatorRefusalSpeaksToAWebUser(t *testing.T) {
	st := &State{
		Topic: "x", Participants: []string{"codex"},
		Human: "qiangli@example.com", Initiator: "somebody-else@example.com",
	}
	err := st.Validate()
	if err == nil {
		t.Fatal("an initiator at no seat must be refused")
	}
	msg := err.Error()
	for _, jargon := range []string{"--initiator", "the chair, or the secretary"} {
		if strings.Contains(msg, jargon) {
			t.Errorf("the message still speaks CLI at a browser (%q): %s", jargon, msg)
		}
	}
	// It must still say who it is talking about, and who IS in the room.
	for _, want := range []string{"somebody-else@example.com", "qiangli@example.com", "codex"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message must name %q: %s", want, msg)
		}
	}
}

// Close is organizer-only and must never reach for stdin: an HTTP handler has no
// terminal, and confirmConclusion would either error or block on a closed pipe.
func TestCloseIsOrganizerOnlyAndNeverPrompts(t *testing.T) {
	st := newRoom(t)

	if err := Close(st.ID, "codex"); !errors.Is(err, ErrNotOrganizer) {
		t.Fatalf("a member's close = %v, want ErrNotOrganizer", err)
	}
	if reloaded, _ := loadState(st.ID); reloaded.Status != "open" {
		t.Fatal("a refused close must not close the room")
	}

	if err := Close(st.ID, "qiangli"); err != nil {
		t.Fatalf("the organizer's close: %v", err)
	}
	reloaded, _ := loadState(st.ID)
	if reloaded.Status != "closed" {
		t.Errorf("status = %q", reloaded.Status)
	}
	// Yes is recorded rather than silent: the confirm event is what distinguishes
	// a room concluded deliberately from one abandoned.
	events, _ := readTranscript(st.ID)
	if countKind(events, "confirm") != 1 {
		t.Errorf("want one confirm event: %+v", events)
	}
}

// Address takes the room lease, because a turn appends to the transcript a
// concurrent round is also writing.
func TestAddressIsLeaseGuarded(t *testing.T) {
	st := newRoom(t)
	lease, err := acquireRunLease(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	if _, err := Address(context.Background(), st.ID, "codex", "hi"); !errors.Is(err, ErrMeetingBusy) {
		t.Fatalf("Address while the floor is held = %v, want ErrMeetingBusy", err)
	}
	// A post is not: that is what lets a room feel like chat.
	if _, err := Post(st.ID, "qiangli", "meanwhile"); err != nil {
		t.Fatalf("Post while the floor is held: %v", err)
	}
}

func TestMarkKinds(t *testing.T) {
	st := newRoom(t)

	for _, kind := range []string{"decision", "action", "note"} {
		ev, err := Mark(st.ID, kind, kind+" text")
		if err != nil {
			t.Fatalf("Mark(%q): %v", kind, err)
		}
		if ev.Kind != kind || ev.Speaker != st.Human {
			t.Errorf("Mark(%q) = %+v", kind, ev)
		}
	}
	// Case is not the point; the closed set is.
	if _, err := Mark(st.ID, "DECISION", "shouty"); err != nil {
		t.Errorf("kind should be case-insensitive: %v", err)
	}
	if _, err := Mark(st.ID, "proclamation", "hear ye"); err == nil {
		t.Error("an invented kind is an event nothing renders; it must be refused")
	}
	if _, err := Mark(st.ID, "note", "  "); err == nil {
		t.Error("an empty marker is not a marker")
	}
}

// The cobra tree goes THROUGH api.go — one implementation, not two. `tell` is the
// one write verb that runs no agent, so it is the one this can prove end to end
// without spawning a CLI.
func TestCobraTellGoesThroughPost(t *testing.T) {
	st := newRoom(t)
	st.Room = 1
	if err := st.save(); err != nil {
		t.Fatal(err)
	}

	cmd := NewMeetCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	// A room NUMBER, which the old body could not accept: it called loadState on
	// the argument directly. That it works now is the visible consequence of the
	// command reaching the room through Post → roomOf → resolveMeeting.
	cmd.SetArgs([]string{"tell", "1", "the", "cache", "should", "be", "write-through"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("meet tell: %v", err)
	}

	events, _ := readTranscript(st.ID)
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Kind != "human" || events[0].Speaker != st.Human {
		t.Errorf("event = %+v", events[0])
	}
	if events[0].Text != "the cache should be write-through" {
		t.Errorf("text = %q", events[0].Text)
	}
}

func TestPostAsBoardRequiresSeatAndRecordsMessage(t *testing.T) {
	st := newRoom(t)
	st.Board = true
	st.Participants = []string{"codex", "opencode"}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}

	if _, err := PostAs(st.ID, "", "", "missing author"); err == nil {
		t.Fatal("board PostAs accepted an empty author")
	}
	if _, err := PostAs(st.ID, "ghost", "", "no seat"); err == nil || !strings.Contains(err.Error(), "bashy meet invite") {
		t.Fatalf("unseated author error = %v", err)
	}
	if _, err := PostAs(st.ID, "codex", "ghost", "bad target"); err == nil || !strings.Contains(err.Error(), "failed:") {
		t.Fatalf("unseated target error = %v", err)
	}

	ev, err := PostAs(st.ID, "codex", "opencode", "please read")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != "message" || ev.Role != string(RoleParticipant) || ev.Speaker != "codex" || ev.To != "opencode" {
		t.Fatalf("event = %+v", ev)
	}
	events, _ := readTranscript(st.ID)
	if len(events) != 1 || events[0].Kind != "message" || events[0].To != "opencode" {
		t.Fatalf("transcript = %+v", events)
	}
}

func TestCobraBoardTellAndRead(t *testing.T) {
	st := newRoom(t)
	st.Board = true
	st.Participants = []string{"codex", "opencode"}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}

	tell := NewMeetCmd()
	var tellOut, tellErr bytes.Buffer
	tell.SetOut(&tellOut)
	tell.SetErr(&tellErr)
	tell.SetArgs([]string{"tell", st.ID, "--as", "codex", "--to", "opencode", "check", "the", "gate"})
	if err := tell.Execute(); err != nil {
		t.Fatalf("meet tell: %v", err)
	}
	if got := tellErr.String(); !strings.Contains(got, "unverified: opencode") {
		t.Fatalf("receipt = %q", got)
	}

	read := NewMeetCmd()
	var readOut, readErr bytes.Buffer
	read.SetOut(&readOut)
	read.SetErr(&readErr)
	read.SetArgs([]string{"read", st.ID, "--as", "opencode"})
	if err := read.Execute(); err != nil {
		t.Fatalf("meet read: %v", err)
	}
	if !strings.Contains(readOut.String(), "check the gate") {
		t.Fatalf("read output = %q", readOut.String())
	}
	if got := SeenSeq(st.ID, "opencode"); got != 1 {
		t.Fatalf("read did not advance cursor: %d", got)
	}

	again := NewMeetCmd()
	var againOut, againErr bytes.Buffer
	again.SetOut(&againOut)
	again.SetErr(&againErr)
	again.SetArgs([]string{"read", st.ID, "--as", "opencode"})
	if err := again.Execute(); err != nil {
		t.Fatalf("empty meet read: %v", err)
	}
	if againOut.Len() != 0 || !strings.Contains(againErr.String(), "EMPTY (seen through 1)") {
		t.Fatalf("empty read stdout=%q stderr=%q", againOut.String(), againErr.String())
	}
}

func TestCobraReadPeekAndWaitErrors(t *testing.T) {
	st := newRoom(t)
	st.Board = true
	st.Participants = []string{"codex"}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := PostAs(st.ID, "codex", "", "still unread"); err != nil {
		t.Fatal(err)
	}

	peek := NewMeetCmd()
	peek.SetOut(&bytes.Buffer{})
	peek.SetErr(&bytes.Buffer{})
	peek.SetArgs([]string{"read", st.ID, "--as", "codex", "--peek"})
	if err := peek.Execute(); err != nil {
		t.Fatalf("peek read: %v", err)
	}
	if got := SeenSeq(st.ID, "codex"); got != 0 {
		t.Fatalf("peek advanced cursor to %d", got)
	}

	neg := NewMeetCmd()
	neg.SetArgs([]string{"read", st.ID, "--as", "codex", "--wait", "-1s"})
	if err := neg.Execute(); err == nil {
		t.Fatal("negative wait accepted")
	}

	st.Status = "closed"
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	closed := NewMeetCmd()
	closed.SetArgs([]string{"read", st.ID, "--as", "codex", "--wait", "1ms"})
	if err := closed.Execute(); err == nil || !strings.Contains(err.Error(), "closed board") {
		t.Fatalf("closed wait error = %v", err)
	}
}

func TestBoardDeliveryStateNamesAreCanonical(t *testing.T) {
	st := newRoom(t)
	if got := boardDeliveryState(st.ID, "opencode", 1); got != "unverified" {
		t.Fatalf("no cursor state = %q", got)
	}
	if err := markSeenSeq(st.ID, "opencode", 1); err != nil {
		t.Fatal(err)
	}
	if got := boardDeliveryState(st.ID, "opencode", 2); got != "queued" {
		t.Fatalf("behind cursor state = %q", got)
	}
	if got := boardDeliveryState(st.ID, "opencode", 1); got != "read" {
		t.Fatalf("caught-up cursor state = %q", got)
	}
	p, err := seenPath(st.ID, "opencode")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("not-a-number\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := boardDeliveryState(st.ID, "opencode", 1); got != "failed" {
		t.Fatalf("corrupt cursor state = %q", got)
	}
}

// `meet close` shares closeRoom with Close, and keeps its own attended semantics:
// the terminal prompt is the check there, so --yes is what an unattended CLI
// close needs and the organizer check does not apply to whoever typed it.
func TestCobraCloseSharesTheBody(t *testing.T) {
	st := newRoom(t)

	cmd := NewMeetCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"close", st.ID, "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("meet close: %v", err)
	}
	if !strings.Contains(out.String(), "wrote ") {
		t.Errorf("close must report where the minutes went:\n%s", out.String())
	}
	reloaded, _ := loadState(st.ID)
	if reloaded.Status != "closed" {
		t.Errorf("status = %q", reloaded.Status)
	}
}

// Rooms is the list a channel sidebar renders, and it backfills a door for an
// open room that has none — a number that appeared only after some other command
// ran would be a door that is sometimes there.
func TestRoomsBackfillsADoor(t *testing.T) {
	st := newRoom(t)
	st.Room = 0
	if err := st.save(); err != nil {
		t.Fatal(err)
	}

	rooms, err := Rooms()
	if err != nil {
		t.Fatal(err)
	}
	if len(rooms) != 1 {
		t.Fatalf("rooms = %+v", rooms)
	}
	if rooms[0].Room != 1 {
		t.Errorf("room = %d, want the lowest free door", rooms[0].Room)
	}
	if rooms[0].Updated.IsZero() {
		t.Error("a room with no transcript still reports when it was created")
	}
	reloaded, _ := loadState(st.ID)
	if reloaded.Room != 1 {
		t.Error("the backfilled door must be persisted, or the number renumbers under the reader")
	}
}

// Rooms feeds Relay's left navigation, not the meeting-history ledger. Closed
// and abandoned sessions have released their doors; returning them here makes a
// reused room number or repeated topic look like duplicate active channels.
// `meet list` keeps its separate history-oriented path, so filtering this API
// does not erase or hide history from the CLI.
func TestRoomsExcludesHistoricalSessions(t *testing.T) {
	t.Setenv("BASHY_MEET_DIR", t.TempDir())

	saveMeeting(t, "active", 1, "open")
	saveMeeting(t, "concluded", 1, "closed")
	saveMeeting(t, "reaped", 1, "abandoned")

	rooms, err := Rooms()
	if err != nil {
		t.Fatal(err)
	}
	if len(rooms) != 1 || rooms[0].ID != "active" {
		t.Fatalf("Relay rooms = %+v, want only the active session", rooms)
	}
}

// Addressing an agent must leave the QUESTION in the room, not only the answer.
//
// It did not, and from a browser that meant your own message vanished: the room
// showed replies to questions nobody could see, and a turn that timed out left
// no trace that anything had been asked. The 1:1 chat records the human's
// message and then runs the turn (relay_dm.go) — the same store — so the room
// disagreed with the chat about the shape of one exchange.
func TestAddressRecordsTheQuestionAddressedToTheAgent(t *testing.T) {
	st := newRoom(t)
	seatEverything(t)
	withFakeAgent(t, "the answer")

	if _, err := Address(t.Context(), st.ID, "codex", "what is the hostname?"); err != nil {
		t.Fatalf("Address: %v", err)
	}

	events, err := readTranscript(st.ID)
	if err != nil {
		t.Fatalf("readTranscript: %v", err)
	}
	var asked *Event
	for i := range events {
		if events[i].Kind == "human" {
			asked = &events[i]
			break
		}
	}
	if asked == nil {
		t.Fatalf("the question was not recorded; transcript = %+v", events)
	}
	if asked.Speaker != st.Human {
		t.Errorf("question attributed to %q, want the room's human %q", asked.Speaker, st.Human)
	}
	if asked.To != "codex" {
		t.Errorf("question addressed to %q; an unaddressed question reaches no inbox", asked.To)
	}
	if asked.Text != "what is the hostname?" {
		t.Errorf("question text = %q", asked.Text)
	}
	// Order matters: the question must be readable even when the turn after it
	// times out or crashes.
	if events[0].Kind != "human" {
		t.Errorf("the answer was recorded before the question: %+v", events)
	}
}

// ...and it must not then be delivered a SECOND time.
//
// The question is directed mail and `meet dispatch` wakes on exactly that, so a
// question answered on the spot would be handed to the same agent again on the
// next pass. Address acknowledges it, the way Dispatch acknowledges its own.
func TestAddressLeavesNoUnreadMailForTheAgentThatAnswered(t *testing.T) {
	st := newRoom(t)
	seatEverything(t)
	withFakeAgent(t, "the answer")

	if _, err := Address(t.Context(), st.ID, "codex", "what is the hostname?"); err != nil {
		t.Fatalf("Address: %v", err)
	}

	directed, _, _, err := Unread(st.ID, "codex", 0)
	if err != nil {
		t.Fatalf("Unread: %v", err)
	}
	if len(directed) != 0 {
		t.Errorf("codex still has %d unread directed message(s) after answering: %+v", len(directed), directed)
	}
}

// A FAILED turn is the opposite case: nothing answered the question, so it must
// stay unread and the next dispatch must still deliver it.
func TestAddressKeepsTheQuestionUnreadWhenTheTurnFails(t *testing.T) {
	st := newRoom(t)
	seatEverything(t)
	old := apiRunner
	apiRunner = func() chat.Runner { return unavailableRunner{} }
	t.Cleanup(func() { apiRunner = old })

	if _, err := Address(t.Context(), st.ID, "codex", "still needs an answer"); err == nil {
		t.Fatal("a failed turn must report the failure")
	}

	directed, _, _, err := Unread(st.ID, "codex", 0)
	if err != nil {
		t.Fatalf("Unread: %v", err)
	}
	if len(directed) != 1 {
		t.Fatalf("want the unanswered question still unread, got %d: %+v", len(directed), directed)
	}
	if directed[0].Text != "still needs an answer" {
		t.Errorf("unread message = %q", directed[0].Text)
	}
}

// unavailableRunner stands in for an agent CLI that could not be launched.
type unavailableRunner struct{}

func (unavailableRunner) Run(_ context.Context, _ string, _ []string, _ string) (string, int, error) {
	return "", 127, errors.New("agent unavailable")
}
