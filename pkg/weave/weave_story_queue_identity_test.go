package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sameNamedSprintRepos(t *testing.T) (oldRoot, currentRoot string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = Weave Test\n\temail = weave-test@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	makeRepo := func(parent string) string {
		root := filepath.Join(base, parent, "same-name")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		gitT(t, root, "init", "-q", "-b", "main")
		if err := os.WriteFile(filepath.Join(root, "seed"), []byte(parent+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitT(t, root, "add", "seed")
		gitT(t, root, "commit", "-qm", "seed")
		resolved, err := weaveRepoRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	return makeRepo("historical"), makeRepo("current")
}

func addPointedRun(t *testing.T, root, title string) {
	t.Helper()
	t.Chdir(root)
	if out, code := runWeave(t, "add", title, "--points", "2"); code != 0 {
		t.Fatalf("add run in %s failed (exit %d): %s", root, code, out)
	}
}

func TestSprintLinkResolvesQueueContainingRequestedRun(t *testing.T) {
	oldRoot, currentRoot := sameNamedSprintRepos(t)
	addPointedRun(t, oldRoot, "historical #1")
	addPointedRun(t, currentRoot, "current #1")
	addPointedRun(t, currentRoot, "current #2")

	t.Chdir(currentRoot)
	if out, code := runSprint(t, "add", "queue identity"); code != 0 {
		t.Fatalf("add sprint failed (exit %d): %s", code, out)
	}
	repo := filepath.Base(currentRoot)
	if out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "2"); code != 0 {
		t.Fatalf("run-aware link failed with historical same-name queue (exit %d): %s", code, out)
	}

	storyDir, _ := sprintStoreDir()
	board, err := loadWeaveQueue(storyDir)
	if err != nil {
		t.Fatal(err)
	}
	currentDir, _ := weaveQueueDir(currentRoot)
	run := findWeaveStory(board, 1).Runs[0]
	if run.Queue != filepath.Base(currentDir) {
		t.Fatalf("link did not persist current queue identity: %+v", run)
	}
}

func TestSprintLinkAmbiguityRequiresQueueAndPreventsCrossSprintDuplicate(t *testing.T) {
	oldRoot, currentRoot := sameNamedSprintRepos(t)
	addPointedRun(t, oldRoot, "historical #1")
	addPointedRun(t, currentRoot, "current #1")
	t.Chdir(currentRoot)
	for _, title := range []string{"owner", "contender"} {
		if out, code := runSprint(t, "add", title); code != 0 {
			t.Fatalf("add sprint failed (exit %d): %s", code, out)
		}
	}
	repo := filepath.Base(currentRoot)
	if out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "1"); code == 0 || !strings.Contains(out, "exists in multiple queues") {
		t.Fatalf("ambiguous link exit=%d output=%q", code, out)
	}
	currentDir, _ := weaveQueueDir(currentRoot)
	tag := filepath.Base(currentDir)
	if out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "1", "--queue", tag); code != 0 {
		t.Fatalf("explicit queue link failed (exit %d): %s", code, out)
	}
	if out, code := runSprint(t, "link", "2", "--repo", repo, "--task", "1", "--queue", tag); code == 0 || !strings.Contains(out, "already linked to sprint #1") {
		t.Fatalf("cross-sprint duplicate exit=%d output=%q", code, out)
	}
}

func TestSprintDrainUsesPersistedQueueIdentity(t *testing.T) {
	oldRoot, currentRoot := sameNamedSprintRepos(t)
	addPointedRun(t, oldRoot, "historical #1")
	addPointedRun(t, currentRoot, "current #1")
	oldDir, _ := weaveQueueDir(oldRoot)
	currentDir, _ := weaveQueueDir(currentRoot)
	for _, dir := range []string{oldDir, currentDir} {
		if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
			findWeaveItem(q, 1).State = "working"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	s := &weaveStory{Runs: []sprintRun{{Repo: filepath.Base(currentRoot), ID: 1, Queue: filepath.Base(currentDir)}}}
	paused, problems := pauseLinkedRepos(s)
	if len(problems) != 0 || len(paused) != 1 {
		t.Fatalf("drain paused=%v problems=%v", paused, problems)
	}
	oldQ, _ := loadWeaveQueue(oldDir)
	currentQ, _ := loadWeaveQueue(currentDir)
	if got := findWeaveItem(oldQ, 1).State; got != "working" {
		t.Fatalf("historical queue was changed: state=%q", got)
	}
	if got := findWeaveItem(currentQ, 1).State; got != "paused" {
		t.Fatalf("linked queue was not parked: state=%q", got)
	}
}
