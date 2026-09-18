package meet

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Board mode: a room where participants read and post on their OWN turns. No
// chair runs the floor and no secretary is spawned. The turn model still follows
// from the roster; Board only says the orchestrator never drives a turn itself.

// Validate gains exactly one board rule: a board cannot be chaired. A chair calls
// on speakers and nobody in a board is callable, so the two contradict.
func TestBoardRefusesAChair(t *testing.T) {
	st := &State{
		Topic: "board", Participants: []string{"codex"},
		Chair: "claude", Human: "qiangli", Board: true, Status: "open",
	}
	err := st.Validate()
	if err == nil || !strings.Contains(err.Error(), "board has no facilitator-driven floor") {
		t.Fatalf("Board && chair must be refused naming the conflict: %v", err)
	}
	// Drop the chair and the same board validates.
	st.Chair = ""
	if err := st.Validate(); err != nil {
		t.Fatalf("a board with no chair is a valid room: %v", err)
	}
}

// `meet open` exposes --board; `meet consult` does not — a board is an open-only
// room type, not a one-shot panel mode.
func TestOpenCmdHasBoardFlag(t *testing.T) {
	if newOpenCmd().Flags().Lookup("board") == nil {
		t.Error("meet open must expose --board")
	}
	if newConsultCmd().Flags().Lookup("board") != nil {
		t.Error("meet consult must not expose --board")
	}
}

// The CLI default --secretary "claude" is a meeting default, not a board one:
// opening a board clears it so no secretary is seated or spawned.
func TestBoardNewStateClearsSecretary(t *testing.T) {
	newRoom(t)
	sf := sessionFlags{topic: "board", participants: []string{"codex"}, secretary: "claude", board: true}
	st, err := sf.newState()
	if err != nil {
		t.Fatalf("newState: %v", err)
	}
	if !st.Board {
		t.Error("Board flag not carried onto the State")
	}
	if st.Secretary != "" {
		t.Errorf("board seated a secretary %q; a board keeps no minutes", st.Secretary)
	}
}

// A board never arms a pending secretary, even when the host CAN select one:
// Create forces NoSecretary and the selector must never be reached.
func TestBoardNeverArmsASecretary(t *testing.T) {
	newRoom(t)
	old := StartRoomSecretary
	StartRoomSecretary = func(context.Context, RoomSecretaryStartRequest) (string, error) {
		t.Fatal("a board must never select a secretary")
		return "", nil
	}
	t.Cleanup(func() { StartRoomSecretary = old })

	st, err := Create(CreateOptions{Topic: "board", Participants: []string{"codex"}, Board: true})
	if err != nil {
		t.Fatalf("Create board: %v", err)
	}
	if !st.Board {
		t.Error("Board flag not persisted")
	}
	if st.SecretaryPending {
		t.Error("a board armed a pending secretary")
	}
	if st.Secretary != "" {
		t.Errorf("a board seated a secretary %q", st.Secretary)
	}
	// Belt-and-braces: even if a board slipped through with SecretaryPending set,
	// activation refuses to spawn.
	st.SecretaryPending = true
	if err := ensureRoomSecretary(context.Background(), st); err != nil {
		t.Fatalf("ensureRoomSecretary on a board must be a quiet no-op: %v", err)
	}
}

// The chair-driven verbs refuse a board, naming the mode and the alternative.
func TestBoardRefusesChairDrivenVerbs(t *testing.T) {
	newRoom(t)
	st, err := Create(CreateOptions{Topic: "board", Participants: []string{"codex"}, Board: true})
	if err != nil {
		t.Fatalf("Create board: %v", err)
	}
	ctx := context.Background()

	check := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s on a board must refuse", name)
		}
		msg := err.Error()
		if !strings.Contains(msg, "is a board") || !strings.Contains(msg, "meet tell") {
			t.Errorf("%s refusal must name the mode and the `meet tell` alternative: %v", name, err)
		}
	}
	_, err = Round(ctx, st.ID)
	check("Round", err)
	_, err = Poll(ctx, st.ID, "ship?", nil)
	check("Poll", err)
	_, err = Ask(ctx, st.ID, "thoughts?")
	check("Ask", err)

	// The engine's own round entry point refuses too — belt-and-braces with the
	// api guards so no caller can spawn a round-robin over a board.
	_, err = runRound(ctx, st, "", nil)
	check("runRound", err)
}

// A board never spawns a participant, not even to confirm its own conclusion.
// An agent initiator falls through to the attended path, which concludes only on
// a terminal answer or an explicit --yes — never by launching the agent.
func TestBoardCloseDoesNotSpawnInitiator(t *testing.T) {
	newRoom(t)
	st, err := Create(CreateOptions{
		Topic: "board", Participants: []string{"codex"},
		Initiator: "codex", Board: true,
	})
	if err != nil {
		t.Fatalf("Create board: %v", err)
	}
	if st.initiatorKind() != "agent" {
		t.Fatalf("precondition: initiator should be an agent, got %q", st.initiatorKind())
	}
	var buf bytes.Buffer
	err = confirmConclusion(context.Background(), st, strings.NewReader(""), &buf, false, nil)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("a board must not launch its agent initiator to confirm; want the attended --yes refusal, got %v", err)
	}
}

// The reopen cursor bug: Open archives transcript.jsonl but must ALSO archive the
// per-participant read cursors. Ordinals restart at 1 on reopen; a seen/<name>
// left at the prior high-water mark, with MarkSeen never moving backwards, leaves
// every participant permanently "caught up" and blind to every new message.
func TestReopenArchivesReadCursors(t *testing.T) {
	st := newRoom(t) // organizer qiangli, participant codex, no secretary

	dir, err := storeDir(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A read cursor from the first session, at a high-water mark.
	seen := filepath.Join(dir, "seen")
	if err := os.MkdirAll(seen, 0o755); err != nil {
		t.Fatal(err)
	}
	cursor := filepath.Join(seen, "codex")
	if err := os.WriteFile(cursor, []byte("5"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Post(st.ID, "qiangli", "first session message"); err != nil {
		t.Fatalf("post: %v", err)
	}
	if err := Close(st.ID, "qiangli"); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(st.ID, "qiangli")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := Post(reopened.ID, "qiangli", "second session message"); err != nil {
		t.Fatalf("post after reopen: %v", err)
	}

	// The live cursor must be gone: reopening reset the read state along with the
	// ordinals. Without the fix seen/codex survives and codex never reads again.
	if _, err := os.Stat(cursor); !os.IsNotExist(err) {
		t.Fatalf("reopen must archive read cursors with the transcript; seen/codex still live (%v) "+
			"leaves every participant permanently caught up and never seeing another message", err)
	}
	// It is archived beside the transcript it belonged to, not destroyed.
	archived, _ := filepath.Glob(filepath.Join(dir, "archive", "*", "seen", "codex"))
	if len(archived) == 0 {
		t.Error("the prior session's read cursor should be retained under archive/<ts>/seen/")
	}
}
