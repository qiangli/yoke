package dks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
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
	secrets  map[string][]byte
}

func newRunTest(t *testing.T) *runTest {
	t.Helper()
	te := newTestEnv(t)
	rt := &runTest{
		testEnv:  te,
		runner:   &seqRunner{},
		clock:    time.UnixMilli(1_730_000_000_000).Add(90 * time.Second),
		registry: map[string]fleet.Command{},
		secrets:  map[string][]byte{},
	}
	te.env.newRunner = func() cluster.Runner { return rt.runner }
	te.env.mintID = func() string { return testRunID }
	rt.renv = &runEnv{
		userlandEnv: te.env,
		release:     func() (releaseIdentity, error) { return releaseIdentity{Version: "v1.2.3", SHA256: testExeSHA}, nil },
		shippedArtifacts: func(string, releaseIdentity) (map[string]string, error) {
			return map[string]string{"linux/amd64": testAmdSHA, "linux/arm64": testArmSHA}, nil
		},
		isShipped:     func(name string) bool { return name == "echo" || name == "make" || name == "claude" },
		lookupCommand: func(name string) (fleet.Command, bool) { c, ok := rt.registry[name]; return c, ok },
		getenv: func(k string) (string, bool) {
			if k == "CI" {
				return "true", true
			}
			return "", false
		},
		image: func() string { return testRunImage },
		secretResolver: func(_ context.Context, vaultName string) ([]byte, error) {
			if val, ok := rt.secrets[vaultName]; ok {
				return val, nil
			}
			return nil, fmt.Errorf("secret %q not found in test fake", vaultName)
		},
		recordRunKind:   func(fleet.Command) dagrun.RunKind { return "" },
		stdinIsTerminal: func() bool { return false },
		now:             func() time.Time { return rt.clock },
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
		// A shipped command on the image executor: release + image, nothing fetched.
		Command:     dagrun.CommandRef{Name: "echo", Digest: testExeSHA, Release: "v1.2.3", Image: testRunImage},
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
	code, stdout, stderr := rt.exec(t, "run", "--detach", "--schedule-check", "0", "--", "make")
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

// runPodJSON is a run's Pod with the given phase and PodScheduled condition
// ("" = no condition).
func runPodJSON(run, phase, scheduled, reason, message string) string {
	conds := ""
	if scheduled != "" {
		conds = fmt.Sprintf(`,"conditions":[{"type":"PodScheduled","status":%q,"reason":%q,"message":%q}]`, scheduled, reason, message)
	}
	return fmt.Sprintf(`{"kind":"Pod","metadata":{"name":"run-%s-abcde","labels":{%q:"bashy",%q:%q,%q:%q,%q:"seq"}},"status":{"phase":%q%s}}`,
		strings.ToLower(run), dagrun.LabelManagedBy, dagrun.LabelWorkspace, testWorkspaceID, dagrun.LabelRun, run, dagrun.LabelCommand, phase, conds)
}

const insufficientCPU = "0/5 nodes are available: 5 Insufficient cpu."

func TestRunUnschedulableIsRefusedAndCleanedUp(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	rt.runner.gets = [][]byte{
		runsJSON(jobJSON(testWorkspaceID, testRunID, "seq", "")),
		runsJSON(jobJSON(testWorkspaceID, testRunID, "seq", ""), runPodJSON(testRunID, "Pending", "False", "Unschedulable", insufficientCPU)),
	}
	code, _, stderr := rt.exec(t, "run", "--cpu", "64", "--", "make", "test")
	if code == 0 {
		t.Fatalf("exit 0 for an unschedulable run; stderr %q", stderr)
	}
	for _, want := range []string{"unsatisfied resources", insufficientCPU, "--cpu 64", "nothing ran"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr %q misses %q", stderr, want)
		}
	}
	if got := strings.Join(rt.runner.actions(), ","); got != "apply,get,get,delete" {
		t.Fatalf("plane calls = %s, want apply, two lists, one delete and no further polling", got)
	}
	del := strings.Join(rt.runner.calls[3].argv, " ")
	if !strings.Contains(del, "-l "+dagrun.LabelManagedBy+"=bashy,"+dagrun.LabelRun+"="+testRunID) {
		t.Errorf("delete argv %q does not select by run label", del)
	}
	if rt.sleeps != 1 {
		t.Errorf("slept %d times, want 1", rt.sleeps)
	}
}

func TestRunPlainPendingKeepsWaiting(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	rt.runner.gets = [][]byte{
		runsJSON(jobJSON(testWorkspaceID, testRunID, "seq", ""), runPodJSON(testRunID, "Pending", "", "", "")),
		runsJSON(jobJSON(testWorkspaceID, testRunID, "seq", ""), runPodJSON(testRunID, "Pending", "True", "", "")),
		runsJSON(jobJSON(testWorkspaceID, testRunID, "seq", "Complete")),
	}
	code, _, stderr := rt.exec(t, "run", "--", "make", "test")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := strings.Join(rt.runner.actions(), ","); got != "apply,get,get,get" {
		t.Errorf("plane calls = %s, want no delete", got)
	}
}

func TestRunDetachRefusesUnschedulable(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	rt.runner.gets = [][]byte{runsJSON(jobJSON(testWorkspaceID, testRunID, "seq", ""), runPodJSON(testRunID, "Pending", "False", "Unschedulable", insufficientCPU))}
	code, stdout, stderr := rt.exec(t, "run", "--detach", "--cpu", "64", "--", "make", "test")
	if code == 0 || stdout != "" {
		t.Fatalf("exit %d stdout %q, want non-zero and no run id", code, stdout)
	}
	if !strings.Contains(stderr, "unsatisfied resources: "+insufficientCPU) {
		t.Errorf("stderr %q does not name the scheduler reason", stderr)
	}
	if got := strings.Join(rt.runner.actions(), ","); got != "apply,get,delete" {
		t.Errorf("plane calls = %s, want apply, get, delete", got)
	}
}

func TestRunDetachReturnsOnceScheduled(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	rt.runner.gets = [][]byte{
		runsJSON(jobJSON(testWorkspaceID, testRunID, "seq", ""), runPodJSON(testRunID, "Pending", "", "", "")),
		runsJSON(jobJSON(testWorkspaceID, testRunID, "seq", ""), runPodJSON(testRunID, "Pending", "True", "", "")),
	}
	code, stdout, stderr := rt.exec(t, "run", "--detach", "--", "make", "test")
	if code != 0 || stdout != testRunID+"\n" {
		t.Fatalf("exit %d stdout %q: %s", code, stdout, stderr)
	}
	if got := strings.Join(rt.runner.actions(), ","); got != "apply,get,get" {
		t.Errorf("plane calls = %s, want apply then two lists", got)
	}
}

func TestRunDetachCheckIsBounded(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	rt.runner.gets = [][]byte{runsJSON(jobJSON(testWorkspaceID, testRunID, "seq", ""), runPodJSON(testRunID, "Pending", "", "", ""))}
	code, stdout, stderr := rt.exec(t, "run", "--detach", "--", "make", "test")
	if code != 0 || stdout != testRunID+"\n" {
		t.Fatalf("exit %d stdout %q: %s", code, stdout, stderr)
	}
	if want := int(defaultScheduleCheck / runPollInterval); rt.sleeps != want {
		t.Errorf("slept %d times, want %d", rt.sleeps, want)
	}
	for _, a := range rt.runner.actions() {
		if a == "delete" {
			t.Fatal("a plain Pending run was deleted")
		}
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
	_, _, err := rt.renv.resolveCommand("make", executorTarget{fetches: true})
	if err == nil || !strings.Contains(err.Error(), "no artifact digest for linux/amd64") || !strings.Contains(err.Error(), "nothing ran") {
		t.Errorf("fetching executor: err %v", err)
	}
	rt.noSideEffects(t)
}

// darwinRelease is a macOS client's release: it proves only its own bytes.
func darwinRelease() (releaseIdentity, error) {
	return releaseIdentity{Version: "v1.2.3", SHA256: testExeSHA, Commit: "656d72b81d76e0c0ffee0123456789abcdef0123", GOOS: "darwin", GOARCH: "arm64"}, nil
}

// bashy itself is shipped, and on the Linux image executor a shipped
// command needs no client-provable linux artifact: the image carries it.
func TestRunShippedBashyFromNonLinuxClient(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	rt.renv.release = darwinRelease
	rt.renv.shippedArtifacts = provenShippedArtifacts // the production prover
	rt.renv.image = func() string { return "" }       // the release's own image
	code, stdout, stderr := rt.exec(t, "run", "--session", "--", "bashy", "-c", "echo hi; exit 7")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if strings.Contains(stderr, "artifact digest") {
		t.Errorf("image executor asked for an artifact digest: %s", stderr)
	}
	if stdout != "session "+testRunID+"\n" {
		t.Errorf("stdout %q, want the session id", stdout)
	}
	w := appliedWorkload(t, rt.runner.calls[0].stdin)
	if w["kind"] != "Pod" {
		t.Fatalf("workload kind %v, want Pod", w["kind"])
	}
	c := w["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if c["image"] != "localhost/bashy-scratch:656d72b8" {
		t.Errorf("image %v, want the client's own build", c["image"])
	}
	var argv []string
	for _, a := range c["command"].([]any) {
		argv = append(argv, a.(string))
	}
	if got := strings.Join(argv, " "); !strings.HasPrefix(got, "bashy dks session-server --session "+testRunID+" ") ||
		!strings.Contains(got, " -- bashy -c ") || !strings.Contains(got, "bashy -c 'echo hi; exit 7'") {
		t.Errorf("container command %q is not the session server over bashy -c", got)
	}
	// The identity — release and image — travels in the manifest annotation.
	ann := w["metadata"].(map[string]any)["annotations"].(map[string]any)
	m, err := dagrun.ParseManifest([]byte(ann[dagrun.AnnotManifest].(string)))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Meta.Artifact) != 0 {
		t.Errorf("image run recorded artifacts %v", m.Meta.Artifact)
	}
	text := string(m.Text)
	for _, want := range []string{"bashy release v1.2.3 656d72b81d76e0c0ffee0123456789abcdef0123", "image localhost/bashy-scratch:656d72b8"} {
		if !strings.Contains(text, want) {
			t.Errorf("manifest does not record %q:\n%s", want, text)
		}
	}
}

// The binary's own invoked name is shipped too, whatever the registry says.
func TestRunSelfNameIsShipped(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	rt.renv.selfName = func() string { return "bashy-dev" }
	for _, name := range []string{"bashy", "bashy-dev"} {
		if _, _, err := rt.renv.resolveCommand(name, executorTarget{image: testRunImage}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := rt.renv.resolveCommand("bashy-other", executorTarget{image: testRunImage}); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("unregistered name: err %v", err)
	}
}

// An executor that fetches the artifact (vk-native) still fails closed when
// the client cannot prove a linux digest.
func TestShippedFromNonLinuxClientStillFailsForFetchingExecutor(t *testing.T) {
	rt := newRunTest(t)
	rt.renv.release = darwinRelease
	rt.renv.shippedArtifacts = provenShippedArtifacts
	_, _, err := rt.renv.resolveCommand("bashy", executorTarget{fetches: true, image: testRunImage})
	if err == nil || !strings.Contains(err.Error(), "no artifact digest for linux/amd64 or linux/arm64") ||
		!strings.Contains(err.Error(), "darwin/arm64") {
		t.Errorf("err %v, want the fail-closed artifact error", err)
	}
	// A Linux client proves its own bytes for that executor.
	rt.renv.release = func() (releaseIdentity, error) {
		return releaseIdentity{Version: "v1.2.3", SHA256: testExeSHA, GOOS: "linux", GOARCH: "amd64"}, nil
	}
	ref, _, err := rt.renv.resolveCommand("bashy", executorTarget{fetches: true})
	if err != nil || ref.Artifacts["linux/amd64"] != testExeSHA {
		t.Errorf("linux client: ref %+v err %v", ref, err)
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
	want := "kubectl delete jobs,pods,services,deployments,networkpolicies,secrets -l " + dagrun.LabelManagedBy + "=bashy," + dagrun.LabelRun + "=" + testRunID + " --wait=false"
	if got != want {
		t.Errorf("stop argv = %q, want %q", got, want)
	}
	if !strings.Contains(stdout, testRunID) {
		t.Errorf("stdout %q", stdout)
	}
}

func TestStopDeletesCredentialSecret(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	code, stdout, stderr := rt.exec(t, "stop", testRunID)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if len(rt.runner.calls) != 1 {
		t.Fatalf("calls = %v", rt.runner.actions())
	}
	call := rt.runner.calls[0]
	foundSecrets := false
	for i, arg := range call.argv {
		if arg == "delete" && i+1 < len(call.argv) {
			kinds := strings.Split(call.argv[i+1], ",")
			for _, k := range kinds {
				if k == "secrets" || k == "secret" {
					foundSecrets = true
					break
				}
			}
		}
	}
	if !foundSecrets {
		t.Errorf("stop argv %q does not include secrets in delete kinds", call.argv)
	}
	if !strings.Contains(stdout, testRunID) {
		t.Errorf("stdout %q", stdout)
	}
}

func TestRunCredentialNeverInArgvManifestOrAnnotations(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel@k8s:dev")

	// Construct fake oat token dynamically at runtime (no literal real-looking tokens in source)
	fakeOAT := strings.Join([]string{"sk", "ant", "oat", "fake", "token", "0123456789abcdef"}, "-")
	rt.secrets["anthropic-subscription"] = []byte(fakeOAT)

	rt.runner.gets = [][]byte{
		runsJSON(jobJSON(testWorkspaceID, testRunID, "claude", "Complete")),
	}

	code, stdout, stderr := rt.exec(t, "run", "--credential", "anthropic-subscription=CLAUDE_CODE_OAUTH_TOKEN", "--", "claude", "-p", "generate report")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}

	if len(rt.runner.calls) < 2 {
		t.Fatalf("expected at least 2 plane calls (Secret apply, Job apply), got %d: %v", len(rt.runner.calls), rt.runner.actions())
	}

	secretCall := rt.runner.calls[0]
	jobCall := rt.runner.calls[1]

	// Secret call checks
	wantSecretName := "bashy-cred-" + strings.ToLower(testRunID)
	var secretObj map[string]any
	if err := json.Unmarshal(secretCall.stdin, &secretObj); err != nil {
		t.Fatalf("parse secret apply stdin: %v", err)
	}
	if secretObj["kind"] != "Secret" {
		t.Errorf("secret call kind = %v, want Secret", secretObj["kind"])
	}
	secMeta, _ := secretObj["metadata"].(map[string]any)
	if secMeta["name"] != wantSecretName {
		t.Errorf("secret name = %v, want %s", secMeta["name"], wantSecretName)
	}
	labels, _ := secMeta["labels"].(map[string]any)
	if labels[dagrun.LabelManagedBy] != "bashy" || labels[dagrun.LabelRun] != testRunID || labels[dagrun.LabelWorkspace] != testWorkspaceID {
		t.Errorf("secret labels incorrect: %v", labels)
	}
	stringData, _ := secretObj["stringData"].(map[string]any)
	if stringData["CLAUDE_CODE_OAUTH_TOKEN"] != fakeOAT {
		t.Errorf("stringData[CLAUDE_CODE_OAUTH_TOKEN] = %v, want %s", stringData["CLAUDE_CODE_OAUTH_TOKEN"], fakeOAT)
	}
	// Verify fakeOAT appears ONLY in stringData in the Secret payload
	secretJSONWithoutStringData := strings.Replace(string(secretCall.stdin), fakeOAT, "", 1)
	if strings.Contains(secretJSONWithoutStringData, fakeOAT) {
		t.Fatal("CRITICAL: fakeOAT leaked into other parts of Secret payload")
	}

	// Workload Job checks
	var jobList map[string]any
	if err := json.Unmarshal(jobCall.stdin, &jobList); err != nil {
		t.Fatalf("parse job apply stdin: %v", err)
	}
	items, _ := jobList["items"].([]any)
	if len(items) == 0 {
		t.Fatal("workload items empty")
	}
	jobObj, _ := items[0].(map[string]any)
	jobSpec, _ := jobObj["spec"].(map[string]any)
	tmpl, _ := jobSpec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	containers, _ := podSpec["containers"].([]any)
	container, _ := containers[0].(map[string]any)
	envList, _ := container["env"].([]any)

	var foundEnv map[string]any
	for _, e := range envList {
		entry, _ := e.(map[string]any)
		if entry["name"] == "CLAUDE_CODE_OAUTH_TOKEN" {
			foundEnv = entry
			break
		}
	}
	if foundEnv == nil {
		t.Fatalf("workload container env missing CLAUDE_CODE_OAUTH_TOKEN: %v", envList)
	}
	vf, _ := foundEnv["valueFrom"].(map[string]any)
	skr, _ := vf["secretKeyRef"].(map[string]any)
	if skr["name"] != wantSecretName {
		t.Errorf("secretKeyRef name = %v, want %s", skr["name"], wantSecretName)
	}
	if skr["key"] != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Errorf("secretKeyRef key = %v, want CLAUDE_CODE_OAUTH_TOKEN", skr["key"])
	}

	// Workload must NOT contain secret value bytes anywhere in its JSON payload
	if bytes.Contains(jobCall.stdin, []byte(fakeOAT)) {
		t.Fatal("CRITICAL: secret value leaked into workload payload")
	}

	// Workload annotations/manifest must record only the vault name and env name, never the value
	jobMeta, _ := jobObj["metadata"].(map[string]any)
	annots, _ := jobMeta["annotations"].(map[string]any)
	manifestText, _ := annots[dagrun.AnnotManifest].(string)
	if strings.Contains(manifestText, fakeOAT) {
		t.Fatal("CRITICAL: secret value leaked into manifest annotation")
	}
	if !strings.Contains(manifestText, "Secrets: anthropic-subscription=CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("manifest annotation missing Secrets reference line: %s", manifestText)
	}

	// Argv of all calls must contain no secret value bytes
	for i, c := range rt.runner.calls {
		fullArgv := strings.Join(c.argv, " ")
		if strings.Contains(fullArgv, fakeOAT) {
			t.Fatalf("CRITICAL: call %d argv contains secret value: %s", i, fullArgv)
		}
	}

	// Stdout and stderr must contain no secret value bytes
	if strings.Contains(stdout, fakeOAT) {
		t.Fatal("CRITICAL: stdout contains secret value")
	}
	if strings.Contains(stderr, fakeOAT) {
		t.Fatal("CRITICAL: stderr contains secret value")
	}
}

func TestRunRefusesMeteredKeyForSubscription(t *testing.T) {
	// Case 1: vault name contains "subscription"
	{
		rt := newRunTest(t)
		rt.selectWorkspace(t, "skel")
		meteredKey := strings.Join([]string{"sk", "ant", "api03", "sample", "metered", "key"}, "-")
		rt.secrets["anthropic-subscription"] = []byte(meteredKey)

		code, _, stderr := rt.exec(t, "run", "--credential", "anthropic-subscription", "--", "claude", "-p", "report")
		if code != 1 {
			t.Errorf("exit code = %d, want 1", code)
		}
		wantMsg := "metered API key refused for a subscription credential — nothing ran"
		if !strings.Contains(stderr, wantMsg) {
			t.Errorf("stderr %q does not contain %q", stderr, wantMsg)
		}
		if len(rt.runner.calls) != 0 {
			t.Errorf("plane calls made: %v, want 0", rt.runner.actions())
		}
	}

	// Case 2: ENV_NAME is CLAUDE_CODE_OAUTH_TOKEN
	{
		rt := newRunTest(t)
		rt.selectWorkspace(t, "skel")
		meteredKey := strings.Join([]string{"sk", "ant", "api03", "other", "metered", "key"}, "-")
		rt.secrets["my-custom-vault"] = []byte(meteredKey)

		code, _, stderr := rt.exec(t, "run", "--credential", "my-custom-vault=CLAUDE_CODE_OAUTH_TOKEN", "--", "claude", "-p", "report")
		if code != 1 {
			t.Errorf("exit code = %d, want 1", code)
		}
		wantMsg := "metered API key refused for a subscription credential — nothing ran"
		if !strings.Contains(stderr, wantMsg) {
			t.Errorf("stderr %q does not contain %q", stderr, wantMsg)
		}
		if len(rt.runner.calls) != 0 {
			t.Errorf("plane calls made: %v, want 0", rt.runner.actions())
		}
	}
}

func TestRunRedactsDeliveredSecretFromError(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")

	secretToken := strings.Join([]string{"my", "super", "secret", "token", "val"}, "-")
	rt.secrets["my-vault"] = []byte(secretToken)

	// Inject runner error on apply that echoes the secret
	rt.runner.apply.exit = 1
	rt.runner.apply.stderr = fmt.Sprintf("plane error with leaked %s token", secretToken)

	code, _, stderr := rt.exec(t, "run", "--credential", "my-vault=MY_TOKEN", "--", "echo", "hello")
	if code == 0 {
		t.Fatal("expected non-zero exit")
	}
	if strings.Contains(stderr, secretToken) {
		t.Fatalf("CRITICAL: stderr leaked secret: %s", stderr)
	}
	if !strings.Contains(stderr, "[redacted]") {
		t.Errorf("stderr does not contain [redacted]: %s", stderr)
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
	if f := strings.Fields(lines[0]); strings.Join(f, " ") != "RUN KIND COMMAND PHASE AGE EXECUTOR" {
		t.Errorf("header %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], older) || !strings.HasPrefix(lines[2], testRunID) {
		t.Errorf("rows not in run-id order:\n%s", stdout)
	}
	if strings.Contains(stdout, "leak") || strings.Contains(stdout, otherWorkspace) {
		t.Errorf("ps shows another workspace's run:\n%s", stdout)
	}
	if f := strings.Fields(lines[2]); len(f) != 6 || f[1] != "one-shot" || f[3] != "succeeded" || f[5] != "Job/run-"+strings.ToLower(testRunID) {
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

// fakeBuildRelease is the release identity a binary built with bi reports.
func fakeBuildRelease(bi *debug.BuildInfo) func() (releaseIdentity, error) {
	return func() (releaseIdentity, error) {
		return releaseIdentity{Version: bi.Main.Version, SHA256: testExeSHA, Commit: commitFromBuildInfo(bi)}, nil
	}
}

func vcsBuild(rev string) *debug.BuildInfo {
	return &debug.BuildInfo{Main: debug.Module{Path: "github.com/qiangli/bashy", Version: "(devel)"},
		Settings: []debug.BuildSetting{{Key: "vcs", Value: "git"}, {Key: "vcs.revision", Value: rev}}}
}

func TestRunDefaultImageIsOwnBashyCommit(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	rt.renv.release = fakeBuildRelease(vcsBuild("656d72b81d76e0c0ffee0123456789abcdef0123"))
	rt.renv.image = func() string { return "" } // no $BASHY_DKS_IMAGE
	code, stdout, stderr := rt.exec(t, "run", "--emit", "yaml", "--", "make")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, `"image": "localhost/bashy-scratch:656d72b8"`) {
		t.Errorf("rendered objects do not run the own-commit image:\n%s", stdout)
	}
	if !strings.Contains(stderr, "localhost/bashy-scratch:656d72b8") || !strings.Contains(stderr, "release") {
		t.Errorf("stderr %q does not name the resolved image and its source", stderr)
	}
	rt.noSideEffects(t)
}

func TestRunImagePrecedenceFlagEnvRelease(t *testing.T) {
	const own = "localhost/bashy-scratch:656d72b8"
	for _, tc := range []struct {
		name, flag, env, want, from string
	}{
		{"flag wins", "example.invalid/flag:1", "example.invalid/env:1", "example.invalid/flag:1", "flag"},
		{"env over release", "", "example.invalid/env:1", "example.invalid/env:1", "env"},
		{"release last", "", "", own, "release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := newRunTest(t)
			rt.selectWorkspace(t, "skel")
			rt.renv.release = fakeBuildRelease(vcsBuild("656d72b81d76e0c0ffee0123456789abcdef0123"))
			rt.renv.image = func() string { return tc.env }
			args := []string{"run", "--emit", "yaml"}
			if tc.flag != "" {
				args = append(args, "--image", tc.flag)
			}
			code, stdout, stderr := rt.exec(t, append(args, "--", "make")...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if !strings.Contains(stdout, `"image": "`+tc.want+`"`) {
				t.Errorf("image is not %q:\n%s", tc.want, stdout)
			}
			if !strings.Contains(stderr, tc.want+" (from "+tc.from+")") {
				t.Errorf("stderr %q does not name %q from %s", stderr, tc.want, tc.from)
			}
			rt.noSideEffects(t)
		})
	}
}

func TestRunVerboseNamesImageSource(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel@k8s:dev")
	rt.renv.release = fakeBuildRelease(vcsBuild("656d72b81d76e0c0ffee0123456789abcdef0123"))
	rt.renv.image = func() string { return "" }
	_, _, stderr := rt.exec(t, "run", "--verbose", "--detach", "--", "make")
	if !strings.Contains(stderr, "localhost/bashy-scratch:656d72b8 (from release)") {
		t.Errorf("--verbose stderr %q does not name the image source", stderr)
	}
	_, _, quiet := rt.exec(t, "run", "--detach", "--", "make")
	if strings.Contains(quiet, "executor image") {
		t.Errorf("non-verbose run is chatty: %q", quiet)
	}
}

func TestRunNoCommitNoImageFailsClosed(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel@k8s:dev")
	rt.renv.release = fakeBuildRelease(&debug.BuildInfo{Main: debug.Module{Version: "(devel)"}})
	rt.renv.image = func() string { return "" }
	for _, args := range [][]string{
		{"run", "--", "make"},
		{"run", "--emit", "yaml", "--", "make"},
	} {
		code, stdout, stderr := rt.exec(t, args...)
		if code == 0 {
			t.Errorf("%q exited 0 with no resolvable image", args)
		}
		if !strings.Contains(stderr, "--image") || !strings.Contains(stderr, "$"+imageEnvVar) {
			t.Errorf("%q stderr %q is not actionable", args, stderr)
		}
		if stdout != "" {
			t.Errorf("%q printed %q", args, stdout)
		}
	}
	rt.noSideEffects(t)
}

func TestCommitFromPseudoVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		bi   *debug.BuildInfo
		want string
	}{
		{"vcs.revision", vcsBuild("656d72b81d76e0c0ffee0123456789abcdef0123"), "656d72b81d76e0c0ffee0123456789abcdef0123"},
		{"pseudo", &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261020101500-656d72b81d76"}}, "656d72b81d76"},
		{"pseudo pre", &debug.BuildInfo{Main: debug.Module{Version: "v1.2.4-0.20261020101500-656d72b81d76"}}, "656d72b81d76"},
		{"pseudo dirty", &debug.BuildInfo{Main: debug.Module{Version: "v1.2.4-0.20261020101500-656d72b81d76+dirty"}}, "656d72b81d76"},
		{"release tag", &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}, ""},
		{"devel", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, ""},
		{"nil", nil, ""},
	} {
		if got := commitFromBuildInfo(tc.bi); got != tc.want {
			t.Errorf("%s: commit %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := defaultImage(releaseIdentity{Commit: "656d72b81d76"}); got != "localhost/bashy-scratch:656d72b8" {
		t.Errorf("defaultImage = %q", got)
	}
	if got := defaultImage(releaseIdentity{}); got != "" {
		t.Errorf("defaultImage without commit = %q", got)
	}
}

// appliedItems decodes the v1 List one apply sent.
func appliedItems(t *testing.T, payload []byte) []map[string]any {
	t.Helper()
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(payload, &list); err != nil {
		t.Fatalf("applied payload: %v", err)
	}
	return list.Items
}

// appliedWorkload is the one Job or Pod of an apply.
func appliedWorkload(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	for _, o := range appliedItems(t, payload) {
		if k := o["kind"]; k == "Job" || k == "Pod" {
			return o
		}
	}
	t.Fatalf("no workload in %s", payload)
	return nil
}

func TestRunSessionRendersSessionServerPod(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	rt.renv.isShipped = func(name string) bool { return name == "sh" }
	code, stdout, stderr := rt.exec(t, "run", "--session", "--", "sh", "-c", "echo hi; exit 7")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := strings.Join(rt.runner.actions(), ","); got != "apply" {
		t.Fatalf("plane calls = %s, want apply only (a session never polls)", got)
	}
	if rt.sleeps != 0 {
		t.Errorf("slept %d times; a session returns at once", rt.sleeps)
	}
	if stdout != "session "+testRunID+"\n" {
		t.Errorf("stdout %q, want the session id", stdout)
	}
	w := appliedWorkload(t, rt.runner.calls[0].stdin)
	if w["kind"] != "Pod" {
		t.Fatalf("workload kind %v, want Pod", w["kind"])
	}
	md := w["metadata"].(map[string]any)
	port, _ := md["annotations"].(map[string]any)[dagrun.AnnotAttachPort].(string)
	if port == "" {
		t.Fatalf("no %s annotation: %v", dagrun.AnnotAttachPort, md["annotations"])
	}
	c := w["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	var argv []string
	for _, a := range c["command"].([]any) {
		argv = append(argv, a.(string))
	}
	got := strings.Join(argv, " ")
	if !strings.HasPrefix(got, "bashy dks session-server --session "+testRunID+" ") ||
		!strings.Contains(got, "--port "+port+" ") || !strings.Contains(got, " -- bashy -c ") ||
		!strings.Contains(got, "sh -c") {
		t.Errorf("container command %q is not the session server over the argv", got)
	}
	ports := c["ports"].([]any)[0].(map[string]any)
	if fmt.Sprint(ports["hostPort"]) != port {
		t.Errorf("hostPort %v, want %s", ports["hostPort"], port)
	}
	if _, tty := c["tty"]; tty {
		t.Error("a non-terminal session rendered a tty (want long-running)")
	}
}

func TestRunSessionTTYIsInteractive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		stdin bool
	}{
		{"flag", []string{"run", "--session", "--tty", "--", "sh"}, false},
		{"terminal", []string{"run", "--session", "--", "sh"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := newRunTest(t)
			rt.selectWorkspace(t, "skel")
			rt.renv.isShipped = func(name string) bool { return name == "sh" }
			rt.renv.stdinIsTerminal = func() bool { return tc.stdin }
			code, _, stderr := rt.exec(t, tc.args...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			c := appliedWorkload(t, rt.runner.calls[0].stdin)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			if c["tty"] != true || c["stdin"] != true {
				t.Errorf("container %v is not interactive", c)
			}
		})
	}
}

func TestRunRegisteredRecordKindWins(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	rt.registry["agentd"] = fleet.Command{Name: "agentd", Kind: fleet.KindCommand, Script: "serve"}
	rt.renv.recordRunKind = func(rec fleet.Command) dagrun.RunKind {
		if rec.Name == "agentd" {
			return dagrun.RunLongRunning
		}
		return ""
	}
	code, stdout, stderr := rt.exec(t, "run", "--", "agentd")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := strings.Join(rt.runner.actions(), ","); got != "apply" {
		t.Errorf("plane calls = %s, want apply only", got)
	}
	if stdout != "session "+testRunID+"\n" {
		t.Errorf("stdout %q, want the session id", stdout)
	}
	if k := appliedWorkload(t, rt.runner.calls[0].stdin)["kind"]; k != "Pod" {
		t.Errorf("workload %v, want the record's long-running Pod", k)
	}

	// A shipped command defaults to one-shot: a Job, waited on.
	rt2 := newRunTest(t)
	rt2.selectWorkspace(t, "skel")
	rt2.runner.gets = [][]byte{runsJSON(jobJSON(testWorkspaceID, testRunID, "make", "Complete"))}
	if code, _, stderr := rt2.exec(t, "run", "--", "make"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if k := appliedWorkload(t, rt2.runner.calls[0].stdin)["kind"]; k != "Job" {
		t.Errorf("shipped workload %v, want Job", k)
	}
}

func TestRunSessionRefusesServiceKind(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	rt.registry["web"] = fleet.Command{Name: "web", Kind: fleet.KindCommand, Script: "serve"}
	rt.renv.recordRunKind = func(fleet.Command) dagrun.RunKind { return dagrun.RunService }
	code, stdout, stderr := rt.exec(t, "run", "--session", "--", "web")
	if code == 0 || !strings.Contains(stderr, "service") || !strings.Contains(stderr, "nothing ran") {
		t.Errorf("exit %d stderr %q, want a refusal naming run.kind service", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout %q", stdout)
	}
	rt.noSideEffects(t)
}

// podJSON is a session Pod as the plane reports it: labels plus the
// manifest annotation render wrote.
func podJSON(t *testing.T, ws, run, command string, kind dagrun.RunKind, phase string) string {
	t.Helper()
	manifest, err := dagrun.Generate(dagrun.GenerateInput{
		Command:     dagrun.CommandRef{Name: command, Digest: testExeSHA, Artifacts: map[string]string{"linux/amd64": testAmdSHA}},
		Argv:        []string{command},
		WorkspaceID: ws,
		RunID:       run,
		Cwd:         "~/docs",
		Venue:       dag.VenueCluster,
		Run:         dagrun.RunSpec{Kind: kind},
	})
	if err != nil {
		t.Fatal(err)
	}
	ann, _ := json.Marshal(map[string]string{dagrun.AnnotManifest: string(manifest)})
	return fmt.Sprintf(`{"kind":"Pod","metadata":{"name":"bashy-run-%s","labels":{%q:"bashy",%q:%q,%q:%q,%q:%q},"annotations":%s},"status":{"phase":%q}}`,
		strings.ToLower(run), dagrun.LabelManagedBy, dagrun.LabelWorkspace, ws, dagrun.LabelRun, run, dagrun.LabelCommand, command, ann, phase)
}

func TestPsShowsKind(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	const (
		shellRun = "01JC1ZK3V0000000000000000A"
		agentRun = "01JC1ZK3V0000000000000000B"
	)
	answer := runsJSON(
		jobJSON(testWorkspaceID, testRunID, "make", "Complete"),
		podJSON(t, testWorkspaceID, shellRun, "bashy", dagrun.RunInteractive, "Running"),
		podJSON(t, testWorkspaceID, agentRun, "agentd", dagrun.RunLongRunning, "Running"),
	)
	rt.runner.gets = [][]byte{answer}
	code, stdout, stderr := rt.exec(t, "ps")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 4 {
		t.Fatalf("ps output:\n%s", stdout)
	}
	if f := strings.Join(strings.Fields(lines[0]), " "); f != "RUN KIND COMMAND PHASE AGE EXECUTOR" {
		t.Errorf("header %q", lines[0])
	}
	want := map[string]string{shellRun: "interactive", agentRun: "long-running", testRunID: "one-shot"}
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) != 6 || want[f[0]] != f[1] {
			t.Errorf("row %q, want kind %s", l, want[f[0]])
		}
	}
}
