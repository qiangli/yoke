package meet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

// The chat-room relaxation: a room may keep no minutes, and its roster may change
// while it is running. See dhnt/docs/meet-web-chatroom-design.md §4 — one room
// type, from a human plus one assistant up to a chaired panel.

// newRoom is newTestSession's sibling for a room with no secretary and no chair:
// the smallest thing the model now has to support.
func newRoom(t *testing.T) *State {
	t.Helper()
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	t.Setenv("BASHY_CAPABILITY_DIR", t.TempDir())
	fleettest.Ring(t) // the compiled-in tools + test ring, not the developer's store
	// The fixture names its organizer "qiangli", and humanName() reads $USER. Any
	// path that compares the two — the organizer check, and every transport that
	// defaults an actor to the host's human — would otherwise pass or fail
	// depending on who is running the suite.
	t.Setenv("USER", "qiangli")
	old := nowFn
	nowFn = fixedNow
	t.Cleanup(func() { nowFn = old })

	st := &State{
		ID: newID("Chat room", fixedNow()), Topic: "Chat room",
		Participants: []string{"codex"}, Secretary: "", Chair: "",
		Human: "qiangli", Initiator: "qiangli",
		Status: "open", Cwd: t.TempDir(), Created: fixedNow(),
	}
	if err := st.Validate(); err != nil {
		t.Fatalf("a room with one assistant and no secretary must be valid: %v", err)
	}
	if err := st.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	return st
}

// seatEverything makes the routability gate answer yes for any name, so a test
// can seat agents this machine has never installed. The gate itself is the
// subject of TestInviteRefusesAName..., not a precondition of the roster tests.
func seatEverything(t *testing.T) {
	t.Helper()
	old := operableFn
	operableFn = func(string) (bool, string) { return true, "test" }
	t.Cleanup(func() { operableFn = old })
}

// A room with no secretary opens, takes messages, runs a round, and closes —
// without a synthesis pass and without anything pretending one failed.
func TestSecretarylessRoomStartsPostsAndCloses(t *testing.T) {
	st := newRoom(t)

	// The `--dry-run` preview is the first thing a human sees, and it used to
	// resolve the secretary name against PATH — an empty one must read as a
	// deliberate absence, not as an unnamed agent that is "not installed".
	var preview strings.Builder
	printPreview(&preview, st)
	if !strings.Contains(preview.String(), "secretary    (none") {
		t.Errorf("preview must say the room keeps no minutes:\n%s", preview.String())
	}

	if _, err := record(st, "human", st.Human, "human", "morning"); err != nil {
		t.Fatalf("a human post must not need a secretary: %v", err)
	}
	if _, err := runRound(context.Background(), st, "what now?", fakeRunner{reply: "here"}); err != nil {
		t.Fatalf("runRound: %v", err)
	}

	// The synthesis pass has nothing to run with, and says so in terms of the
	// fix rather than of a failure.
	_, err := converge(context.Background(), st, fakeRunner{reply: "SUMMARY:\n- x"})
	if err == nil {
		t.Fatal("converge must refuse a room with no secretary rather than invoking an empty agent name")
	}
	if !strings.Contains(err.Error(), "--secretary") {
		t.Errorf("the refusal must name the recovery: %v", err)
	}

	var out strings.Builder
	path, err := closeMeeting(context.Background(), st, closeOptions{Synthesize: true, Out: &out}, fakeRunner{reply: "unused"})
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if strings.Contains(out.String(), "secretary pass failed") {
		t.Errorf("a room with no secretary has no pass to fail; got %q", out.String())
	}
	if st.Status != "closed" {
		t.Errorf("status = %q, want closed", st.Status)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	md := string(b)
	// Nothing extracted the decisions, and the minutes say exactly that — the
	// one thing they must never do is report "no decision" as a finding.
	if !strings.Contains(md, "NOT EXTRACTED") {
		t.Errorf("minutes must say nothing extracted the decisions:\n%s", md)
	}
	if strings.Contains(md, "the meeting reached no decision") {
		t.Error("an unrecorded room must not file a no-decision finding")
	}
	if strings.Contains(md, "(secretary)") {
		t.Errorf("attendees must not list a secretary that does not exist:\n%s", md)
	}
	if !strings.Contains(md, "codex (participant)") {
		t.Errorf("minutes lost the roster:\n%s", md)
	}
}

// Invite grows the roster, records the change, and — the part that matters —
// every reader of st.Participants picks the new seat up unchanged.
func TestInviteGrowsTheRosterAndIsRecorded(t *testing.T) {
	seatEverything(t)
	st := newRoom(t)

	if err := inviteTo(st, "qiangli", "opencode"); err != nil {
		t.Fatalf("invite: %v", err)
	}
	if got := strings.Join(st.Participants, ","); got != "codex,opencode" {
		t.Fatalf("participants = %q", got)
	}

	// Durable, not just in memory.
	reloaded, err := loadState(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Participants) != 2 {
		t.Fatalf("the roster change must survive a reload: %+v", reloaded.Participants)
	}

	events, _ := readTranscript(st.ID)
	var invite *Event
	for i := range events {
		if events[i].Kind == "invite" {
			invite = &events[i]
		}
	}
	if invite == nil {
		t.Fatalf("no invite event in the transcript: %+v", events)
	}
	if invite.Speaker != "qiangli" || !strings.Contains(invite.Text, "opencode") {
		t.Errorf("invite event = %+v", invite)
	}

	// The reader check: runRound reads st.Participants at call time, so the new
	// seat speaks in the very next round with no change to the round engine.
	evs, err := runRound(context.Background(), st, "go", fakeRunner{reply: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[1].Speaker != "opencode" {
		t.Fatalf("the invited seat must speak in the next round: %+v", evs)
	}
}

func TestKickShrinksTheRoster(t *testing.T) {
	seatEverything(t)
	st := newRoom(t)
	if err := inviteTo(st, "qiangli", "opencode"); err != nil {
		t.Fatal(err)
	}
	if err := kickFrom(st, "qiangli", "codex"); err != nil {
		t.Fatalf("kick: %v", err)
	}
	if got := strings.Join(st.Participants, ","); got != "opencode" {
		t.Fatalf("participants = %q, want opencode", got)
	}
	reloaded, _ := loadState(st.ID)
	if len(reloaded.Participants) != 1 {
		t.Fatalf("the removal must survive a reload: %+v", reloaded.Participants)
	}

	events, _ := readTranscript(st.ID)
	if countKind(events, "kick") != 1 {
		t.Fatalf("want exactly one kick event: %+v", events)
	}

	// Removing somebody who was never seated is a mistake worth reporting, not a
	// silent success on a roster the caller has misread.
	if err := kickFrom(st, "qiangli", "codex"); err == nil {
		t.Error("kicking a non-member must say so")
	}
}

// Inviting somebody already at the table is a no-op. A retried call must not be
// the way a duplicate seat — which dilutes a vote while looking like diversity —
// gets into a roster.
func TestInviteIsIdempotent(t *testing.T) {
	seatEverything(t)
	st := newRoom(t)

	for range 3 {
		if err := inviteTo(st, "qiangli", "codex"); err != nil {
			t.Fatalf("re-inviting a member must be a no-op, got %v", err)
		}
	}
	if len(st.Participants) != 1 {
		t.Fatalf("participants = %+v, want one seat", st.Participants)
	}
	events, _ := readTranscript(st.ID)
	if n := countKind(events, "invite"); n != 0 {
		t.Errorf("a no-op invite must not append an event; got %d", n)
	}
}

// Any member may post; only the organizer changes the roster.
func TestNonOrganizerCannotChangeTheRoster(t *testing.T) {
	seatEverything(t)
	st := newRoom(t) // convened by qiangli

	err := inviteTo(st, "codex", "opencode")
	if !errors.Is(err, ErrNotOrganizer) {
		t.Fatalf("a non-organizer invite must fail with ErrNotOrganizer, got %v", err)
	}
	if len(st.Participants) != 1 {
		t.Fatalf("a refused invite must not mutate the roster: %+v", st.Participants)
	}
	if err := kickFrom(st, "codex", "codex"); !errors.Is(err, ErrNotOrganizer) {
		t.Fatalf("a non-organizer kick must fail with ErrNotOrganizer, got %v", err)
	}
	events, _ := readTranscript(st.ID)
	if len(events) != 0 {
		t.Errorf("a refused roster change must record nothing: %+v", events)
	}

	// The organizer named at start is the one who can.
	if err := inviteTo(st, "qiangli", "opencode"); err != nil {
		t.Fatalf("the organizer must be allowed: %v", err)
	}
}

// A name nothing on this host can be addressed as is refused up front. Seating
// one would mean every round from then on records a failed turn for a
// participant that never existed.
func TestInviteRefusesAnUnroutableName(t *testing.T) {
	st := newRoom(t) // NOT seatEverything: the real gate is the subject here

	if err := inviteTo(st, "qiangli", "definitely-not-an-agent-xyz"); err == nil {
		t.Fatal("an unknown name must be refused")
	}
	if err := inviteTo(st, "qiangli", "  "); err == nil {
		t.Fatal("an empty name must be refused")
	}
	if len(st.Participants) != 1 {
		t.Fatalf("a refused invite must not mutate the roster: %+v", st.Participants)
	}

	// A fleet agent is routable whether or not its binary is on this host —
	// operability is an advisory warning, not a seating refusal.
	if err := inviteTo(st, "qiangli", "claude-opus5"); err != nil {
		t.Fatalf("a fleet agent must be seatable: %v", err)
	}
}

// The role invariants still hold for a seat added at minute forty: a roster
// cannot decay by invitation into one that `start` would have refused.
func TestInviteIsHeldToTheRoleInvariants(t *testing.T) {
	seatEverything(t)
	st := newRoom(t)
	st.Secretary = "claude"
	if err := st.save(); err != nil {
		t.Fatal(err)
	}

	if err := inviteTo(st, "qiangli", "claude"); err == nil {
		t.Fatal("the secretary must not be invitable as a participant")
	}
	if len(st.Participants) != 1 {
		t.Fatalf("a refused invite must not mutate the roster: %+v", st.Participants)
	}
}

// Backward compatibility. A session written before rooms were mutable and before
// the secretary was optional must load unchanged, under the same schema.
func TestPreChangeSessionStillLoads(t *testing.T) {
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	const id = "2026-07-08-finalize-meet-p0-1a2b"

	const legacyState = `{
  "schema": "bashy-meet-v1",
  "id": "2026-07-08-finalize-meet-p0-1a2b",
  "room": 2,
  "topic": "Finalize meet P0",
  "agenda": ["verb set", "filing target"],
  "participants": ["codex", "opencode"],
  "secretary": "claude",
  "chair": "gemini",
  "human": "qiangli",
  "status": "open",
  "cwd": "/tmp/repo",
  "out": "docs",
  "turn_timeout": "20m",
  "created": "2026-07-08T05:40:00Z",
  "round": 2,
  "initiator": "qiangli",
  "decision_mode": "explicit",
  "min_turn_chars": 40,
  "max_turns": 12,
  "max_stalls": 3
}`
	// Two legacy events: no Status field (it postdates them), and no invite/kick.
	const legacyTranscript = `{"round":1,"speaker":"qiangli","role":"human","kind":"human","text":"hello","ts":"2026-07-08T05:41:00Z"}
{"round":1,"speaker":"codex","kind":"turn","text":"codex timed out after 20m","ts":"2026-07-08T05:42:00Z"}
`
	dir, err := storeDir(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacyState), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "transcript.jsonl"), []byte(legacyTranscript), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := loadState(id)
	if err != nil {
		t.Fatalf("a pre-change state.json must still load: %v", err)
	}
	if st.Schema != schemaVersion {
		t.Errorf("schema = %q, want %q — the relaxation is not a schema change", st.Schema, schemaVersion)
	}
	if st.Topic != "Finalize meet P0" || st.Room != 2 || st.Round != 2 {
		t.Errorf("header fields did not survive: %+v", st)
	}
	if strings.Join(st.Participants, ",") != "codex,opencode" {
		t.Errorf("participants = %+v", st.Participants)
	}
	if st.Secretary != "claude" || st.Chair != "gemini" || st.Initiator != "qiangli" {
		t.Errorf("roster did not survive: secretary=%q chair=%q initiator=%q", st.Secretary, st.Chair, st.Initiator)
	}
	if !st.recorded() || !st.chaired() {
		t.Error("a legacy session with both roles filled must still read as recorded and chaired")
	}
	if st.decisionMode() != "explicit" || st.MinTurnChars != 40 || st.maxTurns() != 12 || st.maxStalls() != 3 {
		t.Errorf("policy fields did not survive: %+v", st)
	}
	if err := st.Validate(); err != nil {
		t.Errorf("a legacy roster must still validate: %v", err)
	}

	events, err := readTranscript(id)
	if err != nil {
		t.Fatalf("a pre-change transcript must still load: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Kind != "human" || events[0].Text != "hello" {
		t.Errorf("event 0 = %+v", events[0])
	}
	// Status is reconstructed from the marker text for transcripts written
	// before the field existed — unchanged by this work.
	if got := statusOf(events[1]); got != statusTimeout {
		t.Errorf("statusOf(legacy timeout) = %q, want %q", got, statusTimeout)
	}

	// And it re-saves under the same schema, roster intact.
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	again, err := loadState(id)
	if err != nil {
		t.Fatal(err)
	}
	if again.Schema != schemaVersion || again.Secretary != "claude" || len(again.Participants) != 2 {
		t.Errorf("round-trip lost something: %+v", again)
	}
}

// The CLI surface for the mutable roster.
func TestRosterVerbsAreWired(t *testing.T) {
	names := map[string]bool{}
	for _, c := range NewMeetCmd().Commands() {
		names[c.Name()] = true
	}
	for _, v := range []string{"invite", "kick"} {
		if !names[v] {
			t.Errorf("missing subcommand %q", v)
		}
	}
}

// Invite/Kick reach a room by the same reference a human types at `meet list` —
// a room number, an id, or an unambiguous prefix.
func TestInviteByRoomReference(t *testing.T) {
	seatEverything(t)
	st := newRoom(t)
	st.Room = 1
	if err := st.save(); err != nil {
		t.Fatal(err)
	}

	if err := Invite("1", "qiangli", "opencode"); err != nil {
		t.Fatalf("invite by room number: %v", err)
	}
	reloaded, _ := loadState(st.ID)
	if len(reloaded.Participants) != 2 {
		t.Fatalf("participants = %+v", reloaded.Participants)
	}
	if err := Kick("1", "qiangli", "opencode"); err != nil {
		t.Fatalf("kick by room number: %v", err)
	}
	reloaded, _ = loadState(st.ID)
	if len(reloaded.Participants) != 1 {
		t.Fatalf("participants = %+v", reloaded.Participants)
	}
}
