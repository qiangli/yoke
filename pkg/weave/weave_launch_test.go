package weave

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A conductor commonly starts weave from a detached, reviewed commit while a
// shared main ref advances under another campaign.  The workspace must fork
// the checkout the conductor actually used, not whichever commit `main`
// resolves to inside a local clone.
func TestWeaveStartPinsDetachedSourceHEAD(t *testing.T) {
	root := setupIsolationFixture(t)
	old := strings.TrimSpace(gitT(t, root, "rev-parse", "HEAD"))
	gitT(t, root, "checkout", "-q", "main")
	mustWrite(t, root+"/new-main.txt", "new\n")
	gitT(t, root, "add", "new-main.txt")
	gitT(t, root, "commit", "-qm", "advance main")
	gitT(t, root, "checkout", "-q", "--detach", old)
	t.Chdir(root)

	if _, code := runWeave(t, "add", "pin detached base", "--json"); code != 0 {
		t.Fatal("weave add failed")
	}
	if out, code := runWeave(t, "start", "--issue", "1", "--no-spawn", "--", "sh", "-c", "true"); code != 0 {
		t.Fatalf("weave start failed (exit %d): %s", code, out)
	}
	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	it := findWeaveItem(q, 1)
	if it == nil || it.BaseSHA != old {
		t.Fatalf("base SHA = %#v, want detached source HEAD %s", it, old)
	}
	// --no-spawn leaves the provisioned item allocated. This is the
	// pre-agent observability contract: a conductor can see the workspace,
	// immutable base, and current provisioning phase even if hydration is
	// still running in another start invocation.
	if it.State != "allocated" || it.Workspace == "" || it.LaunchPhase == "" {
		t.Fatalf("pre-agent provisioning was not durably observable: state=%q workspace=%q phase=%q", it.State, it.Workspace, it.LaunchPhase)
	}
	if got := strings.TrimSpace(gitT(t, it.Workspace, "rev-parse", "HEAD")); got != old {
		t.Fatalf("workspace HEAD = %s, want detached source HEAD %s", got, old)
	}
}

func TestWeaveResumeReassignsOwnerAndLaunchTogether(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	pinAgentFleet(t)
	if _, code := runWeave(t, "add", "reassignment", "--body", "body", "--json"); code != 0 {
		t.Fatal("weave add failed")
	}
	if out, code := runWeave(t, "start", "--run", "1", "--no-spawn", "--tool", "smarty", "--json"); code != 0 {
		t.Fatalf("initial start failed (exit %d): %s", code, out)
	}
	if out, code := runWeave(t, "start", "--run", "1", "--resume", "--no-spawn", "--tool", "agy:gemini3.1", "--json"); code != 0 {
		t.Fatalf("resume failed (exit %d): %s", code, out)
	}
	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	it := findWeaveItem(q, 1)
	if it.Owner != "agy-gemini3.1-a" || it.Tool != "" {
		// --no-spawn deliberately clears Tool after allocation; ownership and
		// the durable launch spec still describe who the resume selected.
		t.Fatalf("resumed item owner/tool = %q/%q", it.Owner, it.Tool)
	}
	if it.LaunchSpec == nil || it.LaunchSpec.Agent != "agy-gemini3.1" || strings.TrimSuffix(filepath.Base(it.LaunchSpec.Tool), ".exe") != "agy" {
		t.Fatalf("resumed launch spec = %+v", it.LaunchSpec)
	}
	last := it.Comments[len(it.Comments)-1].Body
	if last != "reassigned 007-a → agy-gemini3.1-a" {
		t.Fatalf("last assignment comment = %q", last)
	}
}

func TestWeaveStartRejectsFlagsAfterAgentBeforeProvisioning(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	pinAgentFleet(t)
	if _, code := runWeave(t, "add", "bad flag placement", "--body", "body", "--json"); code != 0 {
		t.Fatal("weave add failed")
	}
	out, code := runWeave(t, "start", "--run", "1", "--", "smarty", "--json", "--quiet")
	if code == 0 || !strings.Contains(out, "registered agent") || !strings.Contains(out, "before `--`") {
		t.Fatalf("misplaced agent flags exit=%d output=%q", code, out)
	}
	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	it := findWeaveItem(q, 1)
	if it.State != "todo" || it.Workspace != "" || it.Owner != "" || it.LaunchSpec != nil {
		t.Fatalf("rejected launch provisioned or attributed work: %+v", it)
	}
}

func TestStartRefusesBelowTheFreeSpaceThreshold(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	if _, code := runWeave(t, "add", "disk guard", "--json"); code != 0 {
		t.Fatal("weave add failed")
	}
	oldProbe := weaveWorkspaceFreeSpaceFn
	weaveWorkspaceFreeSpaceFn = func(path string) (weaveFreeSpace, error) {
		return weaveFreeSpace{Bytes: weaveWorkspaceFreeSpaceThreshold - 1}, nil
	}
	t.Cleanup(func() { weaveWorkspaceFreeSpaceFn = oldProbe })

	out, code := runWeave(t, "start", "--run", "1", "--no-spawn", "--tool", "sh")
	if code == 0 {
		t.Fatalf("start succeeded below free-space threshold: %s", out)
	}
	if !strings.Contains(out, "free space") || !strings.Contains(out, "threshold") {
		t.Fatalf("refusal must name free space and threshold: %s", out)
	}
	dir, _ := weaveQueueDir(root)
	q, _ := loadWeaveQueue(dir)
	it := findWeaveItem(q, 1)
	if it.State != "todo" || it.Workspace != "" || it.WrapperPid != 0 {
		t.Fatalf("refused start must leave the run queued: %+v", it)
	}
}

func TestStartProceedsWhenSpaceIsAvailable(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	if _, code := runWeave(t, "add", "disk guard", "--json"); code != 0 {
		t.Fatal("weave add failed")
	}
	oldProbe := weaveWorkspaceFreeSpaceFn
	weaveWorkspaceFreeSpaceFn = func(path string) (weaveFreeSpace, error) {
		return weaveFreeSpace{Bytes: weaveWorkspaceFreeSpaceThreshold}, nil
	}
	t.Cleanup(func() { weaveWorkspaceFreeSpaceFn = oldProbe })

	if out, code := runWeave(t, "start", "--run", "1", "--no-spawn", "--tool", "sh", "--json"); code != 0 {
		t.Fatalf("start refused at threshold: exit=%d %s", code, out)
	}
	dir, _ := weaveQueueDir(root)
	q, _ := loadWeaveQueue(dir)
	it := findWeaveItem(q, 1)
	if it.State != "allocated" || it.Workspace == "" {
		t.Fatalf("start did not allocate with sufficient space: %+v", it)
	}
}

func TestPointRuntimeBudgetScaleAndExplicitCap(t *testing.T) {
	wants := map[int]time.Duration{
		1: 20 * time.Minute,
		2: 32 * time.Minute,
		3: 48 * time.Minute,
		5: 80 * time.Minute,
		8: 120 * time.Minute,
	}
	for points, want := range wants {
		got, err := weaveBoundRuntime(points, 0)
		if err != nil || got != want {
			t.Errorf("points %d: runtime=%s err=%v, want %s", points, got, err, want)
		}
		if got, err := weaveBoundRuntime(points, want-time.Second); err != nil || got != want-time.Second {
			t.Errorf("points %d tighter explicit runtime=%s err=%v", points, got, err)
		}
		if _, err := weaveBoundRuntime(points, want+time.Second); err == nil {
			t.Errorf("points %d accepted runtime above cap", points)
		}
	}
	for _, points := range []int{-1, 4, 13} {
		if _, err := weaveBoundRuntime(points, 0); err == nil {
			t.Errorf("invalid points %d accepted", points)
		}
	}
	if _, err := weaveBoundRuntime(2, -time.Second); err == nil {
		t.Fatal("negative explicit runtime accepted")
	}
	if got, err := weaveBoundRuntime(0, 0); err != nil || got != 0 {
		t.Fatalf("legacy unpointed runtime=%s err=%v", got, err)
	}
}

func TestPointedStartAndResumePersistBoundedRuntime(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "derived"
		if explicit {
			name = "explicit"
		}
		t.Run(name, func(t *testing.T) {
			root := setupIsolationFixture(t)
			t.Chdir(root)
			if _, code := runWeave(t, "add", "bounded", "--points", "1", "--json"); code != 0 {
				t.Fatal("weave add failed")
			}
			args := []string{"start", "--run", "1", "--no-spawn", "--tool", "sh", "--json"}
			if explicit {
				args = append(args, "--max-runtime", "5m")
			}
			if out, code := runWeave(t, args...); code != 0 {
				t.Fatalf("bounded start failed (exit %d): %s", code, out)
			}
			dir, _ := weaveQueueDir(root)
			q, _ := loadWeaveQueue(dir)
			it := findWeaveItem(q, 1)
			want := 20 * time.Minute // derived 1-point launch ceiling
			if explicit {
				want = 5 * time.Minute
			}
			if it.LaunchSpec == nil || it.LaunchSpec.MaxRuntime != want {
				t.Fatalf("launch spec = %+v", it.LaunchSpec)
			}
			if out, code := runWeave(t, "start", "--run", "1", "--resume", "--no-spawn", "--tool", "sh", "--max-runtime", "4m", "--json"); code != 0 {
				t.Fatalf("bounded resume failed (exit %d): %s", code, out)
			}
			q, _ = loadWeaveQueue(dir)
			it = findWeaveItem(q, 1)
			if it.LaunchSpec == nil || it.LaunchSpec.MaxRuntime != 4*time.Minute {
				t.Fatalf("resumed launch spec = %+v", it.LaunchSpec)
			}
		})
	}
}

func TestPointedStartRejectsRuntimeAboveCapBeforeProvisioning(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	if _, code := runWeave(t, "add", "too long", "--points", "1", "--json"); code != 0 {
		t.Fatal("weave add failed")
	}
	out, code := runWeave(t, "start", "--run", "1", "--no-spawn", "--tool", "sh", "--max-runtime", "21m")
	if code == 0 || !strings.Contains(out, "exceeds the 1-point cap 20m0s") {
		t.Fatalf("over-cap start exit=%d output=%q", code, out)
	}
	dir, _ := weaveQueueDir(root)
	q, _ := loadWeaveQueue(dir)
	it := findWeaveItem(q, 1)
	if it.State != "todo" || it.Workspace != "" || it.LaunchSpec != nil {
		t.Fatalf("over-cap launch mutated item: %+v", it)
	}
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		findWeaveItem(q, 1).Points = 4 // corrupt legacy state must fail closed
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out, code = runWeave(t, "start", "--run", "1", "--no-spawn", "--tool", "sh")
	if code == 0 || !strings.Contains(out, "invalid points 4") {
		t.Fatalf("invalid-point start exit=%d output=%q", code, out)
	}
	q, _ = loadWeaveQueue(dir)
	it = findWeaveItem(q, 1)
	if it.State != "todo" || it.Workspace != "" || it.LaunchSpec != nil {
		t.Fatalf("invalid-point launch mutated item: %+v", it)
	}
}

// Provisioning happens before an agent exists to report trouble. Its failure
// must therefore become durable queue state rather than returning to a silent
// todo item that looks like no worker was ever launched.
func TestWeaveProvisioningFailureIsTerminalAndObservable(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	if _, code := runWeave(t, "add", "hydration timeout", "--json"); code != 0 {
		t.Fatal("weave add failed")
	}
	dir, _ := weaveQueueDir(root)
	weaveMarkLaunchFailed(dir, 1, errHydrationTestFailure{})
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	it := findWeaveItem(q, 1)
	if it == nil || it.State != "failed" || it.FinishedAt.IsZero() || !strings.Contains(it.LaunchPhase, "hydration test timeout") {
		t.Fatalf("provisioning failure was not durable terminal evidence: %#v", it)
	}
}

type errHydrationTestFailure struct{}

func (errHydrationTestFailure) Error() string { return "hydration test timeout" }

func TestWeaveStartRefusesInvalidPreExistingWorkspace(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	if _, code := runWeave(t, "add", "workspace reuse check", "--json"); code != 0 {
		t.Fatal("weave add failed")
	}
	dir, _ := weaveQueueDir(root)
	ws := filepath.Join(dir, "workspaces", "issue-1")

	// 1. Existing workspace is NOT a git repo: must refuse and name the directory.
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "junk.txt"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := runWeave(t, "start", "--run", "1", "--no-spawn", "--tool", "sh")
	if code == 0 || !strings.Contains(out, ws) {
		t.Fatalf("start on non-git workspace should fail and name dir %q (code=%d): %s", ws, code, out)
	}

	// 2. Existing workspace is a git repo on the wrong branch (main instead of agent/weave-issue-1).
	_ = os.RemoveAll(ws)
	gitT(t, root, "clone", "--local", "--no-hardlinks", root, ws)
	out, code = runWeave(t, "start", "--run", "1", "--no-spawn", "--tool", "sh")
	if code == 0 || !strings.Contains(out, ws) {
		t.Fatalf("start on wrong branch workspace should fail and name dir %q (code=%d): %s", ws, code, out)
	}

	// 3. Existing workspace is on agent/weave-issue-1 but dirty (e.g. unpopulated tracked deletions).
	_ = os.RemoveAll(ws)
	gitT(t, root, "clone", "--local", "--no-hardlinks", root, ws)
	gitT(t, ws, "checkout", "-q", "-b", "agent/weave-issue-1")
	if err := os.Remove(filepath.Join(ws, "seed.txt")); err != nil {
		t.Fatal(err)
	}
	out, code = runWeave(t, "start", "--run", "1", "--no-spawn", "--tool", "sh")
	if code == 0 || !strings.Contains(out, ws) {
		t.Fatalf("start on unpopulated workspace should fail and name dir %q (code=%d): %s", ws, code, out)
	}

	// 4. Test that weaveMarkLaunchFailed cleans up the half-made workspace before returning.
	weaveMarkLaunchFailed(dir, 1, fmt.Errorf("simulated launch failure"))
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatalf("weaveMarkLaunchFailed did not remove half-made workspace %s: err=%v", ws, err)
	}

	// 5. Existing workspace is a clean git repo on agent/weave-issue-1: start accepts it.
	_ = os.RemoveAll(ws)
	gitT(t, root, "clone", "--local", "--no-hardlinks", root, ws)
	gitT(t, ws, "checkout", "-q", "-b", "agent/weave-issue-1")
	out, code = runWeave(t, "start", "--run", "1", "--no-spawn", "--tool", "sh")
	if code != 0 {
		t.Fatalf("start on clean matching workspace should succeed (code=%d): %s", code, out)
	}
}

// An external kill can happen while clone/hydration is running, before the
// item becomes working.  That must not leave a permanently allocated phantom
// worker with no wrapper and no terminal evidence.
func TestWeaveStatusObservesDeadProvisioningLauncherAndDoctorRecovers(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	dir, _ := weaveQueueDir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID:          1,
		Title:       "orphaned hydration",
		State:       "allocated",
		Workspace:   root + "/workspace",
		BaseSHA:     "immutable-base",
		LaunchPhase: "hydrating submodules",
		WrapperPid:  2147483647, // deliberately impossible PID on supported hosts
	}}}); err != nil {
		t.Fatal(err)
	}
	out, code := runWeave(t, "status", "1", "--json")
	if code != 0 {
		t.Fatalf("status failed (exit %d): %s", code, out)
	}
	for _, want := range []string{
		`"state": "allocated"`,
		`"launch_phase": "hydrating submodules"`,
		`"base_sha": "immutable-base"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("status missing %s:\n%s", want, out)
		}
	}
	if out, code = runWeave(t, "doctor", "--json"); code != 0 || !strings.Contains(out, `"to": "failed"`) {
		t.Fatalf("doctor did not recover dead provisioning launcher: exit=%d output=%s", code, out)
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	it := findWeaveItem(q, 1)
	if it == nil || it.FinishedAt.IsZero() || it.WrapperPid != 0 {
		t.Fatalf("orphan recovery was not durable: %#v", it)
	}
}

// Older launchers recorded allocated/hydrating state before they recorded a
// wrapper PID. Once the bounded provisioning interval elapses that is durable
// orphan evidence, but deliberate no-spawn/manual allocations remain available
// because they carry no active provisioning start time.
func TestWeaveRecoverOrphanedAllocationsWithoutPID(t *testing.T) {
	root := setupIsolationFixture(t)
	dir, _ := weaveQueueDir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{
		{
			ID:          1,
			Title:       "old hydrating launcher disappeared",
			State:       "allocated",
			LaunchPhase: "hydrating submodules",
			StartedAt:   time.Now().UTC().Add(-weaveProvisioningTimeout - time.Second),
		},
		{
			ID:          2,
			Title:       "recent hydrating launcher",
			State:       "allocated",
			LaunchPhase: "hydrating submodules",
			StartedAt:   time.Now().UTC(),
		},
		{
			ID:          3,
			Title:       "manual allocation",
			State:       "allocated",
			LaunchPhase: "hydrating submodules",
		},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := weaveRecoverOrphanedAllocations(dir); err != nil {
		t.Fatal(err)
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := findWeaveItem(q, 1); got == nil || got.State != "failed" || got.FinishedAt.IsZero() {
		t.Fatalf("missing-PID hydrating allocation was not recovered: %#v", got)
	}
	if got := findWeaveItem(q, 2); got == nil || got.State != "allocated" || !got.FinishedAt.IsZero() {
		t.Fatalf("recent missing-PID allocation must remain observable: %#v", got)
	}
	if got := findWeaveItem(q, 3); got == nil || got.State != "allocated" || !got.FinishedAt.IsZero() {
		t.Fatalf("manual allocation must remain resumable: %#v", got)
	}
}

func TestWeaveProvisioningLimitFromEnv(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want time.Duration
	}{
		{"", weaveProvisioningTimeout},
		{"15m", 15 * time.Minute},
		{"90s", 90 * time.Second},
		{"bogus", weaveProvisioningTimeout},
		{"-1m", weaveProvisioningTimeout},
		{"0", weaveProvisioningTimeout},
	} {
		t.Setenv("BASHY_WEAVE_PROVISION_TIMEOUT", tc.env)
		if got := weaveProvisioningLimit(); got != tc.want {
			t.Errorf("BASHY_WEAVE_PROVISION_TIMEOUT=%q: got %s, want %s", tc.env, got, tc.want)
		}
	}
}
