package meet

import (
	"context"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/chat"
)

// dispatchRoom seats three participants, which is the point: every property
// below is about ROUTING, and routing does not exist at N=1. A 1:1 relay DM has
// exactly one possible recipient, so none of these tests can be written against
// it.
func dispatchRoom(t *testing.T, reply string) *State {
	t.Helper()
	st := newTestSession(t)
	st.Participants = []string{"alpha", "beta", "gamma"}
	st.Secretary = ""
	if err := st.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	old := apiRunner
	apiRunner = func() chat.Runner { return fakeRunner{reply: reply} }
	t.Cleanup(func() { apiRunner = old })
	return st
}

func woken(res []Dispatched) []string {
	var names []string
	for _, r := range res {
		names = append(names, r.Agent)
	}
	return names
}

// THE CORE ROUTING PROPERTY: mail addressed to ONE participant wakes exactly
// that one, and leaves the others alone.
func TestDispatchWakesOnlyTheAddressee(t *testing.T) {
	st := dispatchRoom(t, "ack")
	if _, err := record(st, "human", "qiangli", "human", "ignored, addressed to nobody"); err != nil {
		t.Fatal(err)
	}
	if _, err := recordFull(st, Event{
		Round: st.Round, Speaker: "qiangli", Role: string(RoleHuman),
		Kind: "human", To: "beta", Text: "beta, what is the status?", TS: nowFn(),
	}); err != nil {
		t.Fatal(err)
	}

	res, err := Dispatch(context.Background(), st.ID)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := woken(res); len(got) != 1 || got[0] != "beta" {
		t.Fatalf("woke %v, want exactly [beta] — alpha and gamma were not addressed", got)
	}
	if res[0].Unread != 1 {
		t.Errorf("beta had %d unread directed, want 1", res[0].Unread)
	}
	if !strings.Contains(res[0].Reply.Text, "ack") {
		t.Errorf("beta's reply was not recorded: %q", res[0].Reply.Text)
	}
}

// AN UNADDRESSED POST WAKES NOBODY. This is the termination rule, not a
// preference: if a broadcast woke all N, each of the N replies is itself a post
// in the same transcript and would wake the others, without end.
func TestDispatchIgnoresUnaddressedMail(t *testing.T) {
	st := dispatchRoom(t, "ack")
	if _, err := record(st, "human", "qiangli", "human", "general remark to the room"); err != nil {
		t.Fatal(err)
	}
	res, err := Dispatch(context.Background(), st.ID)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("an unaddressed post woke %v; rounds are the mechanism for "+
			"everyone-speaks, precisely because a round terminates", woken(res))
	}
}

// A reply must not itself trigger a dispatch, or the room never settles. The
// turn is recorded with no addressee, so a second pass finds nothing.
func TestDispatchIsIdempotentAndRepliesDoNotCascade(t *testing.T) {
	st := dispatchRoom(t, "ack")
	if _, err := recordFull(st, Event{
		Round: st.Round, Speaker: "qiangli", Role: string(RoleHuman),
		Kind: "human", To: "alpha", Text: "alpha?", TS: nowFn(),
	}); err != nil {
		t.Fatal(err)
	}
	first, err := Dispatch(context.Background(), st.ID)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first pass woke %v, want [alpha]", woken(first))
	}
	second, err := Dispatch(context.Background(), st.ID)
	if err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second pass woke %v — the cursor did not advance, or alpha's own "+
			"reply woke somebody, either of which is an unbounded cascade", woken(second))
	}
}

// Cursors are PER PARTICIPANT. Two agents addressed in the same pass each answer
// once, and neither consumes the other's mail.
func TestDispatchAdvancesEachCursorIndependently(t *testing.T) {
	st := dispatchRoom(t, "ack")
	for _, to := range []string{"alpha", "gamma"} {
		if _, err := recordFull(st, Event{
			Round: st.Round, Speaker: "qiangli", Role: string(RoleHuman),
			Kind: "human", To: to, Text: to + ", report", TS: nowFn(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := Dispatch(context.Background(), st.ID)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	got := woken(res)
	if len(got) != 2 {
		t.Fatalf("woke %v, want alpha and gamma", got)
	}
	for _, r := range res {
		if r.Unread != 1 {
			t.Errorf("%s saw %d directed messages, want 1 — a shared cursor would "+
				"show 2 for one of them and 0 for the other", r.Agent, r.Unread)
		}
	}
	if strings.Join(got, ",") == "beta" {
		t.Fatal("beta was woken and was never addressed")
	}
}

// A FAILED TURN MUST NOT CONSUME THE MAIL. Losing a message to a crashed agent
// is the same defect as never delivering it, with a different cause.
func TestDispatchLeavesMailUnreadWhenTheTurnFails(t *testing.T) {
	st := dispatchRoom(t, "ack")
	old := apiRunner
	apiRunner = func() chat.Runner { return errRunner{} }
	t.Cleanup(func() { apiRunner = old })

	if _, err := recordFull(st, Event{
		Round: st.Round, Speaker: "qiangli", Role: string(RoleHuman),
		Kind: "human", To: "alpha", Text: "alpha?", TS: nowFn(),
	}); err != nil {
		t.Fatal(err)
	}
	res, err := Dispatch(context.Background(), st.ID)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(res) != 1 || res[0].Err == nil {
		t.Fatalf("a failing turn was not reported: %+v", res)
	}
	// The cursor must not have moved: the mail is still owed to alpha.
	directed, _, _, _, uerr := UnreadRecords(st.ID, "alpha", 0)
	if uerr != nil {
		t.Fatal(uerr)
	}
	if len(directed) != 1 {
		t.Fatalf("alpha has %d unread after a FAILED turn, want 1 — a failed "+
			"delivery that advances the cursor loses the message silently", len(directed))
	}
}

// A board spawns nobody by design; dispatching into one would quietly convert it
// into a chaired room.
func TestDispatchRefusesABoard(t *testing.T) {
	st := dispatchRoom(t, "ack")
	st.Board = true
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := Dispatch(context.Background(), st.ID); err == nil {
		t.Fatal("dispatch ran on a board; a board's participants post on their own turns")
	}
}

// THE POINT OF THE WHOLE STORY: a web DM must be MAIL, not a private
// transcript. It used to write to <meetdir>/relay-dms/<agent>/, which nothing
// else read — so one name had two mailboxes and only the CLI's was visible to
// `bashy inbox`, to reachability, and to any spawn that reads its inbox before
// answering.
//
// This asserts the property that closes it: a message sent through the web DM
// path lands in the shared dm room as mail ADDRESSED to that agent, which is
// what makes it directed in the unified inbox and what dispatch wakes on.
func TestWebDMWritesMailTheAgentCanActuallyRead(t *testing.T) {
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	t.Setenv("BASHY_CAPABILITY_DIR", t.TempDir())
	old := nowFn
	nowFn = fixedNow
	t.Cleanup(func() { nowFn = old })
	prevSeat := operableFn
	operableFn = func(string) (bool, string) { return true, "" }
	t.Cleanup(func() { operableFn = prevSeat })

	const agent, human = "codex", "qiangli"
	if _, err := ensureRelayDM(agent, human); err != nil {
		t.Skipf("this host cannot seat %s: %v", agent, err)
	}
	if err := appendRelayDMEvent(agent, relayDMEvent{
		Speaker: human, Role: "user", Text: "are you there?",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	// It is in the shared room, not a private directory.
	st, err := dmRoomFor(agent, human)
	if err != nil {
		t.Fatalf("dm room: %v", err)
	}
	events, err := readRoomTranscript(st.ID)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("the DM wrote nothing into the shared room")
	}
	last := events[len(events)-1]
	if last.Text != "are you there?" {
		t.Fatalf("room transcript last event = %q", last.Text)
	}

	// And it is DIRECTED at the agent, which is what makes it mail rather than
	// ambient text: an unaddressed post wakes nobody and shows up in no inbox
	// as directed.
	if !strings.EqualFold(last.To, agent) {
		t.Fatalf("the DM is addressed to %q, want %q — unaddressed, it is not mail",
			last.To, agent)
	}
	directed, _, _, _, err := UnreadRecords(st.ID, agent, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(directed) != 1 {
		t.Fatalf("%s sees %d directed messages in its DM room, want 1", agent, len(directed))
	}

	// The agent's own reply must NOT be addressed back, or every reply would
	// wake the next turn forever.
	if err := appendRelayDMEvent(agent, relayDMEvent{
		Speaker: agent, Role: "assistant", Text: "yes",
	}); err != nil {
		t.Fatal(err)
	}
	events, _ = readRoomTranscript(st.ID)
	if reply := events[len(events)-1]; reply.To != "" {
		t.Fatalf("the agent's reply is addressed to %q; a reply that is mail cascades", reply.To)
	}
}
