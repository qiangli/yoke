package weave

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	todopkg "github.com/qiangli/yoke/pkg/todo"
	"github.com/spf13/cobra"
)

func workerStoryFixture(t *testing.T) (string, string, *weaveItem) {
	t.Helper()
	root := setupIsolationFixture(t)
	t.Setenv("BASHY_HOME", filepath.Join(t.TempDir(), "bashy"))
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	t.Setenv("BASHY_AGENTS_PATH", "")
	t.Setenv("BASHY_MODELS_PATH", "")
	t.Setenv("BASHY_AGENT", "manager")
	t.Setenv("BASHY_AGENT_ID", "manager")
	t.Setenv("BASHY_PRINCIPAL", "")
	seedAgent(t, "worker")
	store := todopkg.RepoStore(root)
	story, err := todopkg.Add(store, "explicit worker story", "", "p2", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	story.Sprint = 1
	if _, err := store.Save(story); err != nil {
		t.Fatal(err)
	}
	dir, _ := sprintStoreDir()
	if err := saveWeaveQueue(dir, &weaveQueue{Stories: []*weaveStory{{ID: 1, Column: "doing", StoryRoots: []string{root}}}}); err != nil {
		t.Fatal(err)
	}
	gitT(t, root, "add", "docs/todo")
	gitT(t, root, "commit", "-qm", "fixture story")
	base := strings.TrimSpace(gitT(t, root, "rev-parse", "HEAD"))
	qdir, _ := weaveQueueDir(root)
	ws := filepath.Join(qdir, "workspaces", "issue-1")
	if err := os.MkdirAll(filepath.Dir(ws), 0755); err != nil {
		t.Fatal(err)
	}
	gitT(t, root, "clone", "--local", root, ws)
	gitT(t, ws, "checkout", "-qb", "agent/weave-issue-1")
	it := &weaveItem{ID: 1, Created: time.Now().UTC(), Owner: "worker-seat", LaunchSpec: &weaveLaunchSpec{Agent: "worker"}, Workspace: ws, Branch: "agent/weave-issue-1", BaseSHA: base, State: "working", Body: fmt.Sprintf("Implement this.\n\nSprint: #1\nStory: #%d\nStory-ID: %s\n\nThen run focused tests.", story.Seq, story.ID)}
	if err := saveWeaveQueue(qdir, &weaveQueue{Root: root, Items: []*weaveItem{it}}); err != nil {
		t.Fatal(err)
	}
	return root, qdir, it
}

func TestWorkerStoryActorAndTerminalEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, message, state string
		want                 bool
	}{
		{"matching", "match", "submitted", true}, {"nonmatching", "other", "submitted", false}, {"no-commits", "", "submitted", false}, {"failed", "match", "failed", false}, {"killed", "match", "killed", false},
		{"wrong-sprint", "match", "submitted", false}, {"dirty", "match", "submitted", false}, {"verify-failed", "match", "submitted", false}, {"preservation", "match", "submitted", false}, {"manual-submit", "match", "submitted", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, qdir, it := workerStoryFixture(t)
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			if err := weaveClaimWorkerStories(cmd, qdir, it); err != nil {
				t.Fatalf("claim: %v %s", err, out.String())
			}
			st := it.WorkerStories[0]
			story, err := todopkg.ResolveRef(todopkg.RepoStore(root), st.ID)
			if err != nil {
				t.Fatal(err)
			}
			if story.Assignee != "worker" || st.Actor != "worker" {
				t.Fatalf("manager/seat impersonation: %+v %+v", story, st)
			}
			if tc.message != "" {
				msg := "delivery\n\n" + strings.Split(strings.Split(it.Body, "\n\n")[1], "\n\n")[0]
				if tc.message == "other" {
					msg = strings.ReplaceAll(msg, st.ID, "abcdef123456")
				}
				if tc.name == "wrong-sprint" {
					msg = strings.ReplaceAll(msg, "Sprint: #1", "Sprint: #2")
				}
				mustWrite(t, filepath.Join(it.Workspace, "done.txt"), "done\n")
				gitT(t, it.Workspace, "add", "done.txt")
				gitT(t, it.Workspace, "commit", "-qm", msg)
			}
			it.State = tc.state
			switch tc.name {
			case "dirty":
				it.Dirty = true
			case "verify-failed":
				exit := 1
				it.VerifyExit = &exit
			case "preservation":
				it.AutoCommitted = true
			case "manual-submit":
				if err := runSprintStorySubmit(cmd, st.Sprint, st.ID, st.Actor, st.Repo, "worker checked tests and committed", &weaveOutputFlags{}); err != nil {
					t.Fatal(err)
				}
			}
			for n := 0; n < 2; n++ {
				if err := weaveSubmitWorkerStories(cmd, it); err != nil {
					t.Fatalf("submit: %v %s", err, out.String())
				}
			}
			dir, _ := sprintStoreDir()
			q, err := loadWeaveQueue(dir)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, c := range q.Stories[0].Thread {
				if strings.Contains(c.Body, " submitted story ") {
					count++
					if c.Author != "worker" || tc.name != "manual-submit" && !strings.Contains(c.Body, st.ID) {
						t.Fatalf("wrong actor/evidence: %+v", c)
					}
				}
			}
			want := 0
			if tc.want {
				want = 1
			}
			if count != want {
				t.Fatalf("submissions=%d want %d: %+v", count, want, q.Stories[0].Thread)
			}
		})
	}
}

func TestWorkerStoryExplicitIdentityAndConflict(t *testing.T) {
	root, qdir, it := workerStoryFixture(t)
	trace, err := weaveBriefTrace(it.Body)
	if err != nil {
		t.Fatal(err)
	}
	full := trace.Stories[0].ID
	// A storyless sprint link must never guess a story from a roster or number.
	it.Body = "Work on the linked sprint"
	stories, err := weaveResolveWorkerStories(qdir, it)
	if err != nil || len(stories) != 0 {
		t.Fatalf("guessed identity: %+v %v", stories, err)
	}
	it.Register = full
	stories, err = weaveResolveWorkerStories(qdir, it)
	if err != nil || len(stories) != 1 || stories[0].ID != full {
		t.Fatalf("register identity: %+v %v", stories, err)
	}
	story, _ := todopkg.ResolveRef(todopkg.RepoStore(root), full)
	story.Assignee = "another-worker"
	todopkg.RepoStore(root).Save(story)
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetErr(&out)
	if err := weaveClaimWorkerStories(cmd, qdir, it); err == nil || !strings.Contains(err.Error(), "held by another-worker") {
		t.Fatalf("claim conflict: %v %s", err, out.String())
	}
	q, _ := loadWeaveQueue(qdir)
	if len(q.Items[0].WorkerStories) != 0 {
		t.Fatal("failed claim recorded as successful")
	}
	it.Register = ""
	it.Body = fmt.Sprintf("Sprint: #1\nStory: #99999\nStory-ID: %s", full)
	if _, err := weaveResolveWorkerStories(qdir, it); err == nil {
		t.Fatal("mismatched story number accepted")
	}
}

func TestWorkerStoryFinalizeIntegration(t *testing.T) {
	root, qdir, it := workerStoryFixture(t)
	t.Chdir(root)
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetErr(&out)
	if err := weaveClaimWorkerStories(cmd, qdir, it); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(it.Workspace, "done.txt"), "done\n")
	gitT(t, it.Workspace, "add", "done.txt")
	gitT(t, it.Workspace, "commit", "-qm", "delivery\n\n"+strings.Split(it.Body, "\n\n")[1])
	if out, code := runWeave(t, "finalize", "1", "--observed-idle", "--json"); code != 0 {
		t.Fatalf("finalize: %d %s", code, out)
	}
	dir, _ := sprintStoreDir()
	q, _ := loadWeaveQueue(dir)
	if !sprintSubmissionEvidence(q.Stories[0], it.WorkerStories[0].ID) {
		t.Fatal("finalize did not submit committed story")
	}
}

func TestWorkerStoryLaunchClaimsBeforeExecution(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint(conflict), func(t *testing.T) {
			root, qdir, it := workerStoryFixture(t)
			t.Chdir(root)
			seedAgent(t, "sh-a")
			trace, _ := weaveBriefTrace(it.Body)
			if conflict {
				story, _ := todopkg.ResolveRef(todopkg.RepoStore(root), trace.Stories[0].ID)
				story.Assignee = "other-worker"
				if _, err := todopkg.RepoStore(root).Save(story); err != nil {
					t.Fatal(err)
				}
			}
			it.State = "allocated"
			it.LaunchSpec = nil
			it.Owner = ""
			if err := saveWeaveQueue(qdir, &weaveQueue{Root: root, Items: []*weaveItem{it}}); err != nil {
				t.Fatal(err)
			}
			storyFiles, err := filepath.Glob(filepath.Join(root, "docs", "todo", trace.Stories[0].ID+"*.md"))
			if err != nil || len(storyFiles) != 1 {
				t.Fatal("missing story fixture")
			}
			script := "grep -q '^assignee: sh-a$' \"$1\" || exit 19; printf 'executed\\n' > launched.txt; git add launched.txt; git commit -qm \"$2\""
			out, code := runWeave(t, "start", "--resume", "--issue", "1", "--pty", "never", "--json", "--", "sh", "-c", script, "worker-fixture", storyFiles[0], "delivery\n\n"+strings.Split(it.Body, "\n\n")[1])
			if conflict {
				if code == 0 || !strings.Contains(out, "held by other-worker") {
					t.Fatalf("conflict: exit %d %s", code, out)
				}
				if _, err := os.Stat(filepath.Join(it.Workspace, "launched.txt")); !os.IsNotExist(err) {
					t.Fatal("implementation executed despite claim conflict")
				}
				if _, err := os.Stat(filepath.Join(it.Workspace, "seed.txt")); err != nil {
					t.Fatal("claim refusal destroyed existing workspace")
				}
			} else {
				story, _ := todopkg.ResolveRef(todopkg.RepoStore(root), trace.Stories[0].ID)
				if story.Assignee != "sh-a" {
					t.Fatalf("actual launched actor not claimed: %q; exit %d %s", story.Assignee, code, out)
				}
				q, _ := loadWeaveQueue(qdir)
				if len(q.Items[0].WorkerStories) != 1 {
					t.Fatalf("launch did not freeze story: exit %d %s", code, out)
				}
				if code != 0 || q.Items[0].State != "submitted" {
					t.Fatalf("worker did not commit after observing its claim: exit %d %s", code, out)
				}
				dir, _ := sprintStoreDir()
				board, _ := loadWeaveQueue(dir)
				if !sprintSubmissionEvidence(board.Stories[0], trace.Stories[0].ID) {
					t.Fatalf("normal wrapper failed to submit story: %s", out)
				}
			}
		})
	}
}

func TestWorkerStoryLinkGenerationAndUUID(t *testing.T) {
	_, qdir, it := workerStoryFixture(t)
	trace, _ := weaveBriefTrace(it.Body)
	it.Register = trace.Stories[0].ID
	it.Body = ""
	dir, _ := sprintStoreDir()
	q, _ := loadWeaveQueue(dir)
	q.Stories = append(q.Stories, &weaveStory{ID: 2, Column: "doing", Runs: []sprintRun{{ID: it.ID, Queue: filepath.Base(qdir), Born: it.Created.Add(-time.Hour)}}})
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}
	if stories, err := weaveResolveWorkerStories(qdir, it); err != nil || len(stories) != 1 || stories[0].Sprint != 1 {
		t.Fatalf("stale generation polluted identity: %+v %v", stories, err)
	}
	q.Stories[1].Runs[0].Born = it.Created
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}
	if _, err := weaveResolveWorkerStories(qdir, it); err == nil {
		t.Fatal("wrong sprint linkage silently ignored")
	}
	q.Stories[1].Runs = nil
	q.Stories[0].UUID = "11111111-1111-1111-1111-111111111111"
	saveWeaveQueue(dir, q)
	it.Body = fmt.Sprintf("Sprint: #1\nSprint-ID: 22222222-2222-2222-2222-222222222222\nStory: #%d\nStory-ID: %s", trace.Stories[0].Number, it.Register)
	if _, err := weaveResolveWorkerStories(qdir, it); err == nil {
		t.Fatal("wrong sprint UUID accepted")
	}
}

func TestWorkerStorySubmissionRefusalIsDurable(t *testing.T) {
	root, qdir, it := workerStoryFixture(t)
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetErr(&out)
	if err := weaveClaimWorkerStories(cmd, qdir, it); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(it.Workspace, "done.txt"), "done\n")
	gitT(t, it.Workspace, "add", "done.txt")
	gitT(t, it.Workspace, "commit", "-qm", "delivery\n\n"+strings.Split(it.Body, "\n\n")[1])
	it.State = "submitted"
	story, _ := todopkg.ResolveRef(todopkg.RepoStore(root), it.WorkerStories[0].ID)
	story.Assignee = "someone-else"
	todopkg.RepoStore(root).Save(story)
	if err := weaveFinishWorkerStories(cmd, qdir, it); err == nil {
		t.Fatal("submit ignored changed holder")
	}
	q, _ := loadWeaveQueue(qdir)
	if q.Items[0].StorySubmissionError == "" {
		t.Fatal("submission failure not retained")
	}
	if _, err := os.Stat(filepath.Join(it.Workspace, "done.txt")); err != nil {
		t.Fatal("submission refusal lost work")
	}
}

func TestWorkerStoryCommitRangeIsNativeAndMeasured(t *testing.T) {
	_, _, it := workerStoryFixture(t)
	mustWrite(t, filepath.Join(it.Workspace, "done.txt"), "done\n")
	gitT(t, it.Workspace, "add", "done.txt")
	gitT(t, it.Workspace, "commit", "-qm", "delivery\n\n"+strings.Split(it.Body, "\n\n")[1])
	head := strings.TrimSpace(gitT(t, it.Workspace, "rev-parse", "HEAD"))
	t.Setenv("PATH", t.TempDir())
	commits, err := weaveWorkerCommits(it)
	if err != nil || len(commits) != 1 || commits[0].Hash.String() != head {
		t.Fatalf("native evidence: %v %v", commits, err)
	}
	it.Head = it.BaseSHA
	if _, err := weaveWorkerCommits(it); err == nil {
		t.Fatal("accepted HEAD changed after terminal measurement")
	}
	it.Head = head
	it.BaseSHA = head
	commits, err = weaveWorkerCommits(it)
	if err != nil || len(commits) != 0 {
		t.Fatalf("old matching commit became new evidence: %v %v", commits, err)
	}
}
