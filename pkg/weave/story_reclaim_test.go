package weave

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/principal"
	"github.com/qiangli/yoke/pkg/prune"
	"github.com/qiangli/yoke/pkg/room"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

func setupReclaimEnv(t *testing.T) (home, root, queueDir string) {
	t.Helper()
	unsetEnvForTest(t, weaveManagedGOCacheEnv)
	root = setupIsolationFixture(t)
	home = os.Getenv("HOME")
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	t.Setenv("BASHY_COORD_DIR", filepath.Join(home, ".bashy", "coord"))
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(home, ".bashy", "room"))
	t.Setenv("BASHY_TODO_DIR", filepath.Join(home, ".bashy", "todo"))
	t.Setenv("WEAVE_CONDUCTOR", "boss")
	seedLiveAgent(t, "boss")
	seedLiveAgent(t, "worker")
	t.Chdir(root)
	queueDir, _ = weaveQueueDir(root)
	return home, root, queueDir
}

func runClaimCLI(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()
	cmd := coord.NewClaimCmd(func() []string { return []string{root} })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func runTodoCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := todopkg.NewTodoCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func seedSettledRunForStory(t *testing.T, root, queueDir, storyID string, runID int64, retain bool, merged bool) string {
	t.Helper()
	ws := filepath.Join(queueDir, "workspaces", "issue-"+strconv.FormatInt(runID, 10))
	if err := os.MkdirAll(filepath.Dir(ws), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, root, "clone", "-q", root, ws)
	branch := "agent/weave-issue-" + strconv.FormatInt(runID, 10)
	gitT(t, ws, "checkout", "-qb", branch)
	baseSHA := strings.TrimSpace(gitT(t, root, "rev-parse", "HEAD"))
	fileName := "story-" + storyID[:8] + "-" + strconv.FormatInt(runID, 10) + ".txt"
	writeAndCommit(t, ws, fileName, "delivered\n", "deliver "+storyID[:8])
	headSHA := strings.TrimSpace(gitT(t, ws, "rev-parse", "HEAD"))
	state := "submitted"
	if merged {
		gitT(t, root, "fetch", "-q", ws, branch+":"+branch)
		gitT(t, root, "merge", "-q", "--no-ff", "-m", "merge "+branch, branch)
		state = "done"
	}

	q, err := loadWeaveQueue(queueDir)
	if err != nil {
		q = &weaveQueue{Root: root, NextID: 1}
	}
	if q.Root == "" {
		q.Root = root
	}
	item := &weaveItem{
		ID:           runID,
		Title:        "deliver " + storyID[:8],
		State:        state,
		Register:     storyID,
		Owner:        "worker",
		Workspace:    ws,
		Branch:       branch,
		BaseSHA:      baseSHA,
		Head:         headSHA,
		CommitsAhead: 1,
		Created:      time.Now().UTC(),
		FinishedAt:   time.Now().UTC(),
		Points:       2,
		LaunchSpec:   &weaveLaunchSpec{Tool: "true", MaxRuntime: 5 * time.Minute},
	}
	if retain {
		item.Retain = true
		item.RetainOwner = "boss"
		item.RetainReason = "inspect before accept"
	}
	if merged {
		item.Disposition = weaveDispositionMerged
	}
	q.Items = append(q.Items, item)
	if runID >= q.NextID {
		q.NextID = runID + 1
	}
	if err := saveWeaveQueue(queueDir, q); err != nil {
		t.Fatal(err)
	}
	return ws
}

func seedRetainedGradeCheckout(t *testing.T, sprintID int64, runLabel string) string {
	t.Helper()
	runDir, err := sprintGradeRunDir(sprintID, runLabel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	attemptDir, err := os.MkdirTemp(runDir, "attempt-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attemptDir, "graded.txt"), []byte("graded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return attemptDir
}

// Acceptance 1: accepting a story with a live story claim, a lease claim naming
// it, a settled (--retain) run workspace, and a retained grade checkout leaves
// none of them and records each release/deletion on the room timeline.
func TestSprintAcceptReclaimsStoryClaimLeaseWorkspaceAndGradeCheckout(t *testing.T) {
	_, root, queueDir := setupReclaimEnv(t)
	repo := filepath.Base(root)

	if out, code := runSprint(t, "add", "close reclaims everything"); code != 0 {
		t.Fatalf("sprint add: %s", out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "boss", "--for", "1h"); code != 0 {
		t.Fatalf("sprint start: %s", out)
	}
	store := todopkg.RepoStore(root)
	story, err := todopkg.Add(store, "reclaim on accept", "spec", "p0", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	story.Sprint = 1
	if _, err := store.Save(story); err != nil {
		t.Fatal(err)
	}
	if out, code := runSprint(t, "track", "1"); code != 0 {
		t.Fatalf("sprint track: %s", out)
	}
	if out, code := runSprint(t, "claim", "1", story.ID, "--owner", "worker"); code != 0 {
		t.Fatalf("sprint claim: %s", out)
	}
	if out, code := runSprint(t, "submit", "1", story.ID, "--as", "worker", "-m", "delivered"); code != 0 {
		t.Fatalf("sprint submit: %s", out)
	}
	// Simulate a lingering story announce claim from another session/episode
	// plus a detached resource lease naming this story.
	if _, err := coord.AcquireRef(context.Background(), coord.Request{
		Ref:    storyClaimRef(1, story.ID),
		Holder: principal.Ref{Name: "worker", Episode: "ep-stale-session", Host: "box-a"},
		Mode:   coord.ModeAnnounce,
		Force:  true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.AcquireRef(context.Background(), coord.Request{
		Ref:    coord.Ref{Kind: coord.KindName, Name: "lane-alpha"},
		Holder: principal.Ref{Name: "worker", Episode: "ep-lane", Host: "box-a"},
		Story:  "1/" + story.ID[:8],
		Mode:   coord.ModeLease,
	}); err != nil {
		t.Fatal(err)
	}

	ws := seedSettledRunForStory(t, root, queueDir, story.ID, 1, true, true)
	if out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "1", "--story", story.ID); code != 0 {
		t.Fatalf("sprint link: %s", out)
	}
	gradeCheckout := seedRetainedGradeCheckout(t, 1, repo+"#1")

	// While the story is still open, `sprint prune --apply` must respect --retain.
	if out, code := runSprint(t, "prune", "1", "--apply"); code != 0 {
		t.Fatalf("sprint prune --apply: %s", out)
	}
	if _, err := os.Stat(ws); err != nil {
		t.Fatalf("retained workspace removed before story close: %v", err)
	}

	// Commit repo todo changes so repo tree is clean if needed; accept closes and reclaims!
	if out, code := runSprint(t, "accept", "1", story.ID, "--no-rating", "-m", "verified"); code != 0 {
		t.Fatalf("sprint accept: %s", out)
	}

	claims, err := coord.List(coord.DefaultDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("claims remaining after sprint accept = %+v, want none", claims)
	}
	if _, err := os.Lstat(ws); !os.IsNotExist(err) {
		t.Fatalf("run workspace survived sprint accept: %v", err)
	}
	if _, err := os.Lstat(gradeCheckout); !os.IsNotExist(err) {
		t.Fatalf("grade checkout survived sprint accept: %v", err)
	}

	events, err := room.Timeline(0)
	if err != nil {
		t.Fatal(err)
	}
	var reclaimBodies []string
	for _, ev := range events {
		if ev.Topic == "reclaim" {
			reclaimBodies = append(reclaimBodies, ev.Body)
		}
	}
	joined := strings.Join(reclaimBodies, "\n")
	for _, want := range []string{
		"sprint accept: released claim story:1/" + story.ID,
		"sprint accept: released claim name:lane-alpha",
		"sprint accept: deleted workspace",
		"sprint accept: deleted grade checkout",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("timeline reclaim events missing %q in:\n%s", want, joined)
		}
	}
}

// Story close via `todo done` (standalone), `todo status ... wontfix`, and
// `todo edit --sprint 0` (move off sprint) all share the same reclaim path.
func TestTodoDoneWontfixAndMoveOffSprintReclaim(t *testing.T) {
	_, root, queueDir := setupReclaimEnv(t)
	repo := filepath.Base(root)

	if out, code := runSprint(t, "add", "todo close paths"); code != 0 {
		t.Fatalf("sprint add: %s", out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "boss", "--for", "1h"); code != 0 {
		t.Fatalf("sprint start: %s", out)
	}
	if out, code := runSprint(t, "track", "1"); code != 0 {
		t.Fatalf("sprint track: %s", out)
	}
	store := todopkg.RepoStore(root)

	// 1. Standalone `todo done`.
	standalone, err := todopkg.Add(store, "standalone item", "", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coord.AcquireRef(context.Background(), coord.Request{
		Ref:    coord.Ref{Kind: coord.KindName, Name: "lane-standalone"},
		Holder: principal.Ref{Name: "worker", Episode: "ep-1", Host: "box-a"},
		Story:  standalone.ID,
		Mode:   coord.ModeLease,
	}); err != nil {
		t.Fatal(err)
	}
	wsStandalone := seedSettledRunForStory(t, root, queueDir, standalone.ID, 1, false, true)
	if out, err := runTodoCLI(t, "--repo", "done", standalone.ID); err != nil {
		t.Fatalf("todo done: %v\n%s", err, out)
	}
	if _, err := os.Lstat(wsStandalone); !os.IsNotExist(err) {
		t.Fatalf("standalone workspace survived todo done: %v", err)
	}

	// 2. Sprint story `todo status <id> wontfix`.
	wf, err := todopkg.Add(store, "wontfix item", "", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	wf.Sprint = 1
	if _, err := store.Save(wf); err != nil {
		t.Fatal(err)
	}
	if out, code := runSprint(t, "claim", "1", wf.ID, "--owner", "worker"); code != 0 {
		t.Fatalf("sprint claim wf: %s", out)
	}
	wsWF := seedSettledRunForStory(t, root, queueDir, wf.ID, 2, true, true)
	if out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "2", "--story", wf.ID); code != 0 {
		t.Fatalf("sprint link wf: %s", out)
	}
	if out, err := runTodoCLI(t, "--repo", "status", wf.ID, "wontfix"); err != nil {
		t.Fatalf("todo status wontfix: %v\n%s", err, out)
	}
	if _, err := os.Lstat(wsWF); !os.IsNotExist(err) {
		t.Fatalf("wontfix workspace survived: %v", err)
	}

	// 3. Moving a story off sprint #1 (`todo edit <id> --sprint 0`).
	mv, err := todopkg.Add(store, "moving item", "", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	mv.Sprint = 1
	if _, err := store.Save(mv); err != nil {
		t.Fatal(err)
	}
	if out, code := runSprint(t, "claim", "1", mv.ID, "--owner", "worker"); code != 0 {
		t.Fatalf("sprint claim mv: %s", out)
	}
	if _, err := coord.AcquireRef(context.Background(), coord.Request{
		Ref:    coord.Ref{Kind: coord.KindName, Name: "lane-sprint-scoped"},
		Holder: principal.Ref{Name: "worker", Episode: "ep-mv", Host: "box-a"},
		Story:  "1/" + mv.ID,
		Mode:   coord.ModeLease,
	}); err != nil {
		t.Fatal(err)
	}
	wsMV := seedSettledRunForStory(t, root, queueDir, mv.ID, 3, false, true)
	if out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "3", "--story", mv.ID); code != 0 {
		t.Fatalf("sprint link mv: %s", out)
	}
	if out, err := runTodoCLI(t, "--repo", "edit", mv.ID, "--sprint", "0"); err != nil {
		t.Fatalf("todo edit --sprint 0: %v\n%s", err, out)
	}
	if _, err := os.Lstat(wsMV); !os.IsNotExist(err) {
		t.Fatalf("moved-off-sprint workspace survived: %v", err)
	}
	claims, err := coord.List(coord.DefaultDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("claims left after tododone/wontfix/move-sprint = %+v, want 0", claims)
	}
}

// Acceptance 2: `sprint end` leaves no claim, workspace, or grade copy for its stories.
func TestSprintEndReclaimsRemainingClaimsRetainedWorkspacesAndGradeDirs(t *testing.T) {
	_, root, queueDir := setupReclaimEnv(t)
	repo := filepath.Base(root)

	if out, code := runSprint(t, "add", "end reclaims all"); code != 0 {
		t.Fatalf("sprint add: %s", out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "boss", "--for", "1h"); code != 0 {
		t.Fatalf("sprint start: %s", out)
	}
	store := todopkg.RepoStore(root)
	st, err := todopkg.Add(store, "already closed before fix", "", "p0", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	st.Sprint = 1
	if _, err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	if out, code := runSprint(t, "track", "1"); code != 0 {
		t.Fatalf("sprint track: %s", out)
	}

	ws := seedSettledRunForStory(t, root, queueDir, st.ID, 1, true, true)
	if out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "1", "--story", st.ID); code != 0 {
		t.Fatalf("sprint link: %s", out)
	}
	// Simulate a story closed before this fix (status=done on disk while its
	// claim, lease, retained workspace, and grade checkout still linger).
	now := time.Now().UTC()
	st.Status = todopkg.StatusDone
	st.Closed = &now
	st.ClosedBy = "boss"
	if _, err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	gitT(t, root, "add", ".")
	gitT(t, root, "commit", "-q", "-m", "record closed story")
	gradeCheckout := seedRetainedGradeCheckout(t, 1, repo+"#1")

	if _, err := coord.AcquireRef(context.Background(), coord.Request{
		Ref:    storyClaimRef(1, st.ID),
		Holder: principal.Ref{Name: "worker", Episode: "ep-old", Host: "box-a"},
		Mode:   coord.ModeAnnounce,
		Force:  true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.AcquireRef(context.Background(), coord.Request{
		Ref:    coord.Ref{Kind: coord.KindName, Name: "lane-beta"},
		Holder: principal.Ref{Name: "worker", Episode: "ep-old", Host: "box-a"},
		Story:  st.ID,
		Mode:   coord.ModeLease,
	}); err != nil {
		t.Fatal(err)
	}

	out, code := runSprint(t, "end", "1")
	if code != 0 {
		t.Fatalf("sprint end exit=%d:\n%s", code, out)
	}
	if !strings.Contains(out, "residual: 0") {
		t.Fatalf("sprint end missing residual: 0:\n%s", out)
	}
	if claims, _ := coord.List(coord.DefaultDir()); len(claims) != 0 {
		t.Fatalf("claims survived sprint end: %+v", claims)
	}
	if _, err := os.Lstat(ws); !os.IsNotExist(err) {
		t.Fatalf("workspace survived sprint end: %v", err)
	}
	if _, err := os.Lstat(gradeCheckout); !os.IsNotExist(err) {
		t.Fatalf("grade checkout survived sprint end: %v", err)
	}
	if sprintGradeDir, err := sprintGradeSprintDir(1); err == nil {
		if _, statErr := os.Lstat(sprintGradeDir); !os.IsNotExist(statErr) {
			t.Fatalf("sprint grade dir survived sprint end: %s", sprintGradeDir)
		}
	}
}

// Delete-by-default for `sprint grade` (todo:bf2f149daa87), plus --retain opt-in.
func TestSprintGradeDeletesCheckoutByDefaultAndHonorsRetain(t *testing.T) {
	dir, base, _ := sprintGradeFixture(t)
	storyRoot := t.TempDir()
	if _, err := gogit.PlainInit(storyRoot, false); err != nil {
		t.Fatal(err)
	}
	story, err := todopkg.Add(todopkg.RepoStore(storyRoot), "graded story", "body", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	story.ID, story.Seq, story.Sprint = "4474b08d8299", 1217, 331
	if _, err := todopkg.RepoStore(storyRoot).Save(story); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_PRINCIPAL", "agent-manager")
	t.Setenv("BASHY_SPRINT_ENFORCE", "must")
	t.Setenv(sprintLeaseTokenEnv, "secret")
	fork := filepath.Join(t.TempDir(), "fork.git")
	if _, err := gogit.PlainClone(fork, true, &gogit.CloneOptions{URL: dir}); err != nil {
		t.Fatal(err)
	}
	queue, err := weaveQueueDir(storyRoot)
	if err != nil {
		t.Fatal(err)
	}
	item := &weaveItem{ID: 1, Owner: "agent-a", Register: story.ID, Workspace: dir, Branch: "master", BoothForkURL: fork, BaseSHA: base, Created: time.Now().UTC()}
	if err := sprintGradePushAttempt(context.Background(), item, ""); err != nil {
		t.Fatal(err)
	}
	if err := saveWeaveQueue(queue, &weaveQueue{Root: storyRoot, NextID: 2, Items: []*weaveItem{item}}); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Base(storyRoot)
	run := repo + "#1"
	s := &weaveStory{
		ID:         331,
		Column:     "doing",
		StoryRoots: []string{storyRoot},
		Lease:      &weaveStoryLease{Holder: "agent-manager", TokenHash: sprintLeaseTokenHash("secret"), At: time.Now()},
		Runs:       []sprintRun{{Repo: repo, Queue: filepath.Base(queue), ID: 1, Born: item.Created, Story: story.ID}},
		Arena:      &sprintArena{Repos: []arenaRepo{{Repo: repo, Base: base}}},
	}
	board, _ := sprintStoreDir()
	if err := withWeaveQueueLock(board, func(q *weaveQueue) error { q.Stories = append(q.Stories, s); return nil }); err != nil {
		t.Fatal(err)
	}

	// 1. Default `sprint grade` deletes temporary checkout on both pass and fail.
	out, code := runSprint(t, "grade", "331", "--run", run, "--gate", "exit 0", "--json")
	if code != 0 {
		t.Fatalf("grade pass: %d %s", code, out)
	}
	var passEv sprintGradeEvent
	if err := json.Unmarshal([]byte(out), &passEv); err != nil {
		t.Fatal(err)
	}
	if passEv.Checkout != "" || passEv.Retained {
		t.Fatalf("default grade kept Checkout in event: %+v", passEv)
	}
	runDir, _ := sprintGradeRunDir(331, run)
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("default grade leaked run dir %s: %v", runDir, err)
	}

	// 2. `--retain` without `--retain-reason` is refused.
	if out, code := runSprint(t, "grade", "331", "--run", run, "--gate", "exit 0", "--retain"); code == 0 || !strings.Contains(out, "--retain-reason") {
		t.Fatalf("--retain without reason should fail: %d %s", code, out)
	}

	// 3. `--retain --retain-reason` keeps checkout on disk until story close.
	out, code = runSprint(t, "grade", "331", "--run", run, "--gate", "exit 0", "--retain", "--retain-reason", "forensic check", "--json")
	if code != 0 {
		t.Fatalf("grade retain: %d %s", code, out)
	}
	var retEv sprintGradeEvent
	if err := json.Unmarshal([]byte(out), &retEv); err != nil {
		t.Fatal(err)
	}
	if !retEv.Retained || retEv.RetainReason != "forensic check" || retEv.Checkout == "" {
		t.Fatalf("retained grade event = %+v", retEv)
	}
	if _, err := os.Stat(retEv.Checkout); err != nil {
		t.Fatalf("retained grade checkout missing before close: %v", err)
	}

	// Settle the booth run record, then close the story (`wontfix`) and verify
	// the retained grade checkout is reclaimed.
	if err := withWeaveQueueLock(queue, func(q *weaveQueue) error {
		cur := findWeaveItem(q, 1)
		cur.State = "abandoned"
		cur.Disposition = weaveDispositionRejected
		cur.Workspace = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := todopkg.SetStatus(todopkg.RepoStore(storyRoot), story.ID, todopkg.StatusWontfix); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(retEv.Checkout); !os.IsNotExist(err) {
		t.Fatalf("retained grade checkout survived story close: %v", err)
	}
}

// Acceptance 4: a run with unpushed/unmerged unique commits is refused, NEVER deleted.
func TestUnpushedUniqueCommitsRefusedNeverDeleted(t *testing.T) {
	_, root, queueDir := setupReclaimEnv(t)
	repo := filepath.Base(root)

	if out, code := runSprint(t, "add", "protect unique work"); code != 0 {
		t.Fatalf("sprint add: %s", out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "boss", "--for", "1h"); code != 0 {
		t.Fatalf("sprint start: %s", out)
	}
	store := todopkg.RepoStore(root)
	st, err := todopkg.Add(store, "has unique commit", "spec", "p0", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	st.Sprint = 1
	if _, err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	if out, code := runSprint(t, "track", "1"); code != 0 {
		t.Fatalf("sprint track: %s", out)
	}
	if out, code := runSprint(t, "claim", "1", st.ID, "--owner", "worker"); code != 0 {
		t.Fatalf("sprint claim: %s", out)
	}
	if out, code := runSprint(t, "submit", "1", st.ID, "--as", "worker", "-m", "unmerged branch"); code != 0 {
		t.Fatalf("sprint submit: %s", out)
	}

	// Seed a run with a unique commit NOT merged into main and NOT in refs/salvage/*.
	ws := seedSettledRunForStory(t, root, queueDir, st.ID, 1, false, false)
	if out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "1", "--story", st.ID); code != 0 {
		t.Fatalf("sprint link: %s", out)
	}

	// 1. `sprint accept` refuses closure.
	if out, code := runSprint(t, "accept", "1", st.ID, "--no-rating", "-m", "try accept"); code == 0 || !strings.Contains(out, "unsettled work") {
		t.Fatalf("sprint accept must refuse unmerged unique commit: exit=%d\n%s", code, out)
	}
	// 2. `todo status ... wontfix` refuses closure.
	if out, err := runTodoCLI(t, "--repo", "status", st.ID, "wontfix"); err == nil || !strings.Contains(out+err.Error(), "unsettled work") {
		t.Fatalf("todo wontfix must refuse unmerged unique commit: err=%v out=%s", err, out)
	}
	// 3. `todo edit ... --sprint 0` refuses unlinking.
	if out, err := runTodoCLI(t, "--repo", "edit", st.ID, "--sprint", "0"); err == nil || !strings.Contains(out+err.Error(), "unsettled work") {
		t.Fatalf("todo move-sprint must refuse unmerged unique commit: err=%v out=%s", err, out)
	}
	if _, err := os.Stat(ws); err != nil {
		t.Fatalf("unique workspace was deleted on refusal: %v", err)
	}

	// 4. Even if the story is already closed on disk (legacy state), `--stories-closed --apply`
	// must NEVER delete a workspace holding unique unmerged commits.
	now := time.Now().UTC()
	st.Status = todopkg.StatusDone
	st.Closed = &now
	if _, err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	out, err := runClaimCLI(t, root, "prune", "--stories-closed", "--apply")
	if err != nil {
		t.Fatalf("claim prune --stories-closed --apply: %v\n%s", err, out)
	}
	if _, err := os.Stat(ws); err != nil {
		t.Fatalf("backfill deleted workspace with unique unmerged commit: %v", err)
	}
}

// Acceptance 3: backfill (`bashy claim prune --stories-closed [--apply]`)
// lists the exact stale set (closed story claims/leases/copies + deleted story
// in readable repo + ended sprint copies) while keeping live open stories,
// unrelated claims, and open sprints whose story root is unavailable/unreadable.
func TestBackfillStoriesClosedExactSetAndUnreadableGuard(t *testing.T) {
	_, root, queueDir := setupReclaimEnv(t)
	repo := filepath.Base(root)

	if out, code := runSprint(t, "add", "active sprint"); code != 0 {
		t.Fatalf("sprint add 1: %s", out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "boss", "--for", "1h"); code != 0 {
		t.Fatalf("sprint start 1: %s", out)
	}
	if out, code := runSprint(t, "track", "1"); code != 0 {
		t.Fatalf("sprint track 1: %s", out)
	}

	// Sprint #2 has a missing/unmounted StoryRoot: missing stories on #2 are
	// UNVERIFIABLE (not provably deleted) and must NOT be pruned!
	if out, code := runSprint(t, "add", "unmounted repo sprint"); code != 0 {
		t.Fatalf("sprint add 2: %s", out)
	}
	boardDir, _ := sprintStoreDir()
	unmountedRoot := filepath.Join(t.TempDir(), "not-checked-out")
	if err := withWeaveQueueLock(boardDir, func(q *weaveQueue) error {
		s2 := findWeaveStory(q, 2)
		s2.Column = "doing"
		s2.StoryRoots = []string{unmountedRoot}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	store := todopkg.RepoStore(root)
	doneStory, err := todopkg.Add(store, "already done", "", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	doneStory.Sprint = 1
	if _, err := store.Save(doneStory); err != nil {
		t.Fatal(err)
	}

	openStory, err := todopkg.Add(store, "still working", "", "p0", nil, "", "worker")
	if err != nil {
		t.Fatal(err)
	}
	openStory.Sprint = 1
	if _, err := store.Save(openStory); err != nil {
		t.Fatal(err)
	}

	wsDone := seedSettledRunForStory(t, root, queueDir, doneStory.ID, 1, true, true)
	if out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "1", "--story", doneStory.ID); code != 0 {
		t.Fatalf("sprint link done: %s", out)
	}
	// Simulate pre-existing closed story on disk with leftover claim/run/grade copies.
	now := time.Now().UTC()
	doneStory.Status = todopkg.StatusDone
	doneStory.Closed = &now
	if _, err := store.Save(doneStory); err != nil {
		t.Fatal(err)
	}
	gradeDone := seedRetainedGradeCheckout(t, 1, repo+"#1")

	deletedStoryID := "deadbeef1234"
	unverifiableStoryID := "cafebabe5678"

	// Stale claims that MUST be pruned:
	for _, req := range []coord.Request{
		{Ref: storyClaimRef(1, doneStory.ID), Holder: principal.Ref{Name: "worker", Episode: "ep-1", Host: "h"}, Mode: coord.ModeAnnounce, Force: true},
		{Ref: coord.Ref{Kind: coord.KindName, Name: "lane-done"}, Holder: principal.Ref{Name: "worker", Episode: "ep-1", Host: "h"}, Story: "1/" + doneStory.ID, Mode: coord.ModeLease},
		{Ref: storyClaimRef(1, deletedStoryID), Holder: principal.Ref{Name: "ghost", Episode: "ep-2", Host: "h"}, Mode: coord.ModeAnnounce, Force: true},
	} {
		if _, err := coord.AcquireRef(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}

	// Claims that MUST survive:
	for _, req := range []coord.Request{
		{Ref: storyClaimRef(1, openStory.ID), Holder: principal.Ref{Name: "worker", Episode: "ep-3", Host: "h"}, Mode: coord.ModeAnnounce, Force: true},
		{Ref: coord.Ref{Kind: coord.KindName, Name: "shared-gpu"}, Holder: principal.Ref{Name: "other", Episode: "ep-4", Host: "h"}, Mode: coord.ModeLease},
		{Ref: storyClaimRef(2, unverifiableStoryID), Holder: principal.Ref{Name: "remote-worker", Episode: "ep-5", Host: "h"}, Mode: coord.ModeAnnounce, Force: true},
	} {
		if _, err := coord.AcquireRef(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}

	// Dry-run JSON check: exact stale set, zero mutation.
	dryJSON, err := runClaimCLI(t, root, "prune", "--stories-closed", "--json")
	if err != nil {
		t.Fatalf("dry-run --stories-closed: %v\n%s", err, dryJSON)
	}
	var dryRes prune.Result
	if err := json.Unmarshal([]byte(dryJSON), &dryRes); err != nil {
		t.Fatal(err)
	}
	if dryRes.Applied {
		t.Fatal("dry-run reported Applied=true")
	}
	gotIDs := map[string]bool{}
	for _, item := range dryRes.Items {
		gotIDs[item.Noun+"|"+item.ID] = true
	}
	for _, want := range []string{
		"claim|story:1/" + doneStory.ID,
		"claim|name:lane-done",
		"claim|story:1/" + deletedStoryID,
		"grade|" + gradeDone,
	} {
		if !gotIDs[want] {
			t.Fatalf("dry-run missing %q; got %+v", want, dryRes.Items)
		}
	}
	for _, forbidden := range []string{
		"claim|story:1/" + openStory.ID,
		"claim|name:shared-gpu",
		"claim|story:2/" + unverifiableStoryID,
	} {
		if gotIDs[forbidden] {
			t.Fatalf("dry-run wrongly included %q; got %+v", forbidden, dryRes.Items)
		}
	}
	if _, err := os.Stat(wsDone); err != nil {
		t.Fatalf("dry-run deleted workspace: %v", err)
	}
	if _, err := os.Stat(gradeDone); err != nil {
		t.Fatalf("dry-run deleted grade checkout: %v", err)
	}

	// Apply clears exact stale set and preserves open, unrelated, and unverifiable claims.
	applyJSON, err := runClaimCLI(t, root, "prune", "--stories-closed", "--apply", "--json")
	if err != nil {
		t.Fatalf("apply --stories-closed: %v\n%s", err, applyJSON)
	}
	var applyRes prune.Result
	if err := json.Unmarshal([]byte(applyJSON), &applyRes); err != nil {
		t.Fatal(err)
	}
	if !applyRes.Applied || applyRes.Counts.Removed == 0 {
		t.Fatalf("apply result = %+v", applyRes)
	}
	if _, err := os.Lstat(wsDone); !os.IsNotExist(err) {
		t.Fatalf("wsDone survived --apply: %v", err)
	}
	if _, err := os.Lstat(gradeDone); !os.IsNotExist(err) {
		t.Fatalf("gradeDone survived --apply: %v", err)
	}

	survivors, err := coord.List(coord.DefaultDir())
	if err != nil {
		t.Fatal(err)
	}
	survKeys := map[string]bool{}
	for _, c := range survivors {
		survKeys[c.Key()] = true
	}
	if len(survKeys) != 3 ||
		!survKeys["story:1/"+openStory.ID] ||
		!survKeys["name:shared-gpu"] ||
		!survKeys["story:2/"+unverifiableStoryID] {
		t.Fatalf("surviving claims = %v, want exactly openStory, shared-gpu, and unverifiableStory", survKeys)
	}
}

// Manager review safeguards:
// (a) live ModeAttached kernel-locked claim refuses closure across accept, wontfix,
//     move-off-sprint, and sprint end until released;
// (b) foreign claims and new-epoch / concurrently reopened claims are preserved on apply.
func TestAttachedRefusalAndNewEpochReopenedClaimsPreserved(t *testing.T) {
	_, root, _ := setupReclaimEnv(t)

	if out, code := runSprint(t, "add", "attached & epoch safeguards"); code != 0 {
		t.Fatalf("sprint add: %s", out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "boss", "--for", "1h"); code != 0 {
		t.Fatalf("sprint start: %s", out)
	}
	if out, code := runSprint(t, "track", "1"); code != 0 {
		t.Fatalf("sprint track: %s", out)
	}
	store := todopkg.RepoStore(root)
	st, err := todopkg.Add(store, "guarded by attached lock", "", "p0", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	st.Sprint = 1
	if _, err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	if out, code := runSprint(t, "claim", "1", st.ID, "--owner", "worker"); code != 0 {
		t.Fatalf("sprint claim: %s", out)
	}
	if out, code := runSprint(t, "submit", "1", st.ID, "--as", "worker", "-m", "ready"); code != 0 {
		t.Fatalf("sprint submit: %s", out)
	}

	// Hold a live kernel-locked ModeAttached claim naming this story.
	_, attachedLock, err := coord.AcquireAttachedRef(context.Background(), coord.Request{
		Ref:    coord.Ref{Kind: coord.KindName, Name: "lane-kernel"},
		Holder: principal.Ref{Name: "worker", Episode: "ep-child", Host: "box-a"},
		Story:  "1/" + st.ID,
		Mode:   coord.ModeAttached,
	})
	if err != nil {
		t.Fatal(err)
	}

	if out, code := runSprint(t, "accept", "1", st.ID, "--no-rating", "-m", "try"); code == 0 || !strings.Contains(out, "live attached claim") {
		_ = attachedLock.Release()
		t.Fatalf("sprint accept must refuse live attached claim: exit=%d\n%s", code, out)
	}
	if out, err := runTodoCLI(t, "--repo", "status", st.ID, "wontfix"); err == nil || !strings.Contains(out+err.Error(), "live attached claim") {
		_ = attachedLock.Release()
		t.Fatalf("todo wontfix must refuse live attached claim: err=%v out=%s", err, out)
	}
	if out, err := runTodoCLI(t, "--repo", "edit", st.ID, "--sprint", "0"); err == nil || !strings.Contains(out+err.Error(), "live attached claim") {
		_ = attachedLock.Release()
		t.Fatalf("todo move-sprint must refuse live attached claim: err=%v out=%s", err, out)
	}

	// Release the attached hold cleanly, then accept closes the story.
	if err := attachedLock.Release(); err != nil {
		t.Fatal(err)
	}
	if out, code := runSprint(t, "accept", "1", st.ID, "--no-rating", "-m", "now free"); code != 0 {
		t.Fatalf("sprint accept after attached release: %s", out)
	}

	// Seed two closed-story claims for backfill dry-run, then advance one to a
	// new epoch (for an open story) and reopen the other's story BEFORE apply:
	// ApplyClosedStories must preserve BOTH!
	reopenedStory, err := todopkg.Add(store, "will reopen before apply", "", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	reopenedStory.Sprint = 1
	reopenedStory.Status = todopkg.StatusDone
	reopenedStory.Closed = &now
	if _, err := store.Save(reopenedStory); err != nil {
		t.Fatal(err)
	}

	if _, err := coord.AcquireRef(context.Background(), coord.Request{
		Ref:    coord.Ref{Kind: coord.KindName, Name: "lane-reused"},
		Holder: principal.Ref{Name: "worker", Episode: "ep-old", Host: "box-a"},
		Story:  "1/" + st.ID,
		Mode:   coord.ModeLease,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.AcquireRef(context.Background(), coord.Request{
		Ref:    storyClaimRef(1, reopenedStory.ID),
		Holder: principal.Ref{Name: "worker", Episode: "ep-reopen", Host: "box-a"},
		Mode:   coord.ModeAnnounce,
		Force:  true,
	}); err != nil {
		t.Fatal(err)
	}

	dryItems, err := (weaveClosedStoryPruner{}).DryRunClosedStories(context.Background(), coord.DefaultDir(), prune.Options{})
	if err != nil || len(dryItems) != 2 {
		t.Fatalf("dryItems = %+v, err = %v; want 2", dryItems, err)
	}

	// 1) Re-acquire lane-reused at a new epoch (forced takeover by another worker).
	gNew, err := coord.AcquireRef(context.Background(), coord.Request{
		Ref:    coord.Ref{Kind: coord.KindName, Name: "lane-reused"},
		Holder: principal.Ref{Name: "other-worker", Episode: "ep-new", Host: "box-a"},
		Story:  "1/" + st.ID,
		Mode:   coord.ModeLease,
		Force:  true,
	})
	if err != nil || gNew.Epoch <= 1 {
		t.Fatalf("reacquire lane-reused: %+v %v", gNew, err)
	}

	// 2) Reopen reopenedStory before Apply runs.
	reopenedStory.Status = todopkg.StatusDoing
	reopenedStory.Closed = nil
	if _, err := store.Save(reopenedStory); err != nil {
		t.Fatal(err)
	}

	counts, err := (weaveClosedStoryPruner{}).ApplyClosedStories(context.Background(), coord.DefaultDir(), dryItems)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Removed != 0 {
		t.Fatalf("ApplyClosedStories removed %d item(s), want 0 (new-epoch + reopened preserved)", counts.Removed)
	}
	after, err := coord.List(coord.DefaultDir())
	if err != nil || len(after) != 2 {
		t.Fatalf("claims after guarded apply = %+v (%v), want both preserved", after, err)
	}
}

