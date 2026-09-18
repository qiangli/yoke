package weave

import (
	"os/exec"
	"strings"
	"testing"
)

func TestBeatrixSprintCheckpointRegression(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("system git not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_AGENTIC", "")

	repo := t.TempDir()
	gitE2E(t, repo, "init", "-q", "-b", "main")
	gitE2E(t, repo, "config", "user.email", "e2e@test.local")
	gitE2E(t, repo, "config", "user.name", "E2E")
	if err := writeFile(repo+"/README.md", "hello\n"); err != nil {
		t.Fatal(err)
	}
	gitE2E(t, repo, "add", "-A")
	gitE2E(t, repo, "commit", "-q", "-m", "init")
	t.Chdir(repo)

	// Create a sprint
	out, code := runSprint(t, "add", "hello sprint")
	if code != 0 {
		t.Fatalf("sprint add exit=%d out=%s", code, out)
	}

	// 1. Unclaimed lease should fail checkpoint
	t.Setenv("WEAVE_CONDUCTOR", "Charlie")
	out, code = runSprint(t, "checkpoint", "1", "-m", "test")
	if code == 0 || !strings.Contains(out, "unclaimed") {
		t.Fatalf("expected checkpoint on unclaimed to fail with unclaimed, got code=%d out=%s", code, out)
	}

	// 2. Beatrix takes the lease. She must be a LIVE entry in `bashy agent`:
	// taking a sprint seats a coordination address, and one with no process
	// behind it accepts room and inbox traffic nobody will ever read.
	t.Setenv("WEAVE_CONDUCTOR", "Beatrix")
	seedLiveAgent(t, "Beatrix")
	out, code = runSprint(t, "take", "1", "--owner", "Beatrix")
	if code != 0 {
		t.Fatalf("take exit=%d out=%s", code, out)
	}

	// 3. Charlie runs checkpoint; it should preserve Beatrix (fresh appointed holder)
	t.Setenv("WEAVE_CONDUCTOR", "Charlie")
	out, code = runSprint(t, "checkpoint", "1", "-m", "charlie update")
	if code != 0 {
		t.Fatalf("checkpoint exit=%d out=%s", code, out)
	}
	if !strings.Contains(out, "(Beatrix)") {
		t.Errorf("checkpoint should preserve Beatrix as holder, got %s", out)
	}
}
