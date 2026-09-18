package kb

import (
	"bytes"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

func isolateKB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BASHY_KB_DIR", dir)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_PRINCIPAL", "tester")
	return dir
}

func runKB(t *testing.T, args ...string) error {
	t.Helper()
	cmd := NewKBCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(append([]string{"--user"}, args...))
	return cmd.Execute()
}

func notifications(t *testing.T) []room.Event {
	t.Helper()
	events, err := room.Timeline(0)
	if err != nil {
		t.Fatal(err)
	}
	var out []room.Event
	for _, e := range events {
		if e.Type == room.EventNotify {
			out = append(out, e)
		}
	}
	return out
}

// A SUPERSEDE is the one kb operation an agent cannot afford to discover on its
// next pull: it says something the agent may ALREADY have read and believed is
// wrong. Waiting for it to go looking again is waiting for it to re-derive a
// conclusion it thinks it has settled.
func TestSupersedePublishesToThePageTopic(t *testing.T) {
	isolateKB(t)

	if err := runKB(t, "add", "--title", "Use flag X", "--description", "how to configure X"); err != nil {
		t.Fatal(err)
	}
	if err := runKB(t, "supersede", "use-flag-x",
		"--title", "Flag X was removed", "--description", "X no longer exists"); err != nil {
		t.Fatal(err)
	}

	got := notifications(t)
	if len(got) != 1 {
		t.Fatalf("got %d notifications, want 1: %+v", len(got), got)
	}
	e := got[0]
	if e.Topic != "kb.page.use-flag-x" {
		t.Errorf("topic = %q, want the SUPERSEDED page's topic — that is what a reader of it subscribed to", e.Topic)
	}
	if !strings.Contains(e.Body, "SUPERSEDED") || !strings.Contains(e.Body, "flag-x-was-removed") {
		t.Errorf("body should name the invalidation and its replacement: %q", e.Body)
	}
	if e.Priority == "interrupt" {
		t.Error("a superseded fact must not interrupt every subscriber mid-turn — it is a broadcast, and a broadcast that interrupts is how a bus becomes noise")
	}
	if e.Principal == "" {
		t.Error("a publish with no principal is not attributable")
	}
}

// The SECOND route into the same state. Announcing on only one of the two would
// be the same shape of bug the coach's supervisor alert had: a mode-independent
// condition reported from only one of the paths that produce it.
func TestUpdateToSupersededAlsoPublishes(t *testing.T) {
	isolateKB(t)

	if err := runKB(t, "add", "--title", "Old fact", "--description", "d"); err != nil {
		t.Fatal(err)
	}
	if err := runKB(t, "update", "old-fact", "--status", "superseded"); err != nil {
		t.Fatal(err)
	}

	got := notifications(t)
	if len(got) != 1 {
		t.Fatalf("`update --status superseded` published %d notifications, want 1", len(got))
	}
	if got[0].Topic != "kb.page.old-fact" {
		t.Errorf("topic = %q", got[0].Topic)
	}
	// No replacement exists on this route, so the body must not invent one.
	if strings.Contains(got[0].Body, "by \"\"") {
		t.Errorf("body names an empty replacement: %q", got[0].Body)
	}
}

// Only invalidation pushes. An ADD, an UPDATE that is not an invalidation, and a
// VALIDATE are all fine to discover on the next pull — nobody is acting on a page
// that does not exist yet, and an improved page is still the same page. Announcing
// them would be pure noise.
func TestOrdinaryKBWritesPublishNothing(t *testing.T) {
	isolateKB(t)

	if err := runKB(t, "add", "--title", "A fact", "--description", "d"); err != nil {
		t.Fatal(err)
	}
	if err := runKB(t, "update", "a-fact", "--description", "a better description"); err != nil {
		t.Fatal(err)
	}
	if err := runKB(t, "validate", "a-fact", "--evidence", "used it, worked"); err != nil {
		t.Fatal(err)
	}
	if err := runKB(t, "update", "a-fact", "--status", "stale"); err != nil {
		t.Fatal(err)
	}

	if got := notifications(t); len(got) != 0 {
		t.Errorf("ordinary kb writes published %d notifications, want 0: %+v", len(got), got)
	}
}

// Re-flipping a page that is already superseded must not re-announce.
func TestRepeatedSupersedeStatusDoesNotRepublish(t *testing.T) {
	isolateKB(t)

	if err := runKB(t, "add", "--title", "Old fact", "--description", "d"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := runKB(t, "update", "old-fact", "--status", "superseded"); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(notifications(t)); n != 1 {
		t.Errorf("published %d times, want 1 — the notification marks the TRANSITION", n)
	}
}
