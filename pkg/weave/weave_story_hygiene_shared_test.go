package weave

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dirtyHygieneRepo(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.name", "Sprint Hygiene Test")
	run("config", "user.email", "sprint-hygiene@example.invalid")
	tracked := filepath.Join(root, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "tracked.txt")
	run("commit", "-qm", "base")
	if err := os.WriteFile(tracked, []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSprintHygieneAttributesDirtyPathsInSharedRoots(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	umbrella := dirtyHygieneRepo(t, "umbrella")
	bashy := dirtyHygieneRepo(t, "bashy")

	queueDir, err := weaveQueueDir(bashy)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveWeaveQueue(queueDir, &weaveQueue{Root: bashy, Items: []*weaveItem{
		{ID: 16, Title: "closing sprint work", State: "merged"},
		{ID: 17, Title: "active shared work", State: "todo"},
	}}); err != nil {
		t.Fatal(err)
	}
	queue := filepath.Base(queueDir)
	now := time.Now().UTC()
	closing := &weaveStory{
		ID:         114,
		StoryRoots: []string{umbrella},
		Runs:       []sprintRun{{Repo: "bashy", Queue: queue, ID: 16}},
		Boxes:      []weaveStoryBox{{StartedAt: now, Cutoff: now.Add(time.Hour), Planned: time.Hour}},
	}
	active := &weaveStory{
		ID:         120,
		StoryRoots: []string{umbrella},
		Runs:       []sprintRun{{Repo: "bashy", Queue: queue, ID: 17}},
		Boxes:      []weaveStoryBox{{StartedAt: now, Cutoff: now.Add(time.Hour), Planned: time.Hour}},
	}

	previous := currentBoard
	currentBoard = []*weaveStory{closing, active}
	t.Cleanup(func() { currentBoard = previous })

	hy := sprintCheckHygiene(closing)
	if hy.Clean() || len(hy.Problems) != 1 || !strings.Contains(hy.Problems[0], bashy) {
		t.Fatalf("shared checkout must still block on our linked repo: %+v", hy)
	}
	if !strings.Contains(strings.Join(hy.Shared, "\n"), umbrella) {
		t.Fatalf("unattributed umbrella dirt must stay visible: %+v", hy)
	}

	// Stopping the other sprint does not make unrelated umbrella dirt ours.
	stopped := now
	active.Boxes[0].StoppedAt = &stopped
	hy = sprintCheckHygiene(closing)
	if hy.Clean() || len(hy.Problems) != 1 || !strings.Contains(hy.Problems[0], bashy) {
		t.Fatalf("ownership changed when the other sprint stopped: %+v", hy)
	}
}
