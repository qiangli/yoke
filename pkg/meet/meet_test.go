package meet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

// fakeRunner returns a canned reply without spawning a real agent, so the whole
// engine is hermetic (no network, no agent CLIs).
type fakeRunner struct{ reply string }

func (f fakeRunner) Run(_ context.Context, agent string, _ []string, _ string) (string, int, error) {
	return f.reply + " [" + agent + "]", 0, nil
}

func fixedNow() time.Time { return time.Date(2026, 7, 8, 5, 40, 0, 0, time.UTC) }

func newTestSession(t *testing.T) *State {
	t.Helper()
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	t.Setenv("BASHY_CAPABILITY_DIR", t.TempDir()) // isolate the operability auto-record at close
	fleettest.Ring(t)                             // the agents that auto-record resolve against
	old := nowFn
	nowFn = fixedNow
	t.Cleanup(func() { nowFn = old })
	st := &State{
		ID: newID("Finalize meet P0", fixedNow()), Topic: "Finalize meet P0",
		Agenda: []string{"verb set"}, Secretary: "claude",
		Participants: []string{"codex", "opencode"}, Human: "qiangli", Initiator: "qiangli",
		Status: "open", Cwd: t.TempDir(), Created: fixedNow(),
	}
	if err := st.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	return st
}

func TestSlugifyAndID(t *testing.T) {
	if got := slugify("  Finalize bashy meet P0!!  "); got != "finalize-bashy-meet-p0" {
		t.Fatalf("slugify = %q", got)
	}
	id := newID("Finalize meet P0", fixedNow())
	if !strings.HasPrefix(id, "2026-07-08-finalize-meet-p0-") {
		t.Fatalf("id = %q", id)
	}
	if newID("Finalize meet P0", fixedNow()) != id {
		t.Fatal("id not stable for same topic+time")
	}
}

func TestTranscriptRoundTrip(t *testing.T) {
	st := newTestSession(t)
	if _, err := record(st, "human", "qiangli", "human", "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := record(st, "decision", "qiangli", "", "ship P0"); err != nil {
		t.Fatal(err)
	}
	events, err := readTranscript(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "human" || events[1].Text != "ship P0" {
		t.Fatalf("events = %+v", events)
	}
}

func TestRunRoundAppendsTurns(t *testing.T) {
	st := newTestSession(t)
	got, err := runRound(context.Background(), st, "verb set", fakeRunner{reply: "agree"})
	if err != nil {
		t.Fatalf("runRound: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 turns, got %d", len(got))
	}
	if !strings.Contains(got[0].Text, "[codex]") || !strings.Contains(got[1].Text, "[opencode]") {
		t.Fatalf("turns not attributed: %+v", got)
	}
	events, _ := readTranscript(st.ID)
	if len(events) != 2 {
		t.Fatalf("transcript should hold 2 turns, has %d", len(events))
	}
	if st.Round != 1 {
		t.Fatalf("round = %d, want 1", st.Round)
	}
}

func TestCloseWritesMinutesWithMarkers(t *testing.T) {
	st := newTestSession(t)
	// a git-repo cwd so minutes land under docs/meetings/
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	st.Cwd = repo
	t.Chdir(repo) // close from the repo the room was opened in (fileMinutes contract)
	runRound(context.Background(), st, "verb set", fakeRunner{reply: "agree"})
	_, _ = record(st, "decision", "qiangli", "", "P0 verbs = start, round, close, list")
	_, _ = record(st, "action", "qiangli", "", "claude: strike deferred verbs")

	path, err := closeMeeting(context.Background(), st, closeOptions{Synthesize: true}, fakeRunner{reply: "Discussed and agreed the P0 verb set."})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(repo, "docs", "meetings", "meeting-note-2026-07-08T05-40-finalize-meet-p0.md")
	if path != want {
		t.Fatalf("minutes path = %q, want %q", path, want)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	md := string(b)
	for _, must := range []string{
		"# Meeting — Finalize meet P0",
		"P0 verbs = start, round, close, list", // decision
		"claude: strike deferred verbs",        // action
		"## Summary",                           // secretary prose
		"codex (participant)",
		"claude (secretary)",
	} {
		if !strings.Contains(md, must) {
			t.Fatalf("minutes missing %q\n---\n%s", must, md)
		}
	}
	if st.Status != "closed" {
		t.Fatalf("status = %q, want closed", st.Status)
	}
}

func TestMinutesPathFallsBackOutsideRepo(t *testing.T) {
	st := newTestSession(t) // Cwd is a bare temp dir, no .git
	p := minutesPath(st)
	if !strings.HasSuffix(p, filepath.Join(st.ID, "minutes.md")) {
		t.Fatalf("expected session-store fallback, got %q", p)
	}
}

func TestCommandTreeWiring(t *testing.T) {
	cmd := NewMeetCmd()
	names := map[string]bool{}
	for _, c := range cmd.Commands() {
		names[c.Name()] = true
	}
	for _, v := range []string{
		"open", "consult", "tell", "round", "poll", "ask",
		"converge", "close", "amend", "apply", "show", "contributions", "list", "resume",
	} {
		if !names[v] {
			t.Fatalf("missing subcommand %q", v)
		}
	}
}

// TestMain permits launching agents with their own approval gate disabled.
//
// These tests drive the real launch path (with fake runners) against baseline
// tools whose templates carry a `--dangerously-*` flag, which chat's
// guardUnsafeArgs refuses on an uncontained host. The gate is the point of that
// guard and is tested in pkg/chat; here it is a precondition, not the subject.
func TestMain(m *testing.M) {
	os.Setenv(chat.UnsafeLaunchEnv, "1")
	productionLookup := registeredAgentFn
	registeredAgentFn = func(name string) (fleet.Agent, bool) {
		if agent, ok := productionLookup(name); ok {
			return agent, true
		}
		for _, fixture := range []string{"codex", "claude", "gemini", "opencode", "configured-agent", "release-agent"} {
			if name == fixture {
				return fleet.Agent{Name: name, Tool: name}, true
			}
		}
		return fleet.Agent{}, false
	}
	os.Exit(m.Run())
}
