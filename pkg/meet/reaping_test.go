package meet

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/chat"
)

// spawnGuard is a chat.Runner that fails the test the instant it is asked to run
// an agent. `abandon` reaps a dead room; it must SPAWN NOTHING, so wiring this in
// as the api runner turns any accidental converge/confirm/turn into a test
// failure rather than a silent, expensive regression.
type spawnGuard struct{ t *testing.T }

func (g spawnGuard) Run(_ context.Context, agent string, _ []string, _ string) (string, int, error) {
	g.t.Fatalf("abandon must spawn nothing, but a runner was invoked for %q", agent)
	return "", 0, nil
}

// guardSpawns points the api runner at a spawnGuard for the duration of a test.
func guardSpawns(t *testing.T) {
	t.Helper()
	old := apiRunner
	apiRunner = func() chat.Runner { return spawnGuard{t} }
	t.Cleanup(func() { apiRunner = old })
}

// runAbandon drives the reap through its registered cobra verb, so its name,
// parsing, and dispatch are exercised — not just the RunE body.
func runAbandon(t *testing.T, ref string) string {
	t.Helper()
	cmd := NewMeetCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"abandon", ref})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("abandon %s: %v", ref, err)
	}
	return out.String()
}

// A room dead for weeks is reaped, not concluded: abandon marks it abandoned,
// releases its number, archives the transcript, and spawns/synthesizes/files
// nothing.
func TestAbandonReapsRoomSpawnsNothing(t *testing.T) {
	guardSpawns(t)
	st := newTestSession(t)
	st.Room = 1
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	st.Cwd = repo
	_ = st.save()
	// The room had a real discussion — abandon must still not run the secretary.
	if _, err := runRound(context.Background(), st, "q", fakeRunner{reply: "argued"}); err != nil {
		t.Fatal(err)
	}

	out := runAbandon(t, st.ID)
	if !strings.Contains(out, "abandoned") || !strings.Contains(out, "released room 1") {
		t.Errorf("abandon should report the reap and the freed room: %q", out)
	}

	reloaded, err := loadState(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Reads differently from a concluded room: "abandoned", never "closed".
	if reloaded.Status != "abandoned" {
		t.Errorf("status = %q, want abandoned (must not read like a concluded room)", reloaded.Status)
	}
	if reloaded.Room != 0 {
		t.Errorf("room = %d, want 0 (the number must be released)", reloaded.Room)
	}

	// The room number is genuinely free for reuse.
	sessions, _ := listSessions()
	if got := lowestFreeRoom(sessions); got != 1 {
		t.Errorf("lowest free room = %d, want 1 (the abandoned room's number must be reusable)", got)
	}

	// Nothing was filed: no minutes document, in this repo or anywhere.
	if _, err := os.Stat(minutesPath(reloaded)); !os.IsNotExist(err) {
		t.Errorf("abandon must file no minutes, but %s exists (%v)", minutesPath(reloaded), err)
	}
	if _, err := os.Stat(filepath.Join(repo, "docs", "meetings")); !os.IsNotExist(err) {
		t.Errorf("abandon must not create docs/meetings/ in the room's repo")
	}
	// No confirm event — that marker is what a deliberate CONCLUSION records.
	events, _ := readTranscript(st.ID) // store transcript is now archived → empty
	if countKind(events, "confirm") != 0 {
		t.Errorf("abandon must record no confirm event: %+v", events)
	}
}

// Abandon is NOT a delete: the transcript survives under archive/, carrying the
// marker that says the room was abandoned rather than concluded.
func TestAbandonPreservesTranscriptUnderArchive(t *testing.T) {
	guardSpawns(t)
	st := newTestSession(t)
	_ = st.save()
	if _, err := runRound(context.Background(), st, "q", fakeRunner{reply: "the argument"}); err != nil {
		t.Fatal(err)
	}

	runAbandon(t, st.ID)

	dir, err := storeDir(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The live transcript moved out of the store, exactly as a reopen archives it.
	if _, err := os.Stat(filepath.Join(dir, "transcript.jsonl")); !os.IsNotExist(err) {
		t.Errorf("abandon should archive the live transcript out of the store")
	}
	archived, err := filepath.Glob(filepath.Join(dir, "archive", "*", "transcript.jsonl"))
	if err != nil || len(archived) != 1 {
		t.Fatalf("expected exactly one archived transcript, got %v (%v)", archived, err)
	}
	b, err := os.ReadFile(archived[0])
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if !strings.Contains(body, "the argument") {
		t.Errorf("the archived transcript must preserve the discussion:\n%s", body)
	}
	if !strings.Contains(body, "abandoned") {
		t.Errorf("the archived transcript must record that the room was abandoned:\n%s", body)
	}
}

// A room that never held a discussion files nothing on close — closing it still
// releases the room and marks it closed, but writing NOT-EXTRACTED boilerplate
// for an empty transcript (into a repo, no less) is pure noise.
func TestFileMinutesSkipsEmptyRoom(t *testing.T) {
	st := newTestSession(t)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	st.Cwd = repo
	t.Chdir(repo)
	// Only scaffolding — an agenda marker, no turn/vote/post/decision.
	if _, err := record(st, "agenda", procedural(st), string(RoleChair), "verb set"); err != nil {
		t.Fatal(err)
	}
	_ = st.save()

	path, err := fileMinutes(st)
	if err != nil {
		t.Fatalf("closing an empty room must not error: %v", err)
	}
	if _, err := os.Stat(minutesPath(st)); !os.IsNotExist(err) {
		t.Errorf("an empty room must file no minutes, but %s exists", minutesPath(st))
	}
	if _, err := os.Stat(filepath.Join(repo, "docs", "meetings")); !os.IsNotExist(err) {
		t.Errorf("an empty room must not create docs/meetings/")
	}
	if st.Status != "closed" {
		t.Errorf("status = %q, want closed (an empty room still closes)", st.Status)
	}
	if st.Room != 0 {
		t.Errorf("room = %d, want 0 (an empty room still releases its number)", st.Room)
	}
	// The return points into the store, where the transcript still lives.
	if dir, _ := storeDir(st.ID); path != dir {
		t.Errorf("skip should return the store dir %q, got %q", dir, path)
	}
}

// A room that DID have turns but no secretary still files the NOT-EXTRACTED
// notice — that notice is deliberate and correct, and the empty-room skip must
// not swallow it.
func TestFileMinutesFilesTurnsOnlyRoom(t *testing.T) {
	st := newTestSession(t)
	st.Secretary = "" // no recorder
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	st.Cwd = repo
	t.Chdir(repo)
	_ = st.save()
	if _, err := runRound(context.Background(), st, "q", fakeRunner{reply: "a real contribution"}); err != nil {
		t.Fatal(err)
	}

	path, err := fileMinutes(st)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("a room with turns must file minutes: %v", err)
	}
	if !strings.Contains(string(b), "NOT EXTRACTED") {
		t.Errorf("a secretary-less room with turns must keep the NOT-EXTRACTED notice:\n%s", b)
	}
}

// Closing a room from a different repo than it was opened in must refuse and name
// the path, never silently drop a note into a tree nobody is standing in.
func TestFileMinutesRefusesForeignRepo(t *testing.T) {
	st := newTestSession(t)
	opened := t.TempDir() // the repo the room was opened in
	if err := os.Mkdir(filepath.Join(opened, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	st.Cwd = opened
	_ = st.save()
	if _, err := runRound(context.Background(), st, "q", fakeRunner{reply: "argued"}); err != nil {
		t.Fatal(err)
	}

	// The caller is standing in a DIFFERENT repo.
	elsewhere := t.TempDir()
	if err := os.Mkdir(filepath.Join(elsewhere, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(elsewhere)

	_, err := fileMinutes(st)
	if err == nil {
		t.Fatal("closing into a foreign repo must refuse, not write silently")
	}
	if !strings.Contains(err.Error(), "refusing to file minutes") {
		t.Errorf("the refusal must be explicit: %v", err)
	}
	// It must name the path it declined to write.
	if !strings.Contains(err.Error(), "docs") {
		t.Errorf("the refusal must name the destination path: %v", err)
	}
	// Nothing was written into either repo, and the room stays open (unclosed).
	if _, err := os.Stat(filepath.Join(opened, "docs", "meetings")); !os.IsNotExist(err) {
		t.Errorf("the room's original repo must not be written into")
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "docs", "meetings")); !os.IsNotExist(err) {
		t.Errorf("the caller's repo must not be written into either")
	}
	reloaded, _ := loadState(st.ID)
	if reloaded.Status == "closed" {
		t.Error("a refused close must leave the room open — the transcript stays in the store")
	}
}
