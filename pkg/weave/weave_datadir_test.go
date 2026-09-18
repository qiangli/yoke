// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
)

// ycodeLaunch builds a resolved ycode-family launch the way weaveChildEnv
// receives one: a Launch whose ToolName is the registry tool name "ycode".
func ycodeLaunch(t *testing.T) *weaveAgentLaunch {
	t.Helper()
	return &weaveAgentLaunch{ToolName: agentlaunch.YcodeToolName, Nick: "yc-a", ModelName: "glm-5.2"}
}

// envVal returns the value of name in env, ok=false when absent.
func envVal(env []string, name string) (string, bool) {
	prefix := name + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix), true
		}
	}
	return "", false
}

// Two concurrently-launched ycode workers must receive DIFFERENT data dirs.
// ycode locks its store, so a shared dir is a hidden per-tool concurrency limit
// of 1 — the second worker dies on launch. The fix derives the dir from the
// per-issue run identity, so two issues get two dirs under the queue dir.
func TestWeaveChildEnvGivesConcurrentYcodeRunsDistinctDataDirs(t *testing.T) {
	queueDir := t.TempDir()
	ambient := []string{"PATH=/usr/bin", "HOME=/home/op"}
	launch := ycodeLaunch(t)

	a := weaveChildEnv(ambient, "/ws/issue-7", "agent/issue-7", "main", queueDir,
		&weaveItem{ID: 7, Title: "t", Body: "b", Owner: "yc-a"}, launch)
	b := weaveChildEnv(ambient, "/ws/issue-8", "agent/issue-8", "main", queueDir,
		&weaveItem{ID: 8, Title: "t", Body: "b", Owner: "yc-b"}, launch)

	dirA, ok := envVal(a, agentlaunch.YcodeDataDirEnv)
	if !ok {
		t.Fatalf("ycode worker A missing %s: names=%v", agentlaunch.YcodeDataDirEnv, names(a))
	}
	dirB, ok := envVal(b, agentlaunch.YcodeDataDirEnv)
	if !ok {
		t.Fatalf("ycode worker B missing %s: names=%v", agentlaunch.YcodeDataDirEnv, names(b))
	}
	if dirA == dirB {
		t.Fatalf("two concurrent ycode runs share one data dir %q — second worker would die on the store lock", dirA)
	}
	// The store must NOT live inside the git workspace (it would pollute the
	// tree and the diff); it must live under the queue state dir.
	if !strings.HasPrefix(dirA, queueDir) {
		t.Errorf("data dir %q is not under the queue dir %q", dirA, queueDir)
	}
	if filepath.Base(filepath.Dir(dirA)) != "agent-data" {
		t.Errorf("data dir %q is not under an agent-data/ slot", dirA)
	}
}

// A --resume re-runs `weave start` for the SAME issue, so it must re-derive the
// SAME path — not orphan a brand-new store each time.
func TestWeaveChildEnvReuseDataDirAcrossResume(t *testing.T) {
	queueDir := t.TempDir()
	ambient := []string{"PATH=/usr/bin"}
	launch := ycodeLaunch(t)
	it := &weaveItem{ID: 42, Title: "t", Body: "b", Owner: "yc-a"}

	first := weaveChildEnv(ambient, "/ws/issue-42", "agent/issue-42", "main", queueDir, it, launch)
	second := weaveChildEnv(ambient, "/ws/issue-42", "agent/issue-42", "main", queueDir, it, launch)

	dirA, _ := envVal(first, agentlaunch.YcodeDataDirEnv)
	dirB, _ := envVal(second, agentlaunch.YcodeDataDirEnv)
	if dirA == "" {
		t.Fatalf("first launch missing %s", agentlaunch.YcodeDataDirEnv)
	}
	if dirA != dirB {
		t.Fatalf("resumed run got a different data dir: first=%q second=%q — a resume must reuse, not orphan", dirA, dirB)
	}
}

// An operator who set YCODE_DATA_DIR (or YCODE_HOME) made a deliberate choice;
// weave must NEVER override it, even for a ycode tool. This is why the bug is
// invisible to an operator who happened to set one.
func TestWeaveChildEnvRespectsOperatorSetYcodeDataDir(t *testing.T) {
	queueDir := t.TempDir()
	operatorChoice := "/explicit/op-store"
	ambient := []string{
		"PATH=/usr/bin",
		agentlaunch.YcodeDataDirEnv + "=" + operatorChoice,
	}
	launch := ycodeLaunch(t)
	got := weaveChildEnv(ambient, "/ws/issue-9", "agent/issue-9", "main", queueDir,
		&weaveItem{ID: 9, Title: "t", Body: "b", Owner: "yc-a"}, launch)

	dir, ok := envVal(got, agentlaunch.YcodeDataDirEnv)
	if !ok {
		t.Fatalf("operator-set %s was dropped from the child env", agentlaunch.YcodeDataDirEnv)
	}
	if dir != operatorChoice {
		t.Fatalf("operator-set %s=%q was overridden to %q — a deliberate choice must never be clobbered",
			agentlaunch.YcodeDataDirEnv, operatorChoice, dir)
	}
}

// YCODE_HOME relocates the whole tree; setting it is also a deliberate choice,
// so weave must not additionally force a YCODE_DATA_DIR (it would conflict).
func TestWeaveChildEnvRespectsOperatorSetYcodeHome(t *testing.T) {
	queueDir := t.TempDir()
	ambient := []string{"PATH=/usr/bin", agentlaunch.YcodeHomeEnv + "=/explicit/home"}
	launch := ycodeLaunch(t)
	got := weaveChildEnv(ambient, "/ws/issue-10", "agent/issue-10", "main", queueDir,
		&weaveItem{ID: 10, Title: "t", Body: "b", Owner: "yc-a"}, launch)

	if dir, ok := envVal(got, agentlaunch.YcodeDataDirEnv); ok && !strings.HasPrefix(dir, queueDir) {
		// operator's own YCODE_DATA_DIR would already pass through verbatim; only
		// complain if weave MANUFACTURED one under the queue dir.
		t.Fatalf("weave manufactured %s=%q despite operator-set %s", agentlaunch.YcodeDataDirEnv, dir, agentlaunch.YcodeHomeEnv)
	}
}

// A non-ycode tool (claude, codex, opencode, …) is completely untouched — no
// YCODE_* injection, ever. The fix is keyed off the tool, never a hardcoded
// agent name, so the blast radius stops at ycode-family bindings.
func TestWeaveChildEnvLeavesNonYcodeToolUntouched(t *testing.T) {
	queueDir := t.TempDir()
	ambient := []string{"PATH=/usr/bin"}
	launch := &weaveAgentLaunch{ToolName: "claude", Nick: "007", ModelName: "fable"}
	got := weaveChildEnv(ambient, "/ws/issue-11", "agent/issue-11", "main", queueDir,
		&weaveItem{ID: 11, Title: "t", Body: "b", Owner: "007-a"}, launch)

	if envHas(got, agentlaunch.YcodeDataDirEnv) {
		dir, _ := envVal(got, agentlaunch.YcodeDataDirEnv)
		t.Fatalf("non-ycode tool received %s=%q — the fix must be keyed off the tool, not applied wholesale",
			agentlaunch.YcodeDataDirEnv, dir)
	}
}

// A bare/raw launch (no resolved agent) is also untouched.
func TestWeaveChildEnvLeavesNilLaunchUntouched(t *testing.T) {
	queueDir := t.TempDir()
	ambient := []string{"PATH=/usr/bin"}
	got := weaveChildEnv(ambient, "/ws/issue-12", "agent/issue-12", "main", queueDir,
		&weaveItem{ID: 12, Title: "t", Body: "b", Owner: "raw-a"}, nil)

	if envHas(got, agentlaunch.YcodeDataDirEnv) {
		dir, _ := envVal(got, agentlaunch.YcodeDataDirEnv)
		t.Fatalf("nil/raw launch received %s=%q", agentlaunch.YcodeDataDirEnv, dir)
	}
}

// --- end-to-end: two REAL concurrent ycode workers through the spawn path ---

// TestWeaveTwoConcurrentYcodeWorkersGetDistinctStores is the live reproduction
// of the reported bug: a SECOND ycode weave worker died instantly because both
// shared one locked data store. It launches two ycode weave workers AT THE SAME
// TIME through the real `weave start` cobra spawn path (genuine exec.Command
// subprocesses, real weaveChildEnv), with a stub `ycode` binary that records the
// YCODE_DATA_DIR each worker received. Both must start (exit clean) and the two
// recorded stores must be DIFFERENT — the precondition that prevents the
// "storage is locked by another ycode process" death.
func TestWeaveTwoConcurrentYcodeWorkersGetDistinctStores(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)

	// This test very likely runs INSIDE a ycode agent (the ambient env carries
	// YCODE_DATA_DIR for this very run). That operator-set value must be
	// respected in general (TestWeaveChildEnvRespectsOperatorSetYcodeDataDir),
	// but here we are simulating a CLEAN operator shell so the per-run
	// derivation is exercised end-to-end. Snapshot, unset, restore.
	for _, name := range []string{agentlaunch.YcodeDataDirEnv, agentlaunch.YcodeHomeEnv} {
		if cur, ok := os.LookupEnv(name); ok {
			t.Cleanup(func() { _ = os.Setenv(name, cur) })
			_ = os.Unsetenv(name)
		}
	}

	// Register a ycode agent + model in an isolated fleet catalog, and make
	// weave resolve against it (the same swap TestNamedYcodeChildEnv... uses).
	fleetRoot := t.TempDir()
	cat := fleet.New(fleet.WithRoot(fleetRoot))
	if err := cat.SaveModel(fleet.Model{Name: "glm-5.2", UpstreamID: "glm-5.2", Kind: "api", APIKeyRef: "ZAI_API_KEY"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "yc-worker", Tool: "ycode", Model: "glm-5.2"}); err != nil {
		t.Fatal(err)
	}
	prevCatalog := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(fleetRoot)) }
	t.Cleanup(func() { fleetCatalog = prevCatalog })

	// Stub `ycode` binary on PATH. It must satisfy the workspace preflight
	// (`ycode shell -c pwd` → print the cwd) AND, for the real launch, record
	// the YCODE_DATA_DIR it was handed.
	stubDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "workers.log")
	stub := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *\"shell -c pwd\"*) pwd; exit 0 ;;\n" + // workspace preflight (workspace_arg precedes it): report the cwd
		"  *) pid=$$; dir=${" + agentlaunch.YcodeDataDirEnv + "}; echo \"$pid $dir\" >> " + logPath + "; exit 0 ;;\n" + // agent launch: record the per-run store
		"esac\n"
	if err := os.WriteFile(filepath.Join(stubDir, "ycode"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// The ycode launch template carries --danger-skip-permissions, which an
	// uncontained host refuses; this sandbox is uncontained, so accept the risk
	// explicitly (the operator's documented opt-in).
	t.Setenv(agentlaunch.UnsafeLaunchEnv, "1")

	// Two issues for two workers.
	if _, code := runWeave(t, "add", "worker A task", "--json"); code != 0 {
		t.Fatal("weave add #1 failed")
	}
	if _, code := runWeave(t, "add", "worker B task", "--json"); code != 0 {
		t.Fatal("weave add #2 failed")
	}

	// Launch BOTH workers at the same time through the real spawn path.
	type result struct {
		out  string
		code int
	}
	res := make([]result, 2)
	done := make(chan struct{})
	go func() {
		res[0].out, res[0].code = runWeave(t, "start", "--run", "1", "--pty", "never", "--json", "--", "yc-worker")
		done <- struct{}{}
	}()
	go func() {
		res[1].out, res[1].code = runWeave(t, "start", "--run", "2", "--pty", "never", "--json", "--", "yc-worker")
		done <- struct{}{}
	}()
	<-done
	<-done

	for i, r := range res {
		if r.code != 0 {
			t.Fatalf("ycode worker %d did not start cleanly (exit %d):\n%s", i+1, r.code, r.out)
		}
	}

	// Both workers recorded the store they were handed. They must differ.
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("no worker records written — stub ycode was never launched:\n%+v", res)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected 2 worker records, got %d: %q", len(lines), string(data))
	}
	dirA := strings.TrimSpace(strings.TrimPrefix(lines[0], lines[0][:strings.IndexByte(lines[0], ' ')+1]))
	dirB := strings.TrimSpace(strings.TrimPrefix(lines[1], lines[1][:strings.IndexByte(lines[1], ' ')+1]))
	if dirA == "" || dirB == "" {
		t.Fatalf("a worker received no %s: %q", agentlaunch.YcodeDataDirEnv, string(data))
	}
	if dirA == dirB {
		t.Fatalf("two concurrent ycode workers share one store %q — the second would die on the lock\nrecords:\n%s",
			dirA, string(data))
	}
	t.Logf("TWO CONCURRENT YCODE WORKERS STARTED with distinct stores:\n  worker A: %s\n  worker B: %s\n%s",
		dirA, dirB, strings.TrimSpace(string(data)))
}
