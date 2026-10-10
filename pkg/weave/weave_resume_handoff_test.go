package weave

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/room"
	todopkg "github.com/qiangli/yoke/pkg/todo"
	"github.com/spf13/cobra"
)

func TestResumeHandoffFlagRequiresResume(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	out, code := runWeave(t, "start", "--run", "1", "--handoff-to", "claude:opus5.5")
	if code == 0 || !strings.Contains(out, "--handoff-to requires --resume") {
		t.Fatalf("handoff validation: exit=%d %s", code, out)
	}
}

func handoffFixture(t *testing.T) (string, string, *weaveItem, *weaveAgentLaunch) {
	t.Helper()
	root, dir, it := workerStoryFixture(t)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv(fleet.InstanceDirEnv, t.TempDir())
	t.Setenv("BASHY_INSTANCE", "")
	t.Setenv("BASHY_INSTANCE_UUID", "")
	cat := pinAgentFleet(t)
	oldCatalog := agentlaunch.NewCatalog
	agentlaunch.NewCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { agentlaunch.NewCatalog = oldCatalog })
	old := &weaveAgentLaunch{Nick: "claude:opus5.5", Tool: "claude", ToolName: "claude", ModelName: "opus5.5"}
	if _, err := weaveOpenRunInstance(it, old, it.Branch); err != nil {
		t.Fatal(err)
	}
	if it.Instance == "" {
		t.Fatal("old identity missing")
	}
	inst, err := fleet.NewInstanceStore("").Get(it.Instance)
	if err != nil {
		t.Fatal(err)
	}
	room.ReleaseSession(inst.ClaimID(), it.SessionClaim)
	it.State, it.WrapperPid = "killed", 0
	it.Body = "preserve the workspace"
	it.WorkerStories = []weaveWorkerStory{{Actor: "old-worker", ID: "prior-story"}}
	mustWrite(t, filepath.Join(it.Workspace, "unique.txt"), "unique\n")
	gitT(t, it.Workspace, "add", "unique.txt")
	gitT(t, it.Workspace, "commit", "-qm", "old unique commit")
	mustWrite(t, filepath.Join(it.Workspace, "seed.txt"), "staged\n")
	gitT(t, it.Workspace, "add", "seed.txt")
	mustWrite(t, filepath.Join(it.Workspace, "seed.txt"), "unstaged\n")
	mustWrite(t, filepath.Join(it.Workspace, "untracked.txt"), "untracked\n")
	mustWrite(t, filepath.Join(it.Workspace, "KB.md"), "operator KB work\n")
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{it}}); err != nil {
		t.Fatal(err)
	}
	next, _, err := weaveExpandAgent([]string{"007"}, it.Body, it.Title)
	if err != nil {
		t.Fatal(err)
	}
	return root, dir, it, next
}

func TestResumeHandoffNativePreservesWorkspaceAndImmutableBinding(t *testing.T) {
	_, _, it, next := handoffFixture(t)
	before := *it
	store := fleet.NewInstanceStore("")
	old, _ := store.Get(it.Instance)
	index, err := os.ReadFile(filepath.Join(it.Workspace, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(gitT(t, it.Workspace, "rev-parse", "HEAD"))
	t.Setenv("PATH", t.TempDir()) // handoff itself must require no host git runner.
	if err := weaveOpenHandoff(it, next, it); err != nil {
		t.Fatal(err)
	}
	if it.Instance == before.Instance || it.InstanceFamily == before.InstanceFamily || it.SessionClaim == before.SessionClaim {
		t.Fatal("reused old immutable identity")
	}
	if it.BaseSHA != before.BaseSHA || it.Workspace != before.Workspace || it.Branch != before.Branch || it.HandoffBaseSHA != head {
		t.Fatal("lost workspace/base/unique commit")
	}
	after, _ := store.Get(before.Instance)
	if !reflect.DeepEqual(old, after) {
		t.Fatalf("old immutable instance mutated: %#v -> %#v", old, after)
	}
	now, _ := os.ReadFile(filepath.Join(it.Workspace, ".git", "index"))
	if !bytes.Equal(index, now) {
		t.Fatal("staging changed")
	}
	for name, want := range map[string]string{"seed.txt": "unstaged\n", "unique.txt": "unique\n", "untracked.txt": "untracked\n", "KB.md": "operator KB work\n"} {
		got, err := os.ReadFile(filepath.Join(it.Workspace, name))
		if err != nil || string(got) != want {
			t.Fatalf("lost %s: %q %v", name, got, err)
		}
	}
	var prior weaveItem
	if len(it.Handoffs) != 1 || json.Unmarshal(it.Handoffs[0].Previous, &prior) != nil || prior.Instance != before.Instance || !reflect.DeepEqual(prior.WorkerStories, before.WorkerStories) {
		t.Fatal("lost old provenance")
	}
	fresh, err := store.Get(it.Instance)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.FamilyID != it.InstanceFamily || len(fresh.Bindings) == 0 {
		t.Fatal("unverified new binding")
	}
	if err := fresh.Rebind(fleet.Family{}); !errors.Is(err, fleet.ErrBindingImmutable) {
		t.Fatalf("new binding mutable: %v", err)
	}
}

func TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched(t *testing.T) {
	for _, tc := range []string{"working", "live-wrapper", "live-child", "live-session", "reservation", "branch", "conflict", "unknown-agent"} {
		t.Run(tc, func(t *testing.T) {
			_, _, it, next := handoffFixture(t)
			switch tc {
			case "working":
				it.State = "working"
			case "live-wrapper":
				it.WrapperPid = os.Getpid()
			case "live-child":
				it.ChildPID = os.Getpid()
			case "live-session":
				inst, _ := fleet.NewInstanceStore("").Get(it.Instance)
				if err := room.ClaimSession(room.Card{ID: inst.ClaimID(), PID: os.Getpid(), SessionClaim: it.SessionClaim}); err != nil {
					t.Fatal(err)
				}
			case "reservation":
				it.ResourceReservationID = "unsettled"
			case "branch":
				it.Branch = "wrong"
			case "conflict":
				repo, err := gogit.PlainOpen(it.Workspace)
				if err != nil {
					t.Fatal(err)
				}
				idx, err := repo.Storer.Index()
				if err != nil {
					t.Fatal(err)
				}
				idx.Entries[0].Stage = 1
				if err := repo.Storer.SetIndex(idx); err != nil {
					t.Fatal(err)
				}
			case "unknown-agent":
				next.Nick = "missing-handoff-agent"
			}
			before, _ := json.Marshal(it)
			index, _ := os.ReadFile(filepath.Join(it.Workspace, ".git", "index"))
			if err := weaveOpenHandoff(it, next, it); err == nil {
				t.Fatal("accepted invalid handoff")
			}
			after, _ := json.Marshal(it)
			now, _ := os.ReadFile(filepath.Join(it.Workspace, ".git", "index"))
			if !bytes.Equal(before, after) || !bytes.Equal(index, now) {
				t.Fatal("refusal changed run/workspace")
			}
		})
	}
}

func TestResumeHandoffIncompatibleFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--tool", "007"}, {"--clone"}, {"--no-spawn"}, {"--arena", "413"}, {"--sealed"}, {"--blind"}, {"--allow-host", "example.invalid"}, {"--", "007"},
	} {
		out, code := runWeave(t, append([]string{"start", "--run", "1", "--resume", "--handoff-to", "007"}, args...)...)
		if code == 0 || !strings.Contains(out, "incompatible") {
			t.Fatalf("%v: %d %s", args, code, out)
		}
	}
}

func TestResumeHandoffExcludesInheritedStoryCommits(t *testing.T) {
	_, _, it := workerStoryFixture(t)
	mustWrite(t, filepath.Join(it.Workspace, "old.txt"), "old\n")
	gitT(t, it.Workspace, "add", "old.txt")
	gitT(t, it.Workspace, "commit", "-qm", "delivery\n\n"+strings.Split(it.Body, "\n\n")[1])
	it.HandoffBaseSHA = strings.TrimSpace(gitT(t, it.Workspace, "rev-parse", "HEAD"))
	commits, err := weaveWorkerCommits(it)
	if err != nil || len(commits) != 0 {
		t.Fatalf("inherited commits counted as replacement delivery: %d %v", len(commits), err)
	}
}

// The existing Go test executable acts as a second tool. All git operations in
// the replacement child use the native repository engine, including staging.
func TestResumeHandoffChild(t *testing.T) {
	if os.Getenv("WEAVE_HANDOFF_TEST_CHILD") != "1" {
		return
	}
	fail := func(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(71) }
	cwd, err := os.Getwd()
	if err != nil {
		fail(err)
	}
	if cwd != os.Getenv("WEAVE_HANDOFF_EXPECT_WORKSPACE") {
		fail(fmt.Errorf("workspace changed: %s", cwd))
	}
	if os.Getenv("BASHY_INSTANCE_UUID") == "" || os.Getenv("BASHY_INSTANCE_UUID") == os.Getenv("WEAVE_HANDOFF_OLD_INSTANCE") {
		fail(fmt.Errorf("missing/new identity not stamped"))
	}
	repo, err := gogit.PlainOpen(cwd)
	if err != nil {
		fail(err)
	}
	head, err := repo.Head()
	if err != nil {
		fail(err)
	}
	if head.Hash().String() != os.Getenv("WEAVE_HANDOFF_EXPECT_HEAD") {
		fail(fmt.Errorf("unique commit lost"))
	}
	idx, err := repo.Storer.Index()
	if err != nil {
		fail(err)
	}
	entry, err := idx.Entry("seed.txt")
	if err != nil {
		fail(err)
	}
	blob, err := repo.BlobObject(entry.Hash)
	if err != nil {
		fail(err)
	}
	reader, err := blob.Reader()
	if err != nil {
		fail(err)
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || string(data) != "staged\n" {
		fail(fmt.Errorf("staged content lost: %q %v", data, err))
	}
	for name, want := range map[string]string{"seed.txt": "unstaged\n", "untracked.txt": "untracked\n", "KB.md": "operator KB work\n"} {
		data, err := os.ReadFile(name)
		if err != nil || string(data) != want {
			fail(fmt.Errorf("workspace changed %s: %q %v", name, data, err))
		}
	}
	wt, err := repo.Worktree()
	if err != nil {
		fail(err)
	}
	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		fail(err)
	}
	msg := "replacement delivery\n\nAgent: replacement"
	if trailers := os.Getenv("WEAVE_HANDOFF_STORY_TRAILERS"); trailers != "" {
		msg = "replacement delivery\n\n" + trailers + "\nAgent: replacement"
	}
	if _, err := wt.Commit(msg, &gogit.CommitOptions{Author: &object.Signature{Name: "replacement", Email: "replacement@test.invalid", When: time.Now()}}); err != nil {
		fail(err)
	}
	os.Exit(0)
}

func TestResumeHandoffLaunchAndRollback(t *testing.T) {
	for _, scenario := range []string{"success", "spawn-failure", "story-success", "story-spawn-failure", "story-conflict", "busy-target"} {
		t.Run(scenario, func(t *testing.T) {
			missing := strings.Contains(scenario, "spawn-failure")
			root, dir, it, _ := handoffFixture(t)
			t.Chdir(root)
			mustWrite(t, filepath.Join(root, "advanced.txt"), "source moved\n")
			gitT(t, root, "add", "advanced.txt")
			gitT(t, root, "commit", "-qm", "advance source after original clone")
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			if missing {
				exe = filepath.Join(t.TempDir(), "missing-worker")
			}
			cat := fleetCatalog()
			t.Setenv("BASHY_FLEET_DIR", cat.Root())
			if err := cat.SaveTool(fleet.Tool{Name: "handoff-fixture", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: exe, Launch: fleet.ToolLaunch{Exec: fmt.Sprintf("%q -test.run=^TestResumeHandoffChild$ -- {model} {prompt}", exe)}}}); err != nil {
				t.Fatal(err)
			}
			if err := cat.SaveAgent(fleet.Agent{Name: "replacement", Tool: "handoff-fixture", Model: "fable"}); err != nil {
				t.Fatal(err)
			}
			var storyID string
			if strings.HasPrefix(scenario, "story-") {
				stories, err := todopkg.List(todopkg.RepoStore(root), "")
				if err != nil || len(stories) != 1 {
					t.Fatalf("stories: %v %v", stories, err)
				}
				st := stories[0]
				storyID = st.ID
				it.Body = fmt.Sprintf("Sprint: #1\nStory: #%d\nStory-ID: %s", st.Seq, st.ID)
				if scenario == "story-conflict" {
					st.Assignee = "old-worker"
					if _, err := todopkg.RepoStore(root).Save(st); err != nil {
						t.Fatal(err)
					}
				}
				if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{it}}); err != nil {
					t.Fatal(err)
				}
				t.Setenv("WEAVE_HANDOFF_STORY_TRAILERS", it.Body)
			}
			if scenario == "busy-target" {
				busy := &weaveItem{ID: 2, State: "working", WrapperPid: os.Getpid(), LaunchSpec: &weaveLaunchSpec{Agent: "replacement"}}
				if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{it, busy}}); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("WEAVE_HANDOFF_TEST_CHILD", "1")
			t.Setenv("WEAVE_HANDOFF_EXPECT_WORKSPACE", it.Workspace)
			t.Setenv("WEAVE_HANDOFF_OLD_INSTANCE", it.Instance)
			t.Setenv("WEAVE_HANDOFF_EXPECT_HEAD", strings.TrimSpace(gitT(t, it.Workspace, "rev-parse", "HEAD")))
			original, _ := json.Marshal(it)
			index, _ := os.ReadFile(filepath.Join(it.Workspace, ".git", "index"))
			out, code := runWeave(t, "start", "--run", "1", "--resume", "--handoff-to", "replacement", "--pty", "never", "--mem-limit", "0", "--json")
			q, err := loadWeaveQueue(dir)
			if err != nil {
				t.Fatal(err)
			}
			got := findWeaveItem(q, 1)
			if missing || scenario == "story-conflict" || scenario == "busy-target" {
				if code == 0 {
					t.Fatalf("missing executable accepted: %s", out)
				}
				restored, _ := json.Marshal(got)
				now, _ := os.ReadFile(filepath.Join(it.Workspace, ".git", "index"))
				if storyID != "" {
					st, err := todopkg.ResolveRef(todopkg.RepoStore(root), storyID)
					if err != nil {
						t.Fatal(err)
					}
					want := ""
					if scenario == "story-conflict" {
						want = "old-worker"
					}
					if st.Assignee != want {
						t.Fatalf("failed handoff left story claimed by %s", st.Assignee)
					}
					boardDir, _ := sprintStoreDir()
					board, err := loadWeaveQueue(boardDir)
					if err != nil {
						t.Fatal(err)
					}
					if sprintSubmissionEvidence(board.Stories[0], storyID) {
						t.Fatal("false story submission")
					}
				}
				if missing {
					records, err := fleet.NewInstanceStore("").List()
					if err != nil {
						t.Fatal(err)
					}
					for _, record := range records {
						if record.UUID != it.Instance && record.Active() {
							t.Fatalf("failed handoff leaked active instance: %+v", record)
						}
					}
				}
				if !bytes.Equal(original, restored) || !bytes.Equal(index, now) {
					t.Fatalf("failed handoff did not roll back: %s\n%s\n%s", original, restored, out)
				}
			} else {
				if code != 0 || got.State != "submitted" || got.AutoCommitted || got.Dirty || got.ExitCode == nil || *got.ExitCode != 0 {
					t.Fatalf("replacement failed: %d %s; %+v", code, out, got)
				}
				if got.Instance == it.Instance || got.BaseSHA != it.BaseSHA || got.Workspace != it.Workspace || got.LaunchSpec.Agent != "replacement" || (storyID == "" && len(got.WorkerStories) != 0) {
					t.Fatalf("handoff identity/provenance: %+v", got)
				}
				if storyID != "" {
					if len(got.WorkerStories) != 1 || got.WorkerStories[0].Actor != "replacement" {
						t.Fatalf("wrong new story actor: %+v", got.WorkerStories)
					}
					boardDir, _ := sprintStoreDir()
					board, err := loadWeaveQueue(boardDir)
					if err != nil {
						t.Fatal(err)
					}
					if !sprintSubmissionEvidence(board.Stories[0], storyID) {
						t.Fatalf("replacement did not submit its committed story: %s", out)
					}
				}
				if len(got.Handoffs) != 1 {
					t.Fatal("missing prior attempt")
				}
				var prior weaveItem
				if err := json.Unmarshal(got.Handoffs[0].Previous, &prior); err != nil || !reflect.DeepEqual(prior.WorkerStories, it.WorkerStories) {
					t.Fatalf("lost old story attribution: %v", err)
				}
			}
		})
	}
}

func TestResumeHandoffStoryRequiresNewAttributedCommit(t *testing.T) {
	for _, actor := range []string{"worker", "wrong-actor", "missing", "inherited"} {
		t.Run(actor, func(t *testing.T) {
			_, dir, it := workerStoryFixture(t)
			cmd := &cobra.Command{}
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			if err := weaveClaimWorkerStories(cmd, dir, it); err != nil {
				t.Fatal(err)
			}
			it.HandoffBaseSHA = strings.TrimSpace(gitT(t, it.Workspace, "rev-parse", "HEAD"))
			message := "delivery\n\n" + strings.Split(it.Body, "\n\n")[1]
			if actor != "missing" {
				who := actor
				if who == "inherited" {
					who = "worker"
				}
				message += "\nAgent: " + who
			}
			mustWrite(t, filepath.Join(it.Workspace, "delivery.txt"), "delivery\n")
			gitT(t, it.Workspace, "add", "delivery.txt")
			gitT(t, it.Workspace, "commit", "-qm", message)
			if actor == "inherited" {
				it.HandoffBaseSHA = strings.TrimSpace(gitT(t, it.Workspace, "rev-parse", "HEAD"))
			}
			it.State = "submitted"
			if err := weaveSubmitWorkerStories(cmd, it); err != nil {
				t.Fatal(err)
			}
			boardDir, _ := sprintStoreDir()
			board, err := loadWeaveQueue(boardDir)
			if err != nil {
				t.Fatal(err)
			}
			submitted := sprintSubmissionEvidence(board.Stories[0], it.WorkerStories[0].ID)
			if submitted != (actor == "worker") {
				t.Fatalf("submission=%v for actor %s", submitted, actor)
			}
		})
	}
}
