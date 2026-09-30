package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	todopkg "github.com/qiangli/yoke/pkg/todo"
)

func concurrentCloseFixture(t *testing.T) (string, string, *weaveQueue) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_HOME", "")
	dir := filepath.Join(home, ".bashy", "sprint")
	t.Setenv("BASHY_SPRINT_DIR", dir)
	t.Setenv("BASHY_AGENTIC", "")
	root := dirtyHygieneRepo(t, "umbrella")
	if _, err := gitOutputForTest(root, "checkout", "--", "tracked.txt"); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	now := time.Now().UTC()
	q := &weaveQueue{Stories: []*weaveStory{
		{ID: 1, Title: "closing", Column: "doing", StoryRoots: []string{root}, Boxes: []weaveStoryBox{{StartedAt: now, Planned: time.Hour, Cutoff: now.Add(time.Hour)}}},
		{ID: 2, Title: "active", Column: "doing", StoryRoots: []string{root}, Boxes: []weaveStoryBox{{StartedAt: now, Planned: time.Hour, Cutoff: now.Add(time.Hour)}}},
	}}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}
	return root, dir, q
}

func TestConcurrentSprintEndAfterStop(t *testing.T) {
	_, dir, _ := concurrentCloseFixture(t)
	if out, code := runSprint(t, "stop", "1"); code != 0 {
		t.Fatal(out)
	}
	before, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	stopped := *before.Stories[0].Boxes[0].StoppedAt
	if out, code := runSprint(t, "end", "1", "--gate", "false"); code == 0 || !strings.Contains(out, "gate FAILED") {
		t.Fatalf("end after stop bypassed failing gate: %s", out)
	}
	if out, code := runSprint(t, "end", "1"); code != 0 {
		t.Fatalf("end after stop: %s", out)
	}
	after, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after.Stories[0].Column != "done" || !after.Stories[0].Boxes[0].StoppedAt.Equal(stopped) || after.Stories[1].currentBox() == nil {
		t.Fatalf("close changed timing or concurrent sprint: %+v", after.Stories)
	}
}

func TestConcurrentSprintMoveRetiresDepartedGoal(t *testing.T) {
	root, dir, q := concurrentCloseFixture(t)
	id := sprintTestStory(t, root, 1, "carry forward", "p0", todopkg.StatusTodo)
	q.Stories[0].Goal = []sprintGoalItem{{ID: "carry", Text: "carry outcome", GateRequired: true, Stories: []sprintStoryRef{{Repo: root, ID: id}}}}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}
	st := todopkg.RepoStore(root)
	it, err := todopkg.ResolveRef(st, id)
	if err != nil {
		t.Fatal(err)
	}
	it.Sprint = 2
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutputForTest(root, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutputForTest(root, "commit", "-qm", "move story"); err != nil {
		t.Fatal(err)
	}
	if out, code := runSprint(t, "move", "1", "done"); code != 0 {
		t.Fatalf("departed goal blocked move: %s", out)
	}
	after, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := after.Stories[0]
	if len(s.Goal) != 0 || s.Column != "done" {
		t.Fatalf("goal not retired: %+v", s)
	}
	var thread strings.Builder
	for _, entry := range s.Thread {
		thread.WriteString(entry.Body)
	}
	for _, want := range []string{"carry", "#2", id, "gate-required: yes"} {
		if !strings.Contains(thread.String(), want) {
			t.Errorf("retirement omitted %q: %s", want, thread.String())
		}
	}
}

func TestConcurrentSprintCloseDirtyOwnership(t *testing.T) {
	for _, verb := range []string{"stop", "end"} {
		for _, owner := range []int64{0, 1, 2} {
			t.Run(verb+string(rune('0'+owner)), func(t *testing.T) {
				root, _, _ := concurrentCloseFixture(t)
				id := sprintTestStory(t, root, owner, "dirty story", "p0", todopkg.StatusDone)
				if _, err := gitOutputForTest(root, "add", "."); err != nil {
					t.Fatal(err)
				}
				if _, err := gitOutputForTest(root, "commit", "-qm", "story"); err != nil {
					t.Fatal(err)
				}
				st := todopkg.RepoStore(root)
				it, err := todopkg.ResolveRef(st, id)
				if err != nil {
					t.Fatal(err)
				}
				it.Body = "uncommitted edit"
				path, err := st.Save(it)
				if err != nil {
					t.Fatal(err)
				}
				if owner == 0 {
					if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("unowned\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				out, code := runSprint(t, verb, "1")
				if owner == 1 {
					if code == 0 || !strings.Contains(out, "uncommitted") {
						t.Fatalf("own dirty file accepted: %s", out)
					}
				} else {
					if code != 0 {
						t.Fatalf("other dirt blocked close: %s", out)
					}
					if !strings.Contains(out, "WARNING") || !strings.Contains(out, filepath.Base(path)) {
						t.Fatalf("missing file warning: %s", out)
					}
					if owner == 2 && !strings.Contains(out, "#2") {
						t.Fatalf("missing owner: %s", out)
					}
				}
			})
		}
	}
}

func TestConcurrentSprintEditRetiresDepartedGoal(t *testing.T) {
	root, dir, q := concurrentCloseFixture(t)
	id := sprintTestStory(t, root, 1, "move immediately", "p0", todopkg.StatusTodo)
	q.Stories[0].Goal = []sprintGoalItem{{ID: "departed", Stories: []sprintStoryRef{{Repo: root, ID: id}}}}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}
	cmd := todopkg.NewTodoCmd()
	cmd.SetArgs([]string{"edit", id, "--sprint", "2"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	after, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Stories[0].Goal) != 0 {
		t.Fatal("story moved but old goal remains")
	}
}

func TestConcurrentSprintClosePinOwnership(t *testing.T) {
	for _, verb := range []string{"stop", "end"} {
		for _, name := range []string{"own", "other"} {
			t.Run(verb+"/"+name, func(t *testing.T) {
				root, dir, q := concurrentCloseFixture(t)
				git := func(where string, args ...string) {
					t.Helper()
					if out, err := gitOutputForTest(where, args...); err != nil {
						t.Fatalf("git %v: %v: %s", args, err, out)
					}
				}
				for i, repo := range []string{"own", "other"} {
					nested := filepath.Join(root, repo)
					if err := os.Mkdir(nested, 0755); err != nil {
						t.Fatal(err)
					}
					git(nested, "init", "-q")
					git(nested, "config", "user.name", "test")
					git(nested, "config", "user.email", "test@example.invalid")
					git(nested, "commit", "--allow-empty", "-qm", "base")
					queue, err := weaveQueueDir(nested)
					if err != nil {
						t.Fatal(err)
					}
					if err := saveWeaveQueue(queue, &weaveQueue{Root: nested, Items: []*weaveItem{{ID: 1, Created: q.Stories[i].Boxes[0].StartedAt, State: "done", Disposition: weaveDispositionMerged}}}); err != nil {
						t.Fatal(err)
					}
					q.Stories[i].Runs = []sprintRun{{Repo: repo, Queue: filepath.Base(queue), ID: 1, Born: q.Stories[i].Boxes[0].StartedAt}}
				}
				git(root, "add", ".")
				git(root, "commit", "-qm", "pins")
				if err := saveWeaveQueue(dir, q); err != nil {
					t.Fatal(err)
				}
				git(filepath.Join(root, name), "commit", "--allow-empty", "-qm", "move pin")
				out, code := runSprint(t, verb, "1")
				if name == "own" {
					if code == 0 || !strings.Contains(out, "uncommitted") {
						t.Fatalf("own pin accepted: %s", out)
					}
				} else if code != 0 || !strings.Contains(out, "WARNING") || !strings.Contains(out, "#2") {
					t.Fatalf("other pin did not warn and close: %s", out)
				}
			})
		}
	}
}

func TestConcurrentSprintRetirementKeepsRemainingWork(t *testing.T) {
	root, _, q := concurrentCloseFixture(t)
	own := sprintTestStory(t, root, 1, "still here", "p0", todopkg.StatusTodo)
	moved := sprintTestStory(t, root, 2, "moved", "p0", todopkg.StatusTodo)
	s := q.Stories[0]
	s.Goal = []sprintGoalItem{
		{ID: "mixed", Stories: []sprintStoryRef{{Repo: root, ID: moved}, {Repo: root, ID: own}}},
		{ID: "missing", Stories: []sprintStoryRef{{Repo: root, ID: "missing"}}},
		{ID: "manual"},
	}
	sprintRetireMovedGoals(s)
	if len(s.Goal) != 3 || len(s.Thread) != 0 {
		t.Fatalf("retired remaining or unknown work: %+v", s)
	}
}

func TestConcurrentSprintDeletedStoryStillBlocks(t *testing.T) {
	root, _, q := concurrentCloseFixture(t)
	id := sprintTestStory(t, root, 1, "deleted", "p0", todopkg.StatusDone)
	st := todopkg.RepoStore(root)
	it, err := todopkg.ResolveRef(st, id)
	if err != nil {
		t.Fatal(err)
	}
	path, err := st.Save(it)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutputForTest(root, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutputForTest(root, "commit", "-qm", "story"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	dirty, _, err := sprintDirtyOwnership(q.Stories[0], q.Stories, root, sprintRunRoot)
	if err != nil || dirty != 1 {
		t.Fatalf("deletion lost ownership: dirty=%d err=%v", dirty, err)
	}
}
