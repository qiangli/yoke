package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dag"
	"github.com/qiangli/yoke/pkg/dagrun"
)

const (
	testWorkspace = "ws-01jb6zexample0000000000"
	testRunID     = "01JB7CEXAMPLE0000000000000"
	testImage     = "registry.example/bashy/bashy:v0.0.0-test"
	testRecord    = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testDigest    = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// call is one recorded Runner invocation.
type call struct {
	argv  []string
	stdin []byte
}

// fakeRunner records every call and answers from a script; it never execs.
type fakeRunner struct {
	calls  []call
	stdout []byte
	stderr []byte
	exit   int
	err    error
}

func (f *fakeRunner) Run(_ context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error) {
	f.calls = append(f.calls, call{argv: append([]string(nil), argv...), stdin: append([]byte(nil), stdin...)})
	return f.stdout, f.stderr, f.exit, f.err
}

func linuxClass() dagrun.ExecutorClass { return dagrun.ExecutorClass{OS: "linux", Image: testImage} }

func testManifest(t *testing.T) []byte {
	t.Helper()
	m, err := dagrun.Generate(dagrun.GenerateInput{
		Command:     dagrun.CommandRef{Name: "render", Digest: testRecord, Artifacts: map[string]string{"linux/amd64": testDigest}},
		Argv:        []string{"render", "report.md"},
		WorkspaceID: testWorkspace,
		RunID:       testRunID,
		Cwd:         "~/docs",
		Venue:       dag.VenueCluster,
		Match:       map[string]string{"kubernetes.io/os": "linux"},
		Run:         dagrun.RunSpec{Kind: dagrun.RunOneShot, Egress: dagrun.Egress{Mode: dagrun.EgressNone}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return m
}

func runTask(t *testing.T, manifest []byte) *dag.Task {
	t.Helper()
	m, err := dagrun.ParseManifest(manifest)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	return m.Task
}

func TestClusterStopSelectsByRunLabelOnly(t *testing.T) {
	r := &fakeRunner{}
	c := &Client{Runner: r}
	if err := c.Stop(context.Background(), testRunID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("Stop issued %d calls, want exactly 1: %v", len(r.calls), r.calls)
	}
	argv := r.calls[0].argv
	want := []string{"kubectl", "delete", stopKinds,
		"-l", dagrun.LabelManagedBy + "=" + dagrun.ManagedByBashy + "," + dagrun.LabelRun + "=" + testRunID,
		"--wait=false"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("Stop argv\n got %q\nwant %q", argv, want)
	}
	// Selection is by label only: no object name may appear anywhere.
	for _, a := range argv {
		if strings.Contains(a, "bashy-run-") || strings.Contains(a, "/") && !strings.Contains(a, "=") {
			t.Fatalf("Stop argv names an object: %q", argv)
		}
	}
	if len(r.calls[0].stdin) != 0 {
		t.Fatalf("Stop wrote stdin: %q", r.calls[0].stdin)
	}
}

func TestClusterStopPassesContextAndRejectsUnsafeID(t *testing.T) {
	r := &fakeRunner{}
	c := &Client{Runner: r, Context: "plane-a"}
	if err := c.Stop(context.Background(), testRunID); err != nil {
		t.Fatal(err)
	}
	if got := r.calls[0].argv[:3]; !reflect.DeepEqual(got, []string{"kubectl", "--context", "plane-a"}) {
		t.Fatalf("context not passed: %q", r.calls[0].argv)
	}
	for _, bad := range []string{"", "a,dhnt.io/workspace=x", "run id", "-x", strings.Repeat("a", 64), "a!=b"} {
		if err := c.Stop(context.Background(), bad); !errors.Is(err, ErrInvalidSelector) {
			t.Fatalf("Stop(%q) = %v, want ErrInvalidSelector", bad, err)
		}
	}
	if len(r.calls) != 1 {
		t.Fatalf("an unsafe id reached the runner: %v", r.calls[1:])
	}
}

func TestClusterApplyIsServerSideAndDeterministic(t *testing.T) {
	manifest := testManifest(t)
	var stdins [][]byte
	for i := 0; i < 3; i++ {
		r := &fakeRunner{}
		c := &Client{Runner: r}
		applied, err := c.Apply(context.Background(), manifest, linuxClass())
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(r.calls) != 1 {
			t.Fatalf("Apply issued %d calls, want 1", len(r.calls))
		}
		want := []string{"kubectl", "apply", "--server-side", "--field-manager=bashy", "-f", "-"}
		if !reflect.DeepEqual(r.calls[0].argv, want) {
			t.Fatalf("Apply argv\n got %q\nwant %q", r.calls[0].argv, want)
		}
		if applied.RunID != testRunID || applied.Workspace != testWorkspace {
			t.Fatalf("Applied ids = %+v", applied)
		}
		wantRefs := []ObjectRef{
			{APIVersion: "batch/v1", Kind: "Job", Name: "bashy-run-" + strings.ToLower(testRunID)},
			{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy", Name: "bashy-run-" + strings.ToLower(testRunID)},
		}
		if !reflect.DeepEqual(applied.Objects, wantRefs) {
			t.Fatalf("Applied.Objects = %+v", applied.Objects)
		}
		stdins = append(stdins, r.calls[0].stdin)
	}
	for _, s := range stdins[1:] {
		if !bytes.Equal(s, stdins[0]) {
			t.Fatal("apply payload is not byte-identical across calls")
		}
	}

	// The payload is a v1 List of exactly what Render returns.
	var list struct {
		APIVersion string           `json:"apiVersion"`
		Kind       string           `json:"kind"`
		Items      []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(stdins[0], &list); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if list.APIVersion != "v1" || list.Kind != "List" {
		t.Fatalf("payload is %s/%s, want v1/List", list.APIVersion, list.Kind)
	}
	objs, err := dagrun.Render(manifest, linuxClass())
	if err != nil {
		t.Fatal(err)
	}
	wantItems, _ := json.Marshal(objs)
	gotItems, _ := json.Marshal(list.Items)
	if !bytes.Equal(gotItems, wantItems) {
		t.Fatalf("payload items differ from Render:\n got %s\nwant %s", gotItems, wantItems)
	}
}

func TestClusterApplyUnsupportedClassNeverRuns(t *testing.T) {
	r := &fakeRunner{}
	c := &Client{Runner: r}
	_, err := c.Apply(context.Background(), testManifest(t), dagrun.ExecutorClass{OS: "darwin"})
	var u *UnsatisfiedError
	if !errors.As(err, &u) || !errors.Is(err, dagrun.ErrUnsupportedExecutor) {
		t.Fatalf("Apply(darwin) = %v, want UnsatisfiedError wrapping ErrUnsupportedExecutor", err)
	}
	if !strings.Contains(u.Capability, "darwin") {
		t.Fatalf("capability %q does not name the executor", u.Capability)
	}
	if errors.Is(err, dag.ErrWorkerUnreachable) {
		t.Fatal("an unsupported class must not read as unreachable (it is not retryable)")
	}
	if len(r.calls) != 0 {
		t.Fatalf("an unsupported class reached the runner: %v", r.calls)
	}
}

func TestClusterApplyRejectedByPlaneIsNotUnreachable(t *testing.T) {
	r := &fakeRunner{exit: 1, stderr: []byte(`Error from server (Forbidden): jobs.batch is forbidden`)}
	_, err := (&Client{Runner: r}).Apply(context.Background(), testManifest(t), linuxClass())
	if err == nil || errors.Is(err, dag.ErrWorkerUnreachable) {
		t.Fatalf("plane rejection = %v, want a non-unreachable error", err)
	}
	if !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("error drops kubectl's reason: %v", err)
	}
}

func TestClusterExecUnreachableWrapsErrWorkerUnreachable(t *testing.T) {
	manifest := testManifest(t)
	cases := map[string]*fakeRunner{
		"runner error":   {err: errors.New("exec: kubectl: executable file not found"), exit: -1},
		"no connection":  {exit: 1, stderr: []byte("Unable to connect to the server: dial tcp: connection refused\n")},
		"refused direct": {exit: 1, stderr: []byte("The connection to the server was refused - did you specify the right host or port?\n")},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			x := &Transport{Client: &Client{Runner: r}, Class: linuxClass(), Manifest: manifest}
			task := runTask(t, manifest)
			w := &dag.Worker{ID: "cluster-a", Venues: []string{dag.VenueCluster}}
			res := x.Exec(context.Background(), w, task, dag.TaskIO{})
			if res.Status != dag.StatusFailed {
				t.Fatalf("status = %v, want failed", res.Status)
			}
			if !errors.Is(res.Err, dag.ErrWorkerUnreachable) {
				t.Fatalf("Err = %v, want wrapping dag.ErrWorkerUnreachable", res.Err)
			}
			rec := dag.RecordAttempt(task, w, 1, res)
			if rec.Status != dag.RunInfraFailed || rec.Failure == nil || rec.Failure.Code != dag.FailUnreachable {
				t.Fatalf("record = %+v %+v, want infra-failed/worker-unreachable", rec.Status, rec.Failure)
			}
		})
	}
}

func TestClusterExecUnsupportedClassIsNoEligibleExecutor(t *testing.T) {
	manifest := testManifest(t)
	r := &fakeRunner{}
	x := &Transport{Client: &Client{Runner: r}, Class: dagrun.ExecutorClass{OS: "windows"}, Manifest: manifest}
	task := runTask(t, manifest)
	res := x.Exec(context.Background(), nil, task, dag.TaskIO{})
	var u *UnsatisfiedError
	if res.Status != dag.StatusFailed || !errors.As(res.Err, &u) {
		t.Fatalf("result = %v %v, want failed UnsatisfiedError", res.Status, res.Err)
	}
	if errors.Is(res.Err, dag.ErrWorkerUnreachable) {
		t.Fatal("unsupported class must not be retryable as unreachable")
	}
	rec := dag.RecordAttempt(task, nil, 1, res)
	if rec.Status != dag.RunInfraFailed || rec.Failure.Code != dag.FailNoWorker {
		t.Fatalf("record = %v %+v, want infra-failed/no-eligible-worker", rec.Status, rec.Failure)
	}
	if len(r.calls) != 0 {
		t.Fatalf("runner called: %v", r.calls)
	}
}

func TestClusterExecAppliesAndReportsRun(t *testing.T) {
	manifest := testManifest(t)
	r := &fakeRunner{}
	x := &Transport{Client: &Client{Runner: r}, Class: linuxClass(), Manifest: manifest}
	var out bytes.Buffer
	res := x.Exec(context.Background(), nil, runTask(t, manifest), dag.TaskIO{Stdout: &out})
	if res.Status != dag.StatusDone || res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
	if res.Name != dagrun.TargetName {
		t.Fatalf("result name = %q", res.Name)
	}
	if len(r.calls) != 1 || r.calls[0].argv[1] != "apply" {
		t.Fatalf("calls = %v", r.calls)
	}
	if !strings.Contains(out.String(), testRunID) {
		t.Fatalf("stdout does not report the run id: %q", out.String())
	}
}

func TestClusterExecRefusesForeignTask(t *testing.T) {
	manifest := testManifest(t)
	r := &fakeRunner{}
	x := &Transport{Client: &Client{Runner: r}, Class: linuxClass(), Manifest: manifest}
	other := &dag.Task{Name: "build", Body: "make\n", Venue: dag.VenueCluster}
	res := x.Exec(context.Background(), nil, other, dag.TaskIO{})
	if res.Status != dag.StatusFailed || res.Err == nil {
		t.Fatalf("foreign task result = %+v", res)
	}
	if len(r.calls) != 0 {
		t.Fatalf("a task that is not the manifest's run target was applied: %v", r.calls)
	}
	if err := x.Close(); err != nil {
		t.Fatal(err)
	}
	if err := x.Close(); err != nil {
		t.Fatal("Close is not idempotent")
	}
	res = x.Exec(context.Background(), nil, runTask(t, manifest), dag.TaskIO{})
	if !errors.Is(res.Err, dag.ErrWorkerUnreachable) || len(r.calls) != 0 {
		t.Fatalf("closed transport = %v, calls %v", res.Err, r.calls)
	}
}

func TestClusterListParsesStatusByLabel(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "get-jobs-pods.json"))
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{stdout: fixture}
	got, err := (&Client{Runner: r, Context: "plane-a"}).List(context.Background(), testWorkspace)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantArgv := []string{"kubectl", "--context", "plane-a", "get", "jobs,pods",
		"-l", dagrun.LabelManagedBy + "=" + dagrun.ManagedByBashy + "," + dagrun.LabelWorkspace + "=" + testWorkspace,
		"-o", "json"}
	if len(r.calls) != 1 || !reflect.DeepEqual(r.calls[0].argv, wantArgv) {
		t.Fatalf("List argv\n got %q\nwant %q", r.calls[0].argv, wantArgv)
	}
	want := []RunStatus{
		{RunID: "RUNCOMPLETE", Workspace: testWorkspace, Command: "render", Kind: "Job", Name: "bashy-run-runcomplete", Phase: PhaseSucceeded, Scheduled: true},
		{RunID: "RUNFAILED", Workspace: testWorkspace, Command: "render", Kind: "Job", Name: "bashy-run-runfailed", Phase: PhaseFailed},
		{RunID: "RUNJOBACTIVE", Workspace: testWorkspace, Command: "render", Kind: "Job", Name: "bashy-run-runjobactive", Phase: PhaseRunning, Scheduled: true},
		{RunID: "RUNJOBNEW", Workspace: testWorkspace, Command: "render", Kind: "Job", Name: "bashy-run-runjobnew", Phase: PhasePending},
		{RunID: "RUNPODPENDING", Workspace: testWorkspace, Command: "agent", Kind: "Pod", Name: "bashy-run-runpodpending", Phase: PhasePending},
		{RunID: "RUNSESSION", Workspace: testWorkspace, Command: "bashy", Kind: "Pod", Name: "bashy-run-runsession", Phase: PhaseRunning, Scheduled: true},
	}
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.MarshalIndent(got, "", " ")
		t.Fatalf("List =\n%s", gb)
	}
}

func TestClusterListSurfacesUnschedulablePod(t *testing.T) {
	labels := `{"app.kubernetes.io/managed-by":"bashy","dhnt.io/workspace":"` + testWorkspace + `","dhnt.io/run":"%s","dhnt.io/command":"seq"}`
	out := fmt.Sprintf(`{"items":[
	 {"kind":"Job","metadata":{"name":"bashy-run-big","labels":`+labels+`},"status":{"active":1}},
	 {"kind":"Pod","metadata":{"name":"bashy-run-big-abcde","labels":`+labels+`},"status":{"phase":"Pending","conditions":[
	  {"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"0/5 nodes are available: 5 Insufficient cpu."}]}},
	 {"kind":"Pod","metadata":{"name":"bashy-run-pull","labels":`+labels+`},"status":{"phase":"Pending","conditions":[
	  {"type":"PodScheduled","status":"True"}]}},
	 {"kind":"Pod","metadata":{"name":"bashy-run-fresh","labels":`+labels+`},"status":{"phase":"Pending"}}
	]}`, "BIG", "BIG", "PULL", "FRESH")
	got, err := (&Client{Runner: &fakeRunner{stdout: []byte(out)}}).List(context.Background(), testWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]RunStatus{
		"BIG":   {Phase: PhaseRunning, Unschedulable: "0/5 nodes are available: 5 Insufficient cpu."},
		"PULL":  {Phase: PhasePending, Scheduled: true},
		"FRESH": {Phase: PhasePending},
	}
	if len(got) != len(want) {
		t.Fatalf("List = %+v", got)
	}
	for _, r := range got {
		w := want[r.RunID]
		if r.Phase != w.Phase || r.Scheduled != w.Scheduled || r.Unschedulable != w.Unschedulable {
			t.Errorf("%s = phase %s scheduled %v unschedulable %q, want %+v", r.RunID, r.Phase, r.Scheduled, r.Unschedulable, w)
		}
	}
}

func TestClusterListErrors(t *testing.T) {
	c := &Client{Runner: &fakeRunner{}}
	if _, err := c.List(context.Background(), "ws,x"); !errors.Is(err, ErrInvalidSelector) {
		t.Fatalf("unsafe workspace = %v", err)
	}
	c = &Client{Runner: &fakeRunner{stdout: []byte("not json")}}
	if _, err := c.List(context.Background(), testWorkspace); err == nil {
		t.Fatal("garbage output parsed")
	}
	c = &Client{Runner: &fakeRunner{exit: 1, stderr: []byte("Unable to connect to the server: i/o timeout")}}
	if _, err := c.List(context.Background(), testWorkspace); !errors.Is(err, dag.ErrWorkerUnreachable) {
		t.Fatalf("unreachable List = %v", err)
	}
	c = &Client{Runner: &fakeRunner{stdout: []byte(`{"apiVersion":"v1","kind":"List","items":[]}`)}}
	if got, err := c.List(context.Background(), testWorkspace); err != nil || len(got) != 0 {
		t.Fatalf("empty List = %v, %v", got, err)
	}
}

func TestClusterRunnerHonorsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	r := &fakeRunner{}
	if err := (&Client{Runner: r}).Stop(ctx, testRunID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop on a done context = %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatal("runner called on a done context")
	}
}
