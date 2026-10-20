package dks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/dag"
	"github.com/qiangli/yoke/pkg/dagrun"
	"github.com/qiangli/yoke/pkg/dagrun/cluster"
	"github.com/qiangli/yoke/pkg/fleet"
)

const (
	testWorkspaceID = "01JBQ5E9P2WYZ5ZG4HJ3XRG7W8"
	testRunID       = "01JC1ZK3V8N4Q7R2T5W9X0Y6AB"
	otherWorkspace  = "01JBQ5E9P2WYZ5ZG4HJ3XRG7ZZ"
	testExeSHA      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testAmdSHA      = "1111111111111111111111111111111111111111111111111111111111111111"
	testArmSHA      = "2222222222222222222222222222222222222222222222222222222222222222"
	testRunImage    = "example.invalid/bashy-executor:test"
)

// seqRunner is the run/ps/stop fake: it records argv + stdin and answers
// `get` from a queue (the last answer repeats), so a wait can watch a run
// move through phases. It never execs and never networks.
type seqRunner struct {
	calls   []runnerCall
	gets    [][]byte
	getExit int
	getErr  string
	apply   struct {
		exit   int
		stderr string
	}
}

func (r *seqRunner) Run(_ context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error) {
	r.calls = append(r.calls, runnerCall{argv: append([]string(nil), argv...), stdin: append([]byte(nil), stdin...)})
	switch action(argv) {
	case "get":
		if r.getExit != 0 {
			return nil, []byte(r.getErr), r.getExit, nil
		}
		if len(r.gets) == 0 {
			return []byte(`{"items":[]}`), nil, 0, nil
		}
		out := r.gets[0]
		if len(r.gets) > 1 {
			r.gets = r.gets[1:]
		}
		return out, nil, 0, nil
	case "apply":
		return nil, []byte(r.apply.stderr), r.apply.exit, nil
	}
	return nil, nil, 0, nil
}

func action(argv []string) string {
	for _, a := range argv {
		switch a {
		case "get", "apply", "delete":
			return a
		}
	}
	return ""
}

func (r *seqRunner) actions() []string {
	var out []string
	for _, c := range r.calls {
		out = append(out, action(c.argv))
	}
	return out
}

// runTest is the hermetic runEnv: state under t.TempDir via $BASHY_HOME,
// a fake runner, a fake local exec, a fake release and registry, a frozen
// clock whose sleep advances it.
type runTest struct {
	*testEnv
	renv     *runEnv
	runner   *seqRunner
	clock    time.Time
	sleeps   int
	registry map[string]fleet.Command
}

func newRunTest(t *testing.T) *runTest {
	t.Helper()
	te := newTestEnv(t)
	rt := &runTest{testEnv: te, runner: &seqRunner{}, clock: time.UnixMilli(1_730_000_000_000).Add(90 * time.Second), registry: map[string]fleet.Command{}}
	te.env.newRunner = func() cluster.Runner { return rt.runner }
	te.env.mintID = func() string { return testRunID }
	rt.renv = &runEnv{
		userlandEnv: te.env,
		release:     func() (releaseIdentity, error) { return releaseIdentity{Version: "v1.2.3", SHA256: testExeSHA}, nil },
		shippedArtifacts: func(string, releaseIdentity) (map[string]string, error) {
			return map[string]string{"linux/amd64": testAmdSHA, "linux/arm64": testArmSHA}, nil
		},
		isShipped:     func(name string) bool { return name == "echo" || name == "make" },
		lookupCommand: func(name string) (fleet.Command, bool) { c, ok := rt.registry[name]; return c, ok },
		getenv: func(k string) (string, bool) {
			if k == "CI" {
				return "true", true
			}
			return "", false
		},
		image: func() string { return testRunImage },
		now:   func() time.Time { return rt.clock },
		sleep: func(_ context.Context, d time.Duration) error {
			rt.sleeps++
			rt.clock = rt.clock.Add(d)
			return nil
		},
	}
	return rt
}

// selectWorkspace persists a selection the way `dks use` would.
func (rt *runTest) selectWorkspace(t *testing.T, arg string) dagrun.Context {
	t.Helper()
	sel, err := dagrun.ParseSelection(arg)
	if err != nil {
		t.Fatal(err)
	}
	ctxRec, err := dagrun.NewContext(sel, testWorkspaceID, "~/docs")
	if err != nil {
		t.Fatal(err)
	}
	store, err := dagrun.OpenStore(rt.env.stateDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Select(ctxRec); err != nil {
		t.Fatal(err)
	}
	return ctxRec
}

func (rt *runTest) exec(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	root := &cobra.Command{Use: "dks", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newRunCmds(rt.renv)...)
	root.SetArgs(args)
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	return userlandExitCode(root.Execute()), outBuf.String(), errBuf.String()
}

// noSideEffects asserts nothing ran locally and nothing reached a plane.
func (rt *runTest) noSideEffects(t *testing.T) {
	t.Helper()
	if len(rt.execCalls) != 0 {
		t.Errorf("executed locally: %v", rt.execCalls)
	}
	if len(rt.runner.calls) != 0 {
		t.Errorf("reached the plane: %v", rt.runner.actions())
	}
}

// runsJSON is a kubectl `get jobs,pods -o json` answer.
func runsJSON(items ...string) []byte {
	return []byte(`{"items":[` + strings.Join(items, ",") + `]}`)
}

func jobJSON(ws, run, command, condition string) string {
	status := `{"active":1}`
	if condition != "" {
		status = `{"conditions":[{"type":"` + condition + `","status":"True"}]}`
	}
	return fmt.Sprintf(`{"kind":"Job","metadata":{"name":"run-%s","labels":{%q:"bashy",%q:%q,%q:%q,%q:%q}},"status":%s}`,
		strings.ToLower(run), dagrun.LabelManagedBy, dagrun.LabelWorkspace, ws, dagrun.LabelRun, run, dagrun.LabelCommand, command, status)
}

func TestRunWithoutClusterContextNeverRunsLocally(t *testing.T) {
	rt := newRunTest(t)
	for _, args := range [][]string{
		{"run", "--", "make", "test"},
		{"run", "--detach", "--", "make", "test"},
		{"run", "--emit", "manifest", "--", "make"},
		{"ps"},
		{"stop", testRunID},
	} {
		code, stdout, stderr := rt.exec(t, args...)
		if code == 0 {
			t.Errorf("%q exited 0 without a selected workspace", args)
		}
		if !strings.Contains(stderr, "no workspace selected") || !strings.Contains(stderr, "bashy dks use") {
			t.Errorf("%q stderr %q is not the actionable no-context message", args, stderr)
		}
		if stdout != "" {
			t.Errorf("%q printed %q", args, stdout)
		}
	}
	rt.noSideEffects(t)
}

func TestRunEmitManifestDoesNotApply(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	code, stdout, stderr := rt.exec(t, "run", "--emit", "manifest", "--cpu", "2", "--memory", "4Gi", "--env", "CI", "--env", "MODE=fast", "--", "echo", "hello world")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want, err := dagrun.Generate(dagrun.GenerateInput{
		Command:     dagrun.CommandRef{Name: "echo", Digest: testExeSHA, Artifacts: map[string]string{"linux/amd64": testAmdSHA, "linux/arm64": testArmSHA}},
		Argv:        []string{"echo", "hello world"},
		WorkspaceID: testWorkspaceID,
		RunID:       testRunID,
		Cwd:         "~/docs",
		Env:         map[string]string{"CI": "true", "MODE": "fast"},
		Venue:       dag.VenueCluster,
		Run:         dagrun.RunSpec{Resources: dagrun.Resources{CPU: 2, Memory: "4Gi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout != string(want) {
		t.Errorf("emitted manifest differs from dagrun.Generate:\n--- got\n%s--- want\n%s", stdout, want)
	}
	rt.noSideEffects(t)
}

func TestRunEmitYAMLRendersWithoutApplying(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	code, stdout, stderr := rt.exec(t, "run", "--emit", "yaml", "--", "make")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{`"kind": "List"`, `"kind": "Job"`, testRunImage, testRunID} {
		if !strings.Contains(stdout, want) {
			t.Errorf("rendered objects miss %q:\n%s", want, stdout)
		}
	}
	_, again, _ := rt.exec(t, "run", "--emit", "yaml", "--", "make")
	if again != stdout {
		t.Error("rendered objects are not deterministic")
	}
	rt.noSideEffects(t)
}

func TestRunAppliesServerSideAndWaitsForSucceeded(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel@k8s:dev")
	rt.runner.gets = [][]byte{
		runsJSON(),
		runsJSON(jobJSON(testWorkspaceID, testRunID, "make", "")),
		runsJSON(jobJSON(testWorkspaceID, testRunID, "make", "Complete")),
	}
	code, stdout, stderr := rt.exec(t, "run", "--", "make", "test")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := strings.Join(rt.runner.actions(), ","); got != "apply,get,get,get" {
		t.Errorf("plane calls = %s, want apply then three lists", got)
	}
	apply := rt.runner.calls[0]
	if got := strings.Join(apply.argv, " "); got != "kubectl --context dev apply --server-side --field-manager=bashy -f -" {
		t.Errorf("apply argv = %q", got)
	}
	for _, want := range []string{testRunID, testRunImage, testWorkspaceID, `"kind":"Job"`} {
		if !bytes.Contains(apply.stdin, []byte(want)) {
			t.Errorf("applied payload misses %q", want)
		}
	}
	if rt.sleeps != 2 {
		t.Errorf("slept %d times, want 2", rt.sleeps)
	}
	if !strings.Contains(stdout, "succeeded") || !strings.Contains(stdout, "bashy dks logs") {
		t.Errorf("stdout %q misses the outcome or the logs note", stdout)
	}
	if len(rt.execCalls) != 0 {
		t.Errorf("executed locally: %v", rt.execCalls)
	}
}

func TestRunFailedPhaseExitsOne(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	rt.runner.gets = [][]byte{runsJSON(jobJSON(testWorkspaceID, testRunID, "make", "Failed"))}
	code, _, stderr := rt.exec(t, "run", "--", "make")
	if code != 1 || !strings.Contains(stderr, "failed") {
		t.Errorf("exit %d stderr %q, want 1 naming the failure", code, stderr)
	}
}

func TestRunWaitIsBoundedByTimeout(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	rt.runner.gets = [][]byte{runsJSON(jobJSON(testWorkspaceID, testRunID, "make", ""))}
	code, _, stderr := rt.exec(t, "run", "--timeout", "10s", "--", "make")
	if code != 1 || !strings.Contains(stderr, "still running") || !strings.Contains(stderr, "bashy dks stop "+testRunID) {
		t.Errorf("exit %d stderr %q, want 1 with the stop hint", code, stderr)
	}
	if rt.sleeps != 5 {
		t.Errorf("slept %d times, want 5 (10s / 2s)", rt.sleeps)
	}
}

func TestRunDetachPrintsRunIDWithoutWaiting(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	code, stdout, stderr := rt.exec(t, "run", "--detach", "--", "make")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if stdout != testRunID+"\n" {
		t.Errorf("stdout %q, want the run id", stdout)
	}
	if got := strings.Join(rt.runner.actions(), ","); got != "apply" {
		t.Errorf("plane calls = %s, want apply only", got)
	}
}

func TestRunUnknownCommandAppliesNothing(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	code, _, stderr := rt.exec(t, "run", "--", "no-such-cmd", "x")
	if code == 0 || !strings.Contains(stderr, "unknown command") {
		t.Errorf("exit %d stderr %q, want non-zero unknown command", code, stderr)
	}
	rt.noSideEffects(t)
}

func TestRunRegisteredCommandCarriesRecordAndDownloadDigests(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	rt.registry["fmtx"] = fleet.Command{Name: "fmtx", Kind: "command", Download: &fleet.CommandDownload{
		URL: "https://example.invalid/fmtx-{version}-{goos}-{goarch}", Version: "v1",
		SHA256: map[string]string{"linux/amd64": testAmdSHA, "darwin/arm64": testArmSHA},
	}}
	code, stdout, stderr := rt.exec(t, "run", "--emit", "manifest", "--", "fmtx", "-w")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	m, err := dagrun.ParseManifest([]byte(stdout))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := registeredRef(rt.registry["fmtx"])
	if m.Meta.Command.Digest != "sha256:"+want.Digest && m.Meta.Command.Digest != want.Digest {
		t.Errorf("record digest %q, want %q", m.Meta.Command.Digest, want.Digest)
	}
	if len(m.Meta.Artifact) != 2 || !strings.HasSuffix(m.Meta.Artifact["linux/amd64"], testAmdSHA) {
		t.Errorf("artifacts = %v", m.Meta.Artifact)
	}

	// An exec record pins nothing portable: ineligible, nothing applied.
	rt.registry["hostonly"] = fleet.Command{Name: "hostonly", Kind: "command", Exec: []string{"/usr/bin/true"}}
	code, _, stderr = rt.exec(t, "run", "--", "hostonly")
	if code == 0 || !strings.Contains(stderr, "per-platform artifact digest") {
		t.Errorf("exec record: exit %d stderr %q", code, stderr)
	}
	rt.noSideEffects(t)
}

func TestRunShippedWithoutProvenArtifactFailsClosed(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	rt.renv.shippedArtifacts = func(string, releaseIdentity) (map[string]string, error) {
		return nil, errors.New("no artifact digest for linux/amd64")
	}
	code, _, stderr := rt.exec(t, "run", "--", "make")
	if code == 0 || !strings.Contains(stderr, "no artifact digest for linux/amd64") {
		t.Errorf("exit %d stderr %q", code, stderr)
	}
	rt.noSideEffects(t)
}

// Only --env names travel: a bare K takes its exported value, an unset one
// fails closed, and nothing else of the environment is in the manifest.
func TestRunEnvIsAnExplicitAllowList(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	t.Setenv("SECRET_TOKEN", "leak")
	code, stdout, stderr := rt.exec(t, "run", "--emit", "manifest", "--env", "CI", "--", "make")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Env: CI=true") || strings.Contains(stdout, "SECRET_TOKEN") || strings.Contains(stdout, "BASHY_HOME") {
		t.Errorf("manifest env is not exactly the allow-list:\n%s", stdout)
	}
	code, _, stderr = rt.exec(t, "run", "--emit", "manifest", "--env", "UNSET_VAR", "--", "make")
	if code == 0 || !strings.Contains(stderr, "UNSET_VAR") {
		t.Errorf("unset --env: exit %d stderr %q", code, stderr)
	}
}

func TestRunUnreachablePlaneIsActionableNeverLocal(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	rt.runner.apply.exit = 1
	rt.runner.apply.stderr = "Unable to connect to the server: dial tcp: i/o timeout"
	code, _, stderr := rt.exec(t, "run", "--", "make")
	if code == 0 || !strings.Contains(stderr, "unreachable") || !strings.Contains(stderr, "nothing ran") {
		t.Errorf("exit %d stderr %q, want the dagrun unreachable message", code, stderr)
	}
	if len(rt.execCalls) != 0 {
		t.Errorf("fell back locally: %v", rt.execCalls)
	}
}

func TestPlaneErrorNamesUnsatisfiedCapability(t *testing.T) {
	err := planeError(dagrun.Context{}, &cluster.UnsatisfiedError{Capability: "native darwin executor (Stage B)"})
	if !strings.Contains(err.Error(), "no eligible executor: native darwin executor (Stage B)") {
		t.Errorf("unsatisfied: %v", err)
	}
	err = unsatisfiedOr(fmt.Errorf("%w: credentials [x] are not wired", dagrun.ErrUnsupportedExecutor))
	if !strings.Contains(err.Error(), "no eligible executor: credentials [x] are not wired") {
		t.Errorf("render unsupported: %v", err)
	}
}

func TestStopRejectsWeaveRunRef(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	code, _, stderr := rt.exec(t, "stop", "run:568")
	if code == 0 || !strings.Contains(stderr, "weave run") {
		t.Errorf("exit %d stderr %q, want the weave-ref refusal", code, stderr)
	}
	rt.noSideEffects(t)
}

func TestStopDeletesByRunLabelOnly(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	code, stdout, stderr := rt.exec(t, "stop", testRunID)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if len(rt.runner.calls) != 1 {
		t.Fatalf("calls = %v", rt.runner.actions())
	}
	got := strings.Join(rt.runner.calls[0].argv, " ")
	want := "kubectl delete jobs,pods,services,deployments,networkpolicies -l " + dagrun.LabelManagedBy + "=bashy," + dagrun.LabelRun + "=" + testRunID + " --wait=false"
	if got != want {
		t.Errorf("stop argv = %q, want %q", got, want)
	}
	if !strings.Contains(stdout, testRunID) {
		t.Errorf("stdout %q", stdout)
	}
}

func TestPsListsOnlyCurrentWorkspace(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	const older = "01JC1ZK3V0000000000000000A"
	rt.runner.gets = [][]byte{runsJSON(
		jobJSON(testWorkspaceID, testRunID, "make", "Complete"),
		jobJSON(otherWorkspace, "01JC1ZK3V8N4Q7R2T5W9X0Y6ZZ", "leak", ""),
		jobJSON(testWorkspaceID, older, "echo", ""),
	)}
	code, stdout, stderr := rt.exec(t, "ps")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	sel := rt.runner.calls[0].argv
	if got := strings.Join(sel, " "); !strings.Contains(got, dagrun.LabelWorkspace+"="+testWorkspaceID) {
		t.Errorf("ps selector %q is not the current workspace", got)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("ps output:\n%s", stdout)
	}
	if f := strings.Fields(lines[0]); strings.Join(f, " ") != "RUN COMMAND PHASE AGE EXECUTOR" {
		t.Errorf("header %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], older) || !strings.HasPrefix(lines[2], testRunID) {
		t.Errorf("rows not in run-id order:\n%s", stdout)
	}
	if strings.Contains(stdout, "leak") || strings.Contains(stdout, otherWorkspace) {
		t.Errorf("ps shows another workspace's run:\n%s", stdout)
	}
	if f := strings.Fields(lines[2]); len(f) != 5 || f[2] != "succeeded" || f[4] != "Job/run-"+strings.ToLower(testRunID) {
		t.Errorf("row %q", lines[2])
	}
	_, again, _ := rt.exec(t, "ps")
	if again != stdout {
		t.Error("ps output is not deterministic")
	}
}

func TestRunAge(t *testing.T) {
	ts, ok := ulidTime(testWorkspaceID)
	if !ok {
		t.Fatal("ulidTime rejected a ULID")
	}
	if got := runAge(testWorkspaceID, ts.Add(90*time.Second)); got != "1m" {
		t.Errorf("age = %q, want 1m", got)
	}
	if got := runAge("not-a-ulid", ts); got != "-" {
		t.Errorf("age = %q, want -", got)
	}
}

// The real registration replaces the closed doors: run, ps and stop are
// live commands, logs and attach stay fail-closed.
func TestRunPsStopAreRegistered(t *testing.T) {
	for _, c := range NewUserlandCmds() {
		switch c.Name() {
		case "run", "ps", "stop":
			if strings.Contains(c.Short, "not available") {
				t.Errorf("%s is still a closed door", c.Name())
			}
		}
	}
}
