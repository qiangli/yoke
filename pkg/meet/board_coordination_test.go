package meet

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// Board coordination (issue 18): seeding a board from mb, open invites the
// organizer delegates seating with, and the outcome a board reports back when it
// closes. pkg/meet must not import pkg/bus, so mb rides two package-level seams
// (FetchMB read, PostMB write) that bashy wires; these tests substitute them.

// recordMB installs a recording PostMB (and the given FetchMB) and returns the
// captured posts and audiences. PostMB is ALWAYS non-nil here — the "no seam
// wired" case is set up directly with clearMBSeam.
func recordMB(t *testing.T, fetch func([]int64) ([]MBPost, error)) (posted *[]MBPost, audiences *[]*OpenInvite) {
	t.Helper()
	pf, pp := FetchMB, PostMB
	var recPosts []MBPost
	var recAud []*OpenInvite
	FetchMB = fetch
	PostMB = func(p MBPost, aud *OpenInvite) (int64, error) {
		recPosts = append(recPosts, p)
		recAud = append(recAud, aud)
		return int64(len(recPosts)), nil
	}
	t.Cleanup(func() { FetchMB, PostMB = pf, pp })
	return &recPosts, &recAud
}

// clearMBSeam removes both mb seam directions — the bare-embedding case.
func clearMBSeam(t *testing.T) {
	t.Helper()
	pf, pp := FetchMB, PostMB
	FetchMB, PostMB = nil, nil
	t.Cleanup(func() { FetchMB, PostMB = pf, pp })
}

// withAudienceMatch injects the fleet-selection seam self-seat rides, so the
// matching is exercised hermetically rather than against whichever agent CLIs
// happen to be installed on the host running the suite.
func withAudienceMatch(t *testing.T, match func(string, OpenInvite) bool) {
	t.Helper()
	prev := AudienceMatch
	AudienceMatch = match
	t.Cleanup(func() { AudienceMatch = prev })
}

func boardRoom(t *testing.T) *State {
	t.Helper()
	st := newRoom(t) // organizer qiangli, participant codex
	st.Board = true
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	return st
}

// (a) --from-mb seeds the board attributed to the ORIGINAL authors and posts a
// broadcast pointer BACK to mb — the room is the correlation id mb lacked.
func TestSeedBoardFromMBAttributesAndPointsBack(t *testing.T) {
	st := boardRoom(t)
	var gotSeqs []int64
	posted, auds := recordMB(t,
		func(seqs []int64) ([]MBPost, error) {
			gotSeqs = seqs
			return []MBPost{
				{Seq: 3, From: "codex", Topic: "gate", Body: "gate is red on main"},
				{Seq: 7, From: "claude", Body: "on it"},
			}, nil
		})

	pointerPosted, err := SeedBoardFromMB(st, []int64{3, 7})
	if err != nil {
		t.Fatalf("SeedBoardFromMB: %v", err)
	}
	if !pointerPosted {
		t.Fatal("SeedBoardFromMB reported no pointer while PostMB was wired")
	}
	if len(gotSeqs) != 2 || gotSeqs[0] != 3 || gotSeqs[1] != 7 {
		t.Fatalf("fetched seqs = %v", gotSeqs)
	}
	events, err := readTranscript(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 seeded events, got %d: %+v", len(events), events)
	}
	if events[0].Speaker != "codex" || events[1].Speaker != "claude" {
		t.Fatalf("seeded events must keep original authors, got %q and %q", events[0].Speaker, events[1].Speaker)
	}
	if events[0].Origin == nil || events[0].Origin.Source != "mb" || events[0].Origin.Seq != 3 ||
		events[1].Origin == nil || events[1].Origin.Source != "mb" || events[1].Origin.Seq != 7 {
		t.Fatalf("seeded events lost durable mb provenance: %+v", events)
	}
	if !strings.Contains(events[0].Text, "mb #3") || !strings.Contains(events[0].Text, "gate is red") {
		t.Fatalf("seed text = %q", events[0].Text)
	}
	if len(*posted) != 1 {
		t.Fatalf("want one pointer post back to mb, got %d", len(*posted))
	}
	if (*auds)[0] != nil {
		t.Fatalf("the pointer back is a broadcast, not a group post: audience = %+v", (*auds)[0])
	}
	if !strings.Contains((*posted)[0].Body, st.roomRef()) || !strings.Contains((*posted)[0].Body, "#3") {
		t.Fatalf("pointer body must name the room and the seqs: %q", (*posted)[0].Body)
	}
}

// --from-mb needs the board and needs the seam; both failures are loud, never a
// silently empty room.
func TestSeedBoardFromMBGuards(t *testing.T) {
	st := boardRoom(t)

	// No seam wired: refuse rather than seed nothing.
	clearMBSeam(t)
	if _, err := SeedBoardFromMB(st, []int64{1}); err == nil || !strings.Contains(err.Error(), "no message-board seam") {
		t.Fatalf("missing seam error = %v", err)
	}

	// Not a board.
	recordMB(t, func([]int64) ([]MBPost, error) { return nil, nil })
	meeting := newRoom(t)
	if _, err := SeedBoardFromMB(meeting, []int64{1}); err == nil || !strings.Contains(err.Error(), "not one") {
		t.Fatalf("non-board seed error = %v", err)
	}
}

// The CLI enforces --board before it ever reaches the seam.
func TestOpenFromMBRequiresBoardFlag(t *testing.T) {
	newRoom(t) // establish the meet dir + $USER
	t.Setenv(meetDepthEnv, "")
	cmd := NewMeetCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"open", "--topic", "x", "--from-mb", "3,7"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "pass --board") {
		t.Fatalf("--from-mb without --board error = %v", err)
	}
}

// The full CLI open path: --board --from-mb seeds, points back, and reports it.
func TestCobraOpenBoardFromMB(t *testing.T) {
	newRoom(t)
	t.Setenv(meetDepthEnv, "")
	posted, _ := recordMB(t,
		func([]int64) ([]MBPost, error) {
			return []MBPost{{Seq: 9, From: "codex", Body: "kickoff"}}, nil
		})

	cmd := NewMeetCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetIn(strings.NewReader("")) // empty stdin: the REPL exits at EOF
	cmd.SetArgs([]string{"open", "--board", "--topic", "coord", "--participant", "codex", "--from-mb", "9"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("open --board --from-mb: %v", err)
	}
	if !strings.Contains(out.String(), "seeded board") || !strings.Contains(out.String(), "#9") {
		t.Fatalf("open output = %q", out.String())
	}
	if len(*posted) != 1 {
		t.Fatalf("open --from-mb must post a pointer back, got %d", len(*posted))
	}
}

// (b) An open invite records the audience on State.OpenTo and posts a GROUP
// invite (non-nil audience) carrying mb's own selector.
func TestCobraInviteOpensSeatingToAudience(t *testing.T) {
	st := boardRoom(t)
	posted, auds := recordMB(t, nil)

	cmd := NewMeetCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"invite", st.ID, "--tool", "codex"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("invite --tool: %v", err)
	}
	reloaded, err := loadState(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.OpenTo == nil || reloaded.OpenTo.Tool != "codex" {
		t.Fatalf("OpenTo not recorded: %+v", reloaded.OpenTo)
	}
	if len(*posted) != 1 || (*auds)[0] == nil || (*auds)[0].Tool != "codex" {
		t.Fatalf("group invite audience = %+v", *auds)
	}
	// The `open` roster event is the audit trail of who opened the door.
	events, _ := readTranscript(st.ID)
	found := false
	for _, e := range events {
		if e.Kind == "open" {
			found = true
		}
	}
	if !found {
		t.Fatalf("opening seating must record an `open` event: %+v", events)
	}
}

// --any is the "anyone" audience and cannot be stacked with selectors.
func TestOpenInviteAnyRejectsSelectors(t *testing.T) {
	st := boardRoom(t)
	if _, err := SetOpenTo(st.ID, "qiangli", OpenInvite{Any: true, Band: 4}); err == nil ||
		!strings.Contains(err.Error(), "drop the selectors") {
		t.Fatalf("--any + selector error = %v", err)
	}
}

// An open invite is a board act: a meeting that spawns turns refuses it.
func TestOpenInviteRefusedOnMeeting(t *testing.T) {
	st := newRoom(t) // not a board
	if _, err := SetOpenTo(st.ID, "qiangli", OpenInvite{Any: true}); err == nil ||
		!strings.Contains(err.Error(), "not a board") {
		t.Fatalf("open invite on a meeting error = %v", err)
	}
}

// An empty invite (no agent, no selector) is neither a push nor a delegation.
func TestOpenInviteNeedsAnAudience(t *testing.T) {
	st := boardRoom(t)
	if _, err := SetOpenTo(st.ID, "qiangli", OpenInvite{}); err == nil ||
		!strings.Contains(err.Error(), "must name an audience") {
		t.Fatalf("empty open invite error = %v", err)
	}
}

// Direct push and open delegation are different intents; giving both is refused.
func TestInviteAgentAndSelectorConflict(t *testing.T) {
	st := boardRoom(t)
	recordMB(t, nil)
	cmd := NewMeetCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"invite", st.ID, "codex", "--any"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("agent + selector error = %v", err)
	}
}

// A matching agent self-seats on its first post, recorded as a `join` event so
// the roster shows who came.
func TestSelfSeatOnMatchingOpenInvite(t *testing.T) {
	st := boardRoom(t)
	st.Participants = []string{"opencode"} // codex is NOT seated
	st.OpenTo = &OpenInvite{Tool: "codex"}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	withAudienceMatch(t, func(agent string, inv OpenInvite) bool {
		return inv.Tool != "" && strings.EqualFold(agent, inv.Tool)
	})

	ev, err := PostAs(st.ID, "codex", "", "self-seating now")
	if err != nil {
		t.Fatalf("matching agent must self-seat, got %v", err)
	}
	if ev.Kind != "message" {
		t.Fatalf("post event = %+v", ev)
	}
	reloaded, _ := loadState(st.ID)
	if !participantSeat(reloaded, "codex") {
		t.Fatalf("codex did not get a seat: %v", reloaded.Participants)
	}
	events, _ := readTranscript(st.ID)
	if len(events) < 2 || events[0].Kind != "join" || events[0].Speaker != "codex" {
		t.Fatalf("first event must be codex's join: %+v", events)
	}
}

func TestOversizedFirstPostDoesNotSelfSeatOpenBoard(t *testing.T) {
	st := boardRoom(t)
	st.Participants = []string{"opencode"}
	st.OpenTo = &OpenInvite{Tool: "codex"}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	withAudienceMatch(t, func(agent string, inv OpenInvite) bool { return true })
	if _, err := PostAs(st.ID, "codex", "", strings.Repeat("x", 1025)); err == nil {
		t.Fatal("oversized first post was accepted")
	}
	reloaded, err := loadState(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if participantSeat(reloaded, "codex") {
		t.Fatalf("rejected first post self-seated codex: %v", reloaded.Participants)
	}
	if events, err := readTranscript(st.ID); err != nil || len(events) != 0 {
		t.Fatalf("rejected first post mutated transcript: events=%d err=%v", len(events), err)
	}
}

// A non-matching agent is refused exactly as before — a declared audience is a
// gate, not an open door.
func TestSelfSeatRefusesNonMatch(t *testing.T) {
	st := boardRoom(t)
	st.Participants = []string{"opencode"}
	st.OpenTo = &OpenInvite{Tool: "claude"} // codex is on tool codex, not claude
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	withAudienceMatch(t, func(agent string, inv OpenInvite) bool {
		return inv.Tool != "" && strings.EqualFold(agent, inv.Tool)
	})
	if _, err := PostAs(st.ID, "codex", "", "let me in"); err == nil ||
		!strings.Contains(err.Error(), "has no seat") {
		t.Fatalf("non-matching self-seat error = %v", err)
	}
}

// With no open invite declared, seating stays organizer-push-only: the default
// does not change.
func TestNoOpenInviteStaysPushOnly(t *testing.T) {
	st := boardRoom(t)
	st.Participants = []string{"opencode"}
	st.OpenTo = nil
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := PostAs(st.ID, "codex", "", "knock knock"); err == nil ||
		!strings.Contains(err.Error(), "has no seat") {
		t.Fatalf("push-only board must refuse an unseated poster: %v", err)
	}
}

// (c) A closed board reports its outcome back to mb, keyed by the room id.
func TestBoardCloseReportsOutcomeToMB(t *testing.T) {
	st := boardRoom(t)
	if _, err := PostAs(st.ID, "codex", "", "one message"); err != nil {
		t.Fatal(err)
	}
	posted, auds := recordMB(t, nil)

	if err := Close(st.ID, "qiangli"); err != nil {
		t.Fatalf("close board: %v", err)
	}
	if len(*posted) != 1 {
		t.Fatalf("a closed board must report exactly one outcome, got %d", len(*posted))
	}
	if (*auds)[0] != nil {
		t.Fatalf("the outcome is a broadcast, got audience %+v", (*auds)[0])
	}
	body := (*posted)[0].Body
	if !strings.Contains(body, "board closed") || !strings.Contains(body, st.ID) {
		t.Fatalf("outcome must name the room id: %q", body)
	}
}

// An ordinary meeting does not post a board outcome — only boards report back.
func TestMeetingCloseDoesNotReportToMB(t *testing.T) {
	st := newRoom(t) // not a board, no secretary
	posted, _ := recordMB(t, nil)
	if err := Close(st.ID, "qiangli"); err != nil {
		t.Fatalf("close meeting: %v", err)
	}
	if len(*posted) != 0 {
		t.Fatalf("a meeting must not post a board outcome, got %d", len(*posted))
	}
}

func TestParseSeqList(t *testing.T) {
	got, err := parseSeqList(" 3, #7 ,12 ")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 3 || got[0] != 3 || got[1] != 7 || got[2] != 12 {
		t.Fatalf("parsed = %v", got)
	}
	if seqs, err := parseSeqList(""); err != nil || seqs != nil {
		t.Fatalf("empty = %v, %v", seqs, err)
	}
	for _, bad := range []string{"x", "0", "-3", "3,x"} {
		if _, err := parseSeqList(bad); err == nil {
			t.Fatalf("%q must be a usage error", bad)
		}
	}
}

// The shipped defect this pins: bashy wired meet.FetchMB but not meet.PostMB,
// so seeding SUCCEEDED, the pointer silently no-oped, and the CLI printed
// "posted a pointer back" regardless. Anyone still on mb would never learn the
// thread had moved. Seeding without a write seam must report pointerPosted
// false so the caller can say so.
func TestSeedBoardFromMBReportsAnUnwiredPointer(t *testing.T) {
	st := testState()
	st.Board = true
	pinStore(t, st)

	origFetch, origPost := FetchMB, PostMB
	t.Cleanup(func() { FetchMB, PostMB = origFetch, origPost })
	FetchMB = func([]int64) ([]MBPost, error) {
		return []MBPost{{Seq: 3, From: "codex-profile-c", Body: "the original claim"}}, nil
	}
	PostMB = nil

	posted, err := SeedBoardFromMB(st, []int64{3})
	if err != nil {
		t.Fatalf("seeding must still succeed without a write seam: %v", err)
	}
	if posted {
		t.Fatal("reported a pointer as posted while PostMB was nil")
	}

	events, err := readTranscript(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	var seeded bool
	for _, e := range events {
		if e.Kind == "message" && strings.Contains(e.Text, "the original claim") {
			seeded = true
			if e.Speaker != canonAgent("codex-profile-c") {
				t.Fatalf("seeded post lost its original author: %q", e.Speaker)
			}
		}
	}
	if !seeded {
		t.Fatal("the fetch half must still seed the room")
	}
}

// Invariant 4: anything that travels to mb and is read on a LATER turn must
// carry the durable id. Room numbers are the lowest free number among open
// meetings and are reused on close, so a pointer naming only "room 2" can
// address a different room by the time it is read — and the reader cannot tell.
func TestMBBoundBodiesCarryTheDurableID(t *testing.T) {
	st := boardRoom(t)
	st.Room = 2
	posted, _ := recordMB(t, func([]int64) ([]MBPost, error) {
		return []MBPost{{Seq: 3, From: "codex", Body: "the claim"}}, nil
	})

	if _, err := SeedBoardFromMB(st, []int64{3}); err != nil {
		t.Fatal(err)
	}
	if len(*posted) != 1 {
		t.Fatalf("expected one pointer post, got %d", len(*posted))
	}
	body := (*posted)[0].Body
	if !strings.Contains(body, st.ID) {
		t.Fatalf("mb pointer does not carry the durable id %q:\n%s", st.ID, body)
	}
}

// The board's access contract, in both directions: READING is open to anyone,
// POSTING needs a seat.
//
// The read half is not a preference. A board that seeds from mb posts
// "read: bashy meet read <id> --as <you>" back to the thread, and every reader
// of that pointer is by definition unseated -- so gating the read would make
// the invitation instruct people to run a command that refuses them. It also
// bought nothing: `meet observe` already renders the same transcript to any
// caller, so the gate was a bypassable inconvenience, not a boundary.
func TestBoardReadIsOpenButPostingNeedsASeat(t *testing.T) {
	st := boardRoom(t)
	if err := AppendEvent(st.ID, Event{
		Round: st.Round, Speaker: "codex", Role: string(RoleParticipant),
		Kind: "message", Text: "the seeded context", TS: nowFn(),
	}); err != nil {
		t.Fatal(err)
	}

	read := newReadCmd()
	var out, errW bytes.Buffer
	read.SetOut(&out)
	read.SetErr(&errW)
	read.SetArgs([]string{st.ID, "--as", "stranger"})
	if err := read.Execute(); err != nil {
		t.Fatalf("an unseated reader must be able to read a board: %v", err)
	}
	if !strings.Contains(out.String(), "the seeded context") {
		t.Fatalf("unseated read returned no content:\n%s", out.String())
	}

	tell := newTellCmd()
	var tOut, tErr bytes.Buffer
	tell.SetOut(&tOut)
	tell.SetErr(&tErr)
	tell.SetArgs([]string{st.ID, "--as", "stranger", "posting without a seat"})
	err := tell.Execute()
	if err == nil {
		t.Fatal("an unseated caller must not be able to POST to a board")
	}
	if !strings.Contains(err.Error(), "no seat") {
		t.Fatalf("refusal should name the missing seat, got: %v", err)
	}
}

// A board's ONE promise is that nothing is spawned. The guard therefore has to
// live in runPoll/runAsk — the single implementation — not only on the Poll/Ask
// wrappers: the CLI verbs, the REPL and the --participant forms all call the
// implementation directly, and with the guard on the wrappers alone
// `bashy meet poll <board> --question ... --choice ...` ran past the mode check
// and invoked a model on a room that promised it would not.
func TestBoardRefusesEveryChairDrivenEntryPoint(t *testing.T) {
	st := boardRoom(t)
	if err := inviteTo(st, "qiangli", "codex"); err != nil {
		t.Fatal(err)
	}

	// A runner that fails the test if anything reaches it: the refusal must
	// happen BEFORE a participant is ever invoked.
	trip := &tripRunner{}

	if _, err := runPoll(context.Background(), st, "ready?", []string{"yes", "no"}, st.Participants, trip); err == nil {
		t.Error("runPoll must refuse on a board")
	} else if !strings.Contains(err.Error(), "is a board") {
		t.Errorf("runPoll refusal must name the mode, got: %v", err)
	}
	if _, err := runAsk(context.Background(), st, "thoughts?", true, st.Participants, trip); err == nil {
		t.Error("runAsk must refuse on a board")
	} else if !strings.Contains(err.Error(), "is a board") {
		t.Errorf("runAsk refusal must name the mode, got: %v", err)
	}
	if trip.ran {
		t.Fatal("a board spawned a participant — the mode's one promise is that nothing is spawned")
	}

	// The mode check must precede argument validation: telling the caller to
	// supply a --question implies that supplying one would work.
	if _, err := runPoll(context.Background(), st, "", nil, st.Participants, trip); err == nil ||
		!strings.Contains(err.Error(), "is a board") {
		t.Errorf("an empty question on a board must still refuse by MODE, got: %v", err)
	}
}

// tripRunner fails the test if a board ever reaches the point of invoking one.
type tripRunner struct{ ran bool }

func (r *tripRunner) Run(_ context.Context, _ string, _ []string, _ string) (string, int, error) {
	r.ran = true
	return "", 0, nil
}
