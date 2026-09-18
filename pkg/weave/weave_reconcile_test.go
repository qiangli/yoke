package weave

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/issue"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// TestWeaveQueueSummariesActiveOnly locks in the cross-repo hint fix:
// the "where's the action" summary (activeOnly=true) skips a queue whose
// every item is terminal, while the machine overview (activeOnly=false)
// still lists it. This drives the real weaveQueueSummaries against
// on-disk queue dirs under an isolated HOME — the bug was a repo with
// only done/abandoned items being advertised as if there were something
// to cd to (and each of ycode/sh surfacing the other).
func TestWeaveQueueSummariesActiveOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	base := filepath.Join(home, ".agents", "ycode", "weave")
	doneRoot := t.TempDir()
	busyRoot := t.TempDir()

	writeQueue := func(tag string, q *weaveQueue) {
		dir := filepath.Join(base, tag)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := saveWeaveQueue(dir, q); err != nil {
			t.Fatal(err)
		}
	}
	writeQueue("done-repo-aaaa1111", &weaveQueue{Root: doneRoot, Items: []*weaveItem{
		{ID: 1, State: "done"}, {ID: 2, State: "abandoned"},
	}})
	writeQueue("busy-repo-bbbb2222", &weaveQueue{Root: busyRoot, Items: []*weaveItem{
		{ID: 1, State: "done"}, {ID: 2, State: "working", Tool: "codex"},
	}})

	// Match on the FULL root path, not its basename. t.TempDir numbers its
	// subdirectories 001/002/003 under one random parent, so a basename like
	// "002" is a substring of a sibling's path whenever the random component
	// happens to contain it ("...ActiveOnly3788002406/003" contains "002").
	// That made this test fail for a reason having nothing to do with weave,
	// on roughly whichever runs drew an unlucky number.
	var active bytes.Buffer
	weaveQueueSummaries(&active, "", true)
	if strings.Contains(active.String(), doneRoot) {
		t.Errorf("activeOnly=true should skip the all-terminal queue; got:\n%s", active.String())
	}
	if !strings.Contains(active.String(), busyRoot) {
		t.Errorf("activeOnly=true should list the queue with a working item; got:\n%s", active.String())
	}

	var overview bytes.Buffer
	weaveQueueSummaries(&overview, "", false)
	if !strings.Contains(overview.String(), doneRoot) {
		t.Errorf("activeOnly=false (machine overview) should still list the all-terminal queue; got:\n%s", overview.String())
	}
}

// A queue registered by a test repo used to survive t.TempDir cleanup and
// appear forever in `weave list --all`. Discovery must ignore that stale
// registration without deleting its queue/workspace, while a live repo in the
// same registry remains visible.
func TestWeaveListAllIgnoresMissingRepoAndPreservesLiveQueue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_AGENTIC", "")

	liveRoot := t.TempDir()
	missingRoot := filepath.Join(t.TempDir(), "removed-repo")
	stateRoot := weaveStateRoot(home)
	liveDir := filepath.Join(stateRoot, "live-repo-11111111")
	missingDir := filepath.Join(stateRoot, "removed-repo-22222222")
	for _, dir := range []string{liveDir, missingDir} {
		if err := os.MkdirAll(filepath.Join(dir, "workspaces", "issue-1"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveWeaveQueue(liveDir, &weaveQueue{Root: liveRoot, Items: []*weaveItem{{ID: 1, Title: "live marker", State: "working"}}}); err != nil {
		t.Fatal(err)
	}
	if err := saveWeaveQueue(missingDir, &weaveQueue{Root: missingRoot, Items: []*weaveItem{{ID: 2, Title: "missing marker", State: "working"}}}); err != nil {
		t.Fatal(err)
	}

	out, code := runWeave(t, "list", "--all", "--history", "--json")
	if code != 0 {
		t.Fatalf("list --all exit=%d out=%s", code, out)
	}
	if !strings.Contains(out, "live marker") {
		t.Fatalf("live repository disappeared from global discovery: %s", out)
	}
	if strings.Contains(out, "missing marker") {
		t.Fatalf("missing temporary repository remained visible: %s", out)
	}
	for _, dir := range []string{liveDir, missingDir} {
		if _, err := os.Stat(filepath.Join(dir, "workspaces", "issue-1")); err != nil {
			t.Fatalf("discovery deleted workspace %s: %v", dir, err)
		}
	}
}

// gitT runs a git command in dir, failing the test on error.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir,
		"-c", "user.email=t@t", "-c", "user.name=t",
		"-c", "commit.gpgsign=false"}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// setupMergeFixture builds a user repo (main + seed) and a workspace clone
// with one extra commit, returning (root, workspace, workspaceHeadSha). The
// workspace commit is NOT yet present in root — the caller decides whether
// to merge it, exercising both arms of weaveItemMerged.
func setupMergeFixture(t *testing.T) (root, workspace, sha string) {
	t.Helper()
	root = t.TempDir()
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "commit", "--allow-empty", "-qm", "seed")

	workspace = t.TempDir()
	gitT(t, workspace, "clone", "-q", root, ".")
	gitT(t, workspace, "checkout", "-q", "-b", "agent/weave-issue-1")
	gitT(t, workspace, "commit", "--allow-empty", "-qm", "agent work")
	sha = gitT(t, workspace, "rev-parse", "HEAD")
	return root, workspace, sha
}

func TestWeaveItemMerged(t *testing.T) {
	root, workspace, sha := setupMergeFixture(t)

	// Before merge: the agent commit lives only in the workspace clone, so
	// the sha is not reachable from main in root — not merged. This is
	// the exact bug case `git branch -d` could never detect (the branch
	// was never fetched into the user repo).
	it := &weaveItem{State: "submitted", Head: sha, Workspace: workspace, Branch: "agent/weave-issue-1", CommitsAhead: 1}
	if weaveItemMerged(root, "main", it) {
		t.Fatalf("expected not-merged before the workspace commit lands in main")
	}

	// Merge the agent branch into root's main (simulating an out-of-band
	// merge / a prior `weave pull`).
	gitT(t, root, "fetch", "-q", workspace, "agent/weave-issue-1:agent/weave-issue-1")
	gitT(t, root, "merge", "-q", "--no-ff", "-m", "merge issue 1", "agent/weave-issue-1")

	if !weaveItemMerged(root, "main", it) {
		t.Fatalf("expected merged after the workspace commit is an ancestor of main")
	}

	// No HEAD and no workspace → conservatively not-merged.
	empty := &weaveItem{State: "submitted"}
	if weaveItemMerged(root, "main", empty) {
		t.Fatalf("expected not-merged with no recorded head and no workspace")
	}

	// Zero commits ahead: HEAD equals the base commit, a trivial
	// ancestor of itself. This is an "empty" run (nothing to merge), NOT
	// a merged one — reconciling it to done would wrongly drop a clean
	// submitted item out of the list. CommitsAhead==0 must read as
	// not-merged even though the sha is technically reachable from base.
	baseSha := gitT(t, root, "rev-parse", "main")
	emptyRun := &weaveItem{State: "submitted", Head: baseSha, CommitsAhead: 0}
	if weaveItemMerged(root, "main", emptyRun) {
		t.Fatalf("expected not-merged for a zero-commit (empty) run")
	}
}

func TestWeaveItemMergedFallsBackToWorkspaceHead(t *testing.T) {
	root, workspace, sha := setupMergeFixture(t)
	gitT(t, root, "fetch", "-q", workspace, "agent/weave-issue-1:agent/weave-issue-1")
	gitT(t, root, "merge", "-q", "--no-ff", "-m", "merge issue 1", "agent/weave-issue-1")

	// Head unset on the item: weaveItemMerged should read the workspace's
	// live HEAD as a fallback and still resolve the merge state.
	it := &weaveItem{State: "submitted", Workspace: workspace, CommitsAhead: 1}
	if !weaveItemMerged(root, "main", it) {
		t.Fatalf("expected merged via workspace-HEAD fallback (sha %s)", sha[:7])
	}
}

func TestWeaveReconcileMerged(t *testing.T) {
	root, workspace, sha := setupMergeFixture(t)
	gitT(t, root, "fetch", "-q", workspace, "agent/weave-issue-1:agent/weave-issue-1")
	gitT(t, root, "merge", "-q", "--no-ff", "-m", "merge issue 1", "agent/weave-issue-1")

	q := &weaveQueue{Items: []*weaveItem{
		{ID: 1, State: "submitted", Head: sha, Workspace: workspace, CommitsAhead: 1}, // merged → flip
		{ID: 2, State: "submitted", Head: "deadbeef" + sha[8:], CommitsAhead: 1},      // bogus/unmerged → keep
		{ID: 3, State: "working", Head: sha, CommitsAhead: 1},                         // not submitted → keep
	}}
	n := weaveReconcileMerged(root, "main", q)
	if n != 1 {
		t.Fatalf("expected 1 reconciled, got %d", n)
	}
	if q.Items[0].State != "done" {
		t.Errorf("issue 1: expected done, got %q", q.Items[0].State)
	}
	if q.Items[1].State != "submitted" {
		t.Errorf("issue 2: expected submitted (unmerged), got %q", q.Items[1].State)
	}
	if q.Items[2].State != "working" {
		t.Errorf("issue 3: expected working (untouched), got %q", q.Items[2].State)
	}
}

func TestWeaveTerminalStateRequiresEvidence(t *testing.T) {
	cases := []struct {
		name     string
		exitCode int
		killedBy string
		ev       weaveTerminalEvidence
		want     string
	}{
		{
			name:     "clean exit with commit evidence",
			exitCode: 0,
			ev:       weaveTerminalEvidence{CommitsAhead: 1, Head: "abc"},
			want:     "submitted",
		},
		{
			name:     "clean exit with zero commits",
			exitCode: 0,
			ev:       weaveTerminalEvidence{CommitsAhead: 0, Head: "abc"},
			want:     "no-op",
		},
		{
			name:     "killed reason wins over clean exit",
			exitCode: 0,
			killedBy: "watchdog",
			ev:       weaveTerminalEvidence{CommitsAhead: 1, Head: "abc"},
			want:     "killed",
		},
		{
			name:     "signal exit is killed",
			exitCode: 143,
			ev:       weaveTerminalEvidence{CommitsAhead: 1, Head: "abc"},
			want:     "killed",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := weaveTerminalState(c.exitCode, nil, c.killedBy, c.ev); got != c.want {
				t.Fatalf("state = %q, want %q", got, c.want)
			}
		})
	}
}

func TestWeavePullRefusesEmptyButMergesSubmittedKilledEvidence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	root, workspace, baseSHA := setupEmptyBranchFixture(t)
	t.Chdir(root)
	root, err := weaveRepoRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	committedWorkspace := filepath.Join(dir, "workspaces", "issue-2")
	if err := os.MkdirAll(filepath.Dir(committedWorkspace), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(workspace, committedWorkspace); err != nil {
		t.Fatal(err)
	}
	workspace = committedWorkspace
	gitT(t, workspace, "commit", "--allow-empty", "-qm", "salvaged work")
	head := gitT(t, workspace, "rev-parse", "HEAD")
	emptyWorkspace := filepath.Join(dir, "workspaces", "issue-1")
	if err := os.MkdirAll(emptyWorkspace, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, emptyWorkspace, "clone", "-q", root, ".")
	gitT(t, emptyWorkspace, "checkout", "-q", "-b", "agent/weave-issue-empty")
	q := &weaveQueue{NextID: 3, Root: root, Items: []*weaveItem{
		{
			ID:           1,
			Title:        "empty run",
			State:        "submitted",
			Workspace:    emptyWorkspace,
			Branch:       "agent/weave-issue-empty",
			BaseSHA:      baseSHA,
			Head:         baseSHA,
			CommitsAhead: 0,
			Created:      time.Now().UTC(),
		},
		{
			ID:           2,
			Title:        "previously killed submitted run",
			State:        "submitted",
			Workspace:    workspace,
			Branch:       "agent/weave-issue-1",
			BaseSHA:      baseSHA,
			Head:         head,
			CommitsAhead: 1,
			KilledBy:     "watchdog",
			Created:      time.Now().UTC(),
		},
	}}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}

	out, code := runWeave(t, "pull", "1")
	if code != 0 {
		t.Fatalf("pull empty exit=%d out=%s", code, out)
	}
	if !strings.Contains(out, "empty") || !strings.Contains(out, "0 commits ahead") {
		t.Fatalf("pull empty output did not refuse with evidence:\n%s", out)
	}

	out, code = runWeave(t, "pull", "2")
	if code != 0 {
		t.Fatalf("pull submitted killed evidence exit=%d out=%s", code, out)
	}
	if !strings.Contains(out, "merged") {
		t.Fatalf("pull must merge a deliberately submitted item despite preserved killed_by evidence:\n%s", out)
	}
	q, err = loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := findWeaveItem(q, 2); got == nil || got.State != "done" || got.KilledBy != "watchdog" {
		t.Fatalf("pull must finish and preserve forensic killed_by evidence: %+v", got)
	}
}

func TestWeaveSalvagePromotesKilledWorkThroughPullGate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	root, workspace, baseSHA := setupEmptyBranchFixture(t)
	t.Chdir(root)
	root, err := weaveRepoRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	salvageWorkspace := filepath.Join(dir, "workspaces", "issue-1")
	if err := os.MkdirAll(filepath.Dir(salvageWorkspace), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(workspace, salvageWorkspace); err != nil {
		t.Fatal(err)
	}
	workspace = salvageWorkspace
	gitT(t, workspace, "commit", "--allow-empty", "-qm", "killed work salvaged")
	head := gitT(t, workspace, "rev-parse", "HEAD")
	q := &weaveQueue{NextID: 2, Root: root, Items: []*weaveItem{{
		ID:           1,
		Title:        "killed committed run",
		State:        "killed",
		Workspace:    workspace,
		Branch:       "agent/weave-issue-1",
		BaseSHA:      baseSHA,
		Head:         head,
		CommitsAhead: 1,
		KilledBy:     "watchdog",
		Created:      time.Now().UTC(),
	}}}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}

	// --no-review is the named escape: salvage refuses to merge unreviewed work
	// without it (TestWeaveSalvageRefusesMergeWithoutReviewVerdict pins that).
	// What this test is about is the promotion + forensic-evidence path.
	out, code := runWeave(t, "salvage", "1", "--no-review")
	if code != 0 || !strings.Contains(out, "merged") {
		t.Fatalf("salvage must merge killed committed work through pull's gate: exit=%d out=%s", code, out)
	}
	q, err = loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := findWeaveItem(q, 1); got == nil || got.State != "done" || got.KilledBy != "watchdog" {
		t.Fatalf("salvage must merge and retain forensic killed_by evidence: %+v", got)
	}
}

func TestWeaveKilledRunResumeClearsStaleEvidenceAndPullsFreshSubmission(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("system git not available; weave lifecycle needs it")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_AGENTIC", "")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")

	root := weaveTestRepo(t)
	t.Chdir(root)
	resolvedRoot, err := weaveRepoRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := weaveQueueDir(resolvedRoot)
	if err != nil {
		t.Fatal(err)
	}
	baseSHA := weaveTestGit(t, root, "rev-parse", "HEAD")
	workspace := filepath.Join(dir, "workspaces", "issue-1")
	if err := os.MkdirAll(filepath.Dir(workspace), 0o755); err != nil {
		t.Fatal(err)
	}
	weaveTestGit(t, root, "clone", "--local", "--no-hardlinks", root, workspace)
	weaveTestGit(t, workspace, "checkout", "-qb", "agent/weave-issue-1")

	verifyExit := 1
	killedExit := 143
	staleFinishedAt := time.Now().UTC().Add(-time.Minute)
	q := &weaveQueue{NextID: 2, Root: resolvedRoot, Items: []*weaveItem{{
		ID:              1,
		Title:           "fresh reassignment",
		Body:            "[killed by agy]\n\npreserve this historical note",
		State:           "killed",
		Workspace:       workspace,
		Branch:          "agent/weave-issue-1",
		BaseSHA:         baseSHA,
		Created:         time.Now().UTC().Add(-2 * time.Minute),
		FinishedAt:      staleFinishedAt,
		CommitsAhead:    3,
		Head:            "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		VerifyExit:      &verifyExit,
		VerifyOutput:    "old failed verification",
		VerifyTree:      "head",
		Dirty:           true,
		DirtyFiles:      2,
		UntrackedFiles:  1,
		AutoCommitted:   true,
		AutoCommitError: "old auto-commit failure",
		Throttled:       true,
		ThrottleSignal:  "rate-limit",
		ExitCode:        &killedExit,
		KilledBy:        "agy",
	}}}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}

	if out, code := runWeave(t, "start", "--issue", "1", "--resume", "--no-spawn", "--json"); code != 0 {
		t.Fatalf("resume no-spawn exit=%d out=%s", code, out)
	}
	q, err = loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	relaunched := findWeaveItem(q, 1)
	if relaunched == nil {
		t.Fatal("run #1 disappeared after relaunch")
	}
	if relaunched.KilledBy != "" || relaunched.ExitCode != nil || !relaunched.FinishedAt.IsZero() || relaunched.Head != "" || relaunched.CommitsAhead != 0 {
		t.Fatalf("relaunch kept stale terminal evidence: killed_by=%q exit=%v finished=%v head=%q commits=%d",
			relaunched.KilledBy, relaunched.ExitCode, relaunched.FinishedAt, relaunched.Head, relaunched.CommitsAhead)
	}
	if relaunched.VerifyExit != nil || relaunched.VerifyOutput != "" || relaunched.VerifyTree != "" || relaunched.Dirty || relaunched.DirtyFiles != 0 || relaunched.UntrackedFiles != 0 || relaunched.AutoCommitted || relaunched.AutoCommitError != "" || relaunched.Throttled || relaunched.ThrottleSignal != "" {
		t.Fatalf("relaunch kept stale verification/tree evidence: %+v", relaunched)
	}
	if !strings.Contains(relaunched.Body, "[killed by agy]") {
		t.Fatalf("relaunch dropped historical body note: %q", relaunched.Body)
	}

	script := "printf 'fresh\\n' > fresh.txt && git add fresh.txt && git commit -q -m fresh"
	if out, code := runWeave(t, "start", "--issue", "1", "--resume", "--json", "--", "sh", "-c", script); code != 0 {
		t.Fatalf("resume submission exit=%d out=%s", code, out)
	}
	q, err = loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	submitted := findWeaveItem(q, 1)
	if submitted == nil || submitted.State != "submitted" || submitted.KilledBy != "" || submitted.CommitsAhead <= 0 || submitted.Head == "" {
		t.Fatalf("fresh run did not submit clean evidence: %+v", submitted)
	}
	if out, code := runWeave(t, "pull", "1"); code != 0 || !strings.Contains(out, "merged") {
		t.Fatalf("pull fresh submission exit=%d out=%s", code, out)
	}
	if b, err := os.ReadFile(filepath.Join(root, "fresh.txt")); err != nil || string(b) != "fresh\n" {
		t.Fatalf("fresh submission was not merged into root: content=%q err=%v", b, err)
	}

	q, err = loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	q.Items = append(q.Items,
		// Truly killed = state "killed". killed_by alone no longer vetoes a
		// pull: an item that reached "submitted" did so by a deliberate
		// transition (salvage, or a fresh resume), and pull honors it. Only
		// a run still sitting in "killed" is refused here.
		&weaveItem{ID: 2, Title: "still killed", State: "killed", Workspace: workspace, Branch: "agent/weave-issue-2", BaseSHA: baseSHA, Head: submitted.Head, CommitsAhead: 1, KilledBy: "agy", Created: time.Now().UTC()},
		&weaveItem{ID: 3, Title: "still empty", State: "submitted", Workspace: workspace, Branch: "agent/weave-issue-3", BaseSHA: baseSHA, Head: baseSHA, CommitsAhead: 0, Created: time.Now().UTC()},
	)
	q.NextID = 4
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}
	if out, code := runWeave(t, "pull", "2"); code != 0 || !strings.Contains(out, "killed") || !strings.Contains(out, "agy") {
		t.Fatalf("pull truly killed exit=%d out=%s", code, out)
	}
	if out, code := runWeave(t, "pull", "3"); code != 0 || !strings.Contains(out, "empty") || !strings.Contains(out, "0 commits ahead") {
		t.Fatalf("pull zero-commit exit=%d out=%s", code, out)
	}
}

func TestWeaveCloseRegisterOnMergeRequiresMergedDiff(t *testing.T) {
	root, workspace, sha := setupMergeFixture(t)
	reg := &issue.Store{Root: root, Sub: todopkg.RepoSub}
	ri := &issue.Issue{
		ID:      "abcdef123456",
		Kind:    issue.KindBug,
		Title:   "fix real bug",
		Status:  todopkg.StatusDoing,
		Created: time.Now().UTC(),
	}
	if _, err := reg.Save(ri); err != nil {
		t.Fatal(err)
	}

	empty := &weaveItem{Register: ri.ID, Owner: "agent", State: "done", Head: gitT(t, root, "rev-parse", "main"), CommitsAhead: 0}
	weaveCloseRegisterOnMerge(root, "main", empty)
	got, err := reg.Resolve(ri.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == todopkg.StatusDone {
		t.Fatal("empty run closed the register")
	}

	unmerged := &weaveItem{Register: ri.ID, Owner: "agent", State: "done", Head: sha, Workspace: workspace, CommitsAhead: 1}
	weaveCloseRegisterOnMerge(root, "main", unmerged)
	got, err = reg.Resolve(ri.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == todopkg.StatusDone {
		t.Fatal("unmerged run closed the register")
	}

	gitT(t, root, "fetch", "-q", workspace, "agent/weave-issue-1:agent/weave-issue-1")
	gitT(t, root, "merge", "-q", "--no-ff", "-m", "merge issue 1", "agent/weave-issue-1")
	weaveCloseRegisterOnMerge(root, "main", unmerged)
	got, err = reg.Resolve(ri.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != todopkg.StatusDone || got.Resolution != "fixed" {
		t.Fatalf("merged diff did not close as fixed: %+v", got)
	}
}

func setupEmptyBranchFixture(t *testing.T) (root, workspace, baseSHA string) {
	t.Helper()
	root = t.TempDir()
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "commit", "--allow-empty", "-qm", "seed")
	baseSHA = gitT(t, root, "rev-parse", "HEAD")

	workspace = t.TempDir()
	gitT(t, workspace, "clone", "-q", root, ".")
	gitT(t, workspace, "checkout", "-q", "-b", "agent/weave-issue-1")
	return root, workspace, baseSHA
}

func TestWeaveTruncate(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"short", 40, "short"},
		{"exactly-ten", 11, "exactly-ten"},
		{strings.Repeat("a", 50), 40, strings.Repeat("a", 37) + "..."},
		// Multibyte: byte-slicing would split the emoji mid-rune and emit
		// the U+FFFD replacement char; rune-slicing must not.
		{"recurrence 🔁🔁🔁🔁 detail", 14, "recurrence " + "..."},
	}
	for _, c := range cases {
		got := weaveTruncate(c.in, c.max)
		if got != c.want {
			t.Errorf("weaveTruncate(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
		if strings.ContainsRune(got, '�') {
			t.Errorf("weaveTruncate(%q, %d) = %q contains a replacement char (mid-rune split)", c.in, c.max, got)
		}
	}
}
