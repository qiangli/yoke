package weave

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSprintColumnsAreBacklogDoingDone(t *testing.T) {
	if got := strings.Join(weaveStoryColumns, "|"); got != "backlog|doing|done" {
		t.Fatalf("sprint columns = %q, want backlog|doing|done", got)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_AGENTIC", "")
	if _, stderr, code, _ := runSprintStreams(t, "add", "no review column", "--column", "review"); code == 0 || !strings.Contains(stderr, "column must be one of backlog|doing|done") {
		t.Fatalf("review column rejection: exit=%d stderr=%q", code, stderr)
	}
	if out, code := runSprint(t, "add", "transition test"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	if _, stderr, code, _ := runSprintStreams(t, "move", "1", "review"); code == 0 || !strings.Contains(stderr, "column must be one of backlog|doing|done") {
		t.Fatalf("move-to-review rejection: exit=%d stderr=%q", code, stderr)
	}
}

func TestSprintLegacyReviewColumnLoadsAsDoing(t *testing.T) {
	dir := t.TempDir()
	raw := `{"next_id":1,"stories":[{"id":1,"title":"legacy","column":"review"}]}`
	if err := os.WriteFile(filepath.Join(dir, "queue.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := findWeaveStory(q, 1).Column; got != "doing" {
		t.Fatalf("legacy review column loaded as %q, want doing", got)
	}
}

func TestSprintLifecycleSurface(t *testing.T) {
	cmd := NewSprintCmd()
	have := map[string]bool{}
	for _, sub := range cmd.Commands() {
		have[sub.Name()] = true
	}
	for _, name := range []string{"start", "handoff", "take", "end", "goal", "track", "next", "focus"} {
		if !have[name] {
			t.Errorf("missing sprint lifecycle verb %q", name)
		}
	}
	end, _, err := cmd.Find([]string{"end"})
	if err != nil {
		t.Fatal(err)
	}
	if end.Flags().Lookup("force") != nil || end.Flags().Lookup("no-verify") != nil {
		t.Error("sprint end must not expose force or no-verify escape hatches")
	}
	if end.Flags().Lookup("gate-timeout") == nil {
		t.Error("sprint end must bound a stuck gate")
	}
}

func TestRunDrainGateHonorsContextDeadline(t *testing.T) {
	isolateWeaveKBStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	out := runDrainGate(ctx, t.TempDir(), "sleep 5")
	if out.Passed {
		t.Fatal("timed-out gate must not pass")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("gate ignored its context deadline")
	}
	if !strings.Contains(out.Output, "deadline exceeded") {
		t.Fatalf("timeout reason missing from gate output: %q", out.Output)
	}
}

func TestSprintPauseResumeCarriesContinuityWithoutStoppingBox(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_AGENTIC", "")
	t.Setenv("WEAVE_CONDUCTOR", "Ada")
	seedLiveAgent(t, "Ada")

	if out, code := runSprint(t, "add", "lifecycle test"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "Ada", "--for", "1h"); code != 0 {
		t.Fatalf("start exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "pause", "1"); code == 0 {
		t.Fatalf("pause without continuity must fail, exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "pause", "1", "-m", "next: inspect the journal"); code != 0 {
		t.Fatalf("pause exit=%d: %s", code, out)
	}

	q, err := loadWeaveQueue(home + "/.bashy/sprint")
	if err != nil {
		t.Fatal(err)
	}
	s := findWeaveStory(q, 1)
	if s == nil || s.Lease != nil || !s.currentBox().Running() {
		t.Fatalf("pause must release only the lease and leave the box running: %+v", s)
	}

	t.Setenv("WEAVE_CONDUCTOR", "Grace")
	seedLiveAgent(t, "Grace")
	out, code := runSprint(t, "resume", "1", "--owner", "Grace")
	if code != 0 {
		t.Fatalf("resume exit=%d: %s", code, out)
	}
	if !strings.Contains(out, "next: inspect the journal") {
		t.Fatalf("resume did not display continuity: %s", out)
	}
	q, err = loadWeaveQueue(home + "/.bashy/sprint")
	if err != nil {
		t.Fatal(err)
	}
	s = findWeaveStory(q, 1)
	if s.Lease == nil || s.Lease.Holder != "Grace" || !s.currentBox().Running() {
		t.Fatalf("resume must claim the lease without restarting the box: %+v", s)
	}

	// The next CLI process may not inherit --owner. Pause must use the durable
	// lease holder, not silently fall back to the generic conductor identity.
	t.Setenv("WEAVE_CONDUCTOR", "")
	if out, code := runSprint(t, "pause", "1", "-m", "paused again"); code != 0 {
		t.Fatalf("pause after resume --owner must use the lease identity, exit=%d: %s", code, out)
	}
}

func TestSprintEndWithoutGateClosesLifecycleAsUnverified(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_AGENTIC", "")
	t.Setenv("WEAVE_CONDUCTOR", "Ada")
	seedLiveAgent(t, "Ada")

	if out, code := runSprint(t, "add", "end test"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "Ada", "--for", "1h"); code != 0 {
		t.Fatalf("start exit=%d: %s", code, out)
	}
	out, code := runSprint(t, "end", "1")
	if code != 0 || !strings.Contains(out, "NO GATE RAN") {
		t.Fatalf("end without gate must record unverified and succeed, exit=%d: %s", code, out)
	}

	q, err := loadWeaveQueue(home + "/.bashy/sprint")
	if err != nil {
		t.Fatal(err)
	}
	s := findWeaveStory(q, 1)
	if s == nil || s.Column != "done" || s.Lease != nil || s.currentBox() != nil {
		t.Fatalf("end must close the box, release the lease, and move done: %+v", s)
	}
}

func TestSprintStopWithoutGateRecordsUnverified(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_AGENTIC", "")
	t.Setenv("WEAVE_CONDUCTOR", "Ada")
	seedLiveAgent(t, "Ada")

	if out, code := runSprint(t, "add", "stop test"); code != 0 {
		t.Fatal(out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "Ada", "--for", "1h"); code != 0 {
		t.Fatal(out)
	}
	out, code := runSprint(t, "stop", "1")
	if code != 0 || !strings.Contains(out, "NO GATE RAN") {
		t.Fatalf("stop without gate must record unverified and succeed, exit=%d: %s", code, out)
	}
	q, err := loadWeaveQueue(home + "/.bashy/sprint")
	if err != nil {
		t.Fatal(err)
	}
	s := findWeaveStory(q, 1)
	if s == nil || len(s.Boxes) != 1 || s.Boxes[0].StoppedAt == nil || s.Boxes[0].GateRan {
		t.Fatalf("stop must persist an unverified closed box: %+v", s)
	}
}

func TestSprintSuppliedGateStillBlocksStopAndEnd(t *testing.T) {
	for _, verb := range []string{"stop", "end"} {
		t.Run(verb, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("BASHY_AGENTIC", "")
			t.Setenv("WEAVE_CONDUCTOR", "Ada")
			seedLiveAgent(t, "Ada")
			if out, code := runSprint(t, "add", verb+" gate test"); code != 0 {
				t.Fatal(out)
			}
			if out, code := runSprint(t, "start", "1", "--owner", "Ada", "--for", "1h"); code != 0 {
				t.Fatal(out)
			}
			if out, code := runSprint(t, verb, "1", "--gate", "false"); code == 0 || !strings.Contains(out, "gate FAILED") {
				t.Fatalf("supplied failing gate must block %s, exit=%d: %s", verb, code, out)
			}
		})
	}
}

func TestSprintStartRequiresOwnerEvenWithDurableHolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_AGENTIC", "")
	for _, key := range []string{"BASHY_PRINCIPAL", "BASHY_AGENT_ID", "BASHY_AGENT", "WEAVE_CONDUCTOR", "WEAVE_AGENT"} {
		t.Setenv(key, "")
	}

	if out, code := runSprint(t, "add", "durable holder start"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	seedLiveAgent(t, "meridian")
	if out, code := runSprint(t, "take", "1", "--owner", "meridian"); code != 0 {
		t.Fatalf("take exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "start", "1", "--for", "1h"); code == 0 || !strings.Contains(out, "--owner is required") {
		t.Fatalf("start reused durable holder without explicit owner: exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "meridian", "--for", "1h"); code != 0 {
		t.Fatalf("explicit start exit=%d: %s", code, out)
	}

	q, err := loadWeaveQueue(home + "/.bashy/sprint")
	if err != nil {
		t.Fatal(err)
	}
	s := findWeaveStory(q, 1)
	if s == nil || s.Lease == nil || s.Lease.Holder != "meridian" || !s.currentBox().Running() {
		t.Fatalf("start must preserve the holder established by take --owner: %+v", s)
	}
}

func TestSprintStartDoesNotUseBashyPrincipalAsOwner(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_AGENTIC", "")
	t.Setenv("WEAVE_CONDUCTOR", "")
	t.Setenv("WEAVE_AGENT", "")

	if out, code := runSprint(t, "add", "principal holder start"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	seedLiveAgent(t, "meridian")
	if out, code := runSprint(t, "take", "1", "--owner", "meridian"); code != 0 {
		t.Fatalf("take exit=%d: %s", code, out)
	}
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/meridian")
	if out, code := runSprint(t, "start", "1", "--for", "1h"); code == 0 || !strings.Contains(out, "--owner is required") {
		t.Fatalf("start chose BASHY_PRINCIPAL implicitly: exit=%d: %s", code, out)
	}
}

func TestSprintEndNeverBoxedDoesNotInventDuration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_AGENTIC", "")
	t.Setenv("WEAVE_CONDUCTOR", "Ada")
	seedLiveAgent(t, "Ada")

	if out, code := runSprint(t, "add", "completed before boxes shipped"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	out, code := runSprint(t, "end", "1", "--gate", "true")
	if code != 0 {
		t.Fatalf("unboxed end exit=%d: %s", code, out)
	}
	if !strings.Contains(out, "without a recorded time-box") || strings.Contains(out, "stopped after") || strings.Contains(out, "under by") {
		t.Fatalf("end must disclose missing timing evidence without fabricating it: %s", out)
	}

	q, err := loadWeaveQueue(home + "/.bashy/sprint")
	if err != nil {
		t.Fatal(err)
	}
	s := findWeaveStory(q, 1)
	if s == nil || s.Column != "done" || s.Lease != nil || len(s.Boxes) != 0 {
		t.Fatalf("unboxed end must close lifecycle without creating a box: %+v", s)
	}
}
