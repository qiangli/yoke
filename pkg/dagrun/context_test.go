package dagrun_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/dagrun"
)

// ctxSafeEnv isolates the test from any real HOME, BASHY_HOME or ~/.bashy and
// returns a per-client state directory under the sandbox.
func ctxSafeEnv(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HOME", base)
	t.Setenv("BASHY_HOME", filepath.Join(base, ".bashy"))
	return filepath.Join(base, "state", "dks", "client-a")
}

const (
	ctxULID      = "01JBQ5E9P2WYZ5ZG4HJ3XRG7W8"
	ctxULID2     = "01HGW2D2ZZY7KZ4N7TJ2P5QD4M"
	ctxStateFile = "context.json"
)

// localExecutor models the LOCAL executor. The contract: nothing may reach it
// unless the user asked for it explicitly.
type localExecutor interface {
	RunLocal(argv []string) error
}

type ctxRecordingLocal struct {
	calls int
	argv  []string
}

func (l *ctxRecordingLocal) RunLocal(argv []string) error {
	l.calls++
	l.argv = argv
	return nil
}

// ctxDispatch models the verb front end: resolve first; any error fails the
// verb BEFORE an executor is chosen. There is no fallback path to local.
func ctxDispatch(t *testing.T, ctx dagrun.Context, probe dagrun.PlaneProbe, local localExecutor) (dagrun.Target, error) {
	t.Helper()
	target, err := dagrun.Resolve(ctx, probe)
	if err != nil {
		return dagrun.Target{}, err
	}
	if target.IsLocal() {
		return target, local.RunLocal(nil)
	}
	return target, nil
}

type ctxFakeProbe struct {
	reachErr     error
	workspaceErr error
	reachPlanes  []string
	workspaces   [][2]string
}

func (p *ctxFakeProbe) Reachable(plane string) error {
	p.reachPlanes = append(p.reachPlanes, plane)
	return p.reachErr
}

func (p *ctxFakeProbe) WorkspaceExists(plane, workspaceID string) error {
	p.workspaces = append(p.workspaces, [2]string{plane, workspaceID})
	return p.workspaceErr
}

// TestClusterContextUnreachableNoSilentLocalFallback is the S481.2 first red
// test: an unreachable plane with a selected cluster context exits non-zero
// with an actionable error and never executes locally.
func TestClusterContextUnreachableNoSilentLocalFallback(t *testing.T) {
	dir := ctxSafeEnv(t)
	store, err := dagrun.OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	sel, err := dagrun.ParseSelection("skel@k8s:test-plane")
	if err != nil {
		t.Fatalf("ParseSelection: %v", err)
	}
	ctx, err := dagrun.NewContext(sel, ctxULID, "~/sub")
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	if err := store.Select(ctx); err != nil {
		t.Fatalf("Select: %v", err)
	}

	later, err := dagrun.OpenStore(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	cur, ok, err := later.Current()
	if err != nil || !ok {
		t.Fatalf("Current: ok=%v err=%v", ok, err)
	}

	probe := &ctxFakeProbe{reachErr: errors.New("satellite link down")}
	local := &ctxRecordingLocal{}
	target, err := ctxDispatch(t, cur, probe, local)
	if err == nil {
		t.Fatalf("expected non-zero failure, got target %+v", target)
	}
	var pe *dagrun.PlaneUnreachableError
	if !errors.As(err, &pe) {
		t.Fatalf("want *dagrun.PlaneUnreachableError, got %T: %v", err, err)
	}
	if pe.Plane != "k8s:test-plane" {
		t.Errorf("error plane = %q, want %q", pe.Plane, "k8s:test-plane")
	}
	msg := err.Error()
	for _, want := range []string{"k8s:test-plane", "bashy dks use --none", "bashy dks local"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q missing actionable hint %q", msg, want)
		}
	}
	if len(probe.workspaces) != 0 {
		t.Errorf("WorkspaceExists probed after unreachable plane: %v", probe.workspaces)
	}
	if local.calls != 0 {
		t.Fatalf("local executor invoked %d time(s); no silent local fallback is allowed", local.calls)
	}
	if target != (dagrun.Target{}) {
		t.Errorf("no target may be produced on failure, got %+v", target)
	}
}

func TestResolveMissingWorkspaceFailsClosed(t *testing.T) {
	dir := ctxSafeEnv(t)
	store, err := dagrun.OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	ctx, err := dagrun.NewContext(dagrun.Selection{Name: "proj", Plane: "k8s:dev", Tier: dagrun.TierCluster}, ctxULID, "")
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	if err := store.Select(ctx); err != nil {
		t.Fatalf("Select: %v", err)
	}
	cur, ok, err := store.Current()
	if err != nil || !ok {
		t.Fatalf("Current: ok=%v err=%v", ok, err)
	}

	probe := &ctxFakeProbe{workspaceErr: errors.New("no such workspace")}
	local := &ctxRecordingLocal{}
	target, err := ctxDispatch(t, cur, probe, local)
	if err == nil {
		t.Fatalf("expected failure, got target %+v", target)
	}
	var we *dagrun.WorkspaceMissingError
	if !errors.As(err, &we) {
		t.Fatalf("want *dagrun.WorkspaceMissingError, got %T: %v", err, err)
	}
	if we.Plane != "k8s:dev" || we.WorkspaceID != ctxULID {
		t.Errorf("error = plane %q workspace %q, want %q/%s", we.Plane, we.WorkspaceID, "k8s:dev", ctxULID)
	}
	msg := err.Error()
	for _, want := range []string{ctxULID, "k8s:dev", "bashy dks use"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q missing %q", msg, want)
		}
	}
	if len(probe.workspaces) != 1 || probe.workspaces[0] != [2]string{"k8s:dev", ctxULID} {
		t.Errorf("WorkspaceExists args = %v, want [[k8s:dev %s]]", probe.workspaces, ctxULID)
	}
	if local.calls != 0 {
		t.Fatalf("local executor invoked %d time(s) on missing workspace", local.calls)
	}
}

func TestResolveHappyPathCluster(t *testing.T) {
	ctx, err := dagrun.NewContext(dagrun.Selection{Name: "skel", Plane: "k8s:dev", Tier: dagrun.TierCluster}, ctxULID, "~/sub")
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	probe := &ctxFakeProbe{}
	local := &ctxRecordingLocal{}
	target, err := ctxDispatch(t, ctx, probe, local)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.IsLocal() {
		t.Error("resolved cluster target must not be local")
	}
	if target.Tier != dagrun.TierCluster || target.Plane != "k8s:dev" ||
		target.WorkspaceID != ctxULID || target.Name != "skel" || target.Cwd != "~/sub" {
		t.Errorf("target = %+v", target)
	}
	if local.calls != 0 {
		t.Errorf("local executor invoked on cluster target")
	}
}

func TestSelectionSurvivesNewProcess(t *testing.T) {
	dir := ctxSafeEnv(t)
	store, err := dagrun.OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	sel, err := dagrun.ParseSelection("skel@box7")
	if err != nil {
		t.Fatalf("ParseSelection: %v", err)
	}
	ctx, err := dagrun.NewContext(sel, ctxULID, "~/docs")
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	if err := store.Select(ctx); err != nil {
		t.Fatalf("Select: %v", err)
	}

	// A new process (fresh Store on the same state dir) sees the selection.
	fresh, err := dagrun.OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore (new process): %v", err)
	}
	cur, ok, err := fresh.Current()
	if err != nil || !ok {
		t.Fatalf("Current: ok=%v err=%v", ok, err)
	}
	if cur != ctx {
		t.Errorf("reloaded context = %+v, want %+v", cur, ctx)
	}

	// The state is one JSON file in the caller-given dir, never ~/.bashy.
	b, err := os.ReadFile(filepath.Join(dir, ctxStateFile))
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("state file is not JSON: %v", err)
	}
	if got["workspace_id"] != ctxULID || got["tier"] != "sphere" {
		t.Errorf("state file contents = %v", got)
	}
}

func TestClearReturnsToLocalExplicitly(t *testing.T) {
	dir := ctxSafeEnv(t)
	store, err := dagrun.OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if _, ok, err := store.Current(); err != nil || ok {
		t.Fatalf("fresh Current: ok=%v err=%v (no implicit cluster context)", ok, err)
	}
	ctx, err := dagrun.NewContext(dagrun.Selection{Name: "skel", Tier: dagrun.TierCluster}, ctxULID, "")
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	if err := store.Select(ctx); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if _, ok, _ := store.Current(); !ok {
		t.Fatal("Current after Select: ok=false")
	}

	// `bashy dks use --none`: the selection is gone; only explicit local runs.
	if err := store.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, ok, err := store.Current(); err != nil || ok {
		t.Fatalf("Current after Clear: ok=%v err=%v, want ok=false", ok, err)
	}
	if err := store.Clear(); err != nil {
		t.Errorf("Clear must be idempotent, second call: %v", err)
	}
	if _, ok, _ := store.Current(); ok {
		t.Error("context resurrected after Clear")
	}
	if target := dagrun.LocalEscape(); !target.IsLocal() {
		t.Error("LocalEscape must yield the explicit local target")
	}
}

func TestParseSelectionForms(t *testing.T) {
	tests := []struct {
		arg  string
		want dagrun.Selection
	}{
		{"skel", dagrun.Selection{Name: "skel", Tier: dagrun.TierCluster}},
		{"skel@k8s:dev", dagrun.Selection{Name: "skel", Plane: "k8s:dev", Tier: dagrun.TierCluster}},
		{"skel@box3", dagrun.Selection{Name: "skel", Plane: "box3", Tier: dagrun.TierSphere}},
		{"skel@sandbox:pod1", dagrun.Selection{Name: "skel", Plane: "sandbox:pod1", Tier: dagrun.TierSandbox}},
		{"@ws:" + ctxULID2, dagrun.Selection{WorkspaceID: ctxULID2, Tier: dagrun.TierCluster}},
	}
	for _, tt := range tests {
		got, err := dagrun.ParseSelection(tt.arg)
		if err != nil {
			t.Errorf("ParseSelection(%q): %v", tt.arg, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseSelection(%q) = %+v, want %+v", tt.arg, got, tt.want)
		}
	}
}

func TestParseSelectionMalformed(t *testing.T) {
	tests := []string{
		"",
		" ",
		"skel ",
		"run:yoke-12",
		"kb:deploy-runbook",
		"urn:dhnt:run:yoke-12",
		"tar:bundle.tgz",
		"skel@bogus:dev",
		"skel@",
		"skel@k8s:",
		"skel@@k8s:dev",
		"skel@k8s:de@v",
		"skel!sub",
		"@ws:not-a-ulid",
		"@ws:" + ctxULID2[:25],
		"skel@ws:" + ctxULID2,
		"my/skel",
		"@",
	}
	for _, arg := range tests {
		if _, err := dagrun.ParseSelection(arg); err == nil {
			t.Errorf("ParseSelection(%q) = nil error, want failure", arg)
		}
	}

	// run: must be rejected with its own actionable reason.
	_, err := dagrun.ParseSelection("run:yoke-12")
	if err == nil || !strings.Contains(err.Error(), "weave run") {
		t.Errorf("ParseSelection(run:...) error = %v, want a weave-run explanation", err)
	}
}

func TestStoreRejectsInvalidContexts(t *testing.T) {
	dir := ctxSafeEnv(t)
	store, err := dagrun.OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	tests := []dagrun.Context{
		{WorkspaceID: "not-a-ulid", Tier: dagrun.TierCluster},
		{WorkspaceID: ctxULID, Tier: dagrun.TierUserland},
		{WorkspaceID: ctxULID},
		{WorkspaceID: ctxULID, Tier: "quantum"},
		{WorkspaceID: ctxULID, Tier: dagrun.TierCluster, Cwd: "/abs/path"},
		{WorkspaceID: ctxULID, Tier: dagrun.TierCluster, Cwd: "../escape"},
		{WorkspaceID: ctxULID, Tier: dagrun.TierCluster, Cwd: "~/.."},
		{WorkspaceID: ctxULID, Tier: dagrun.TierCluster, Cwd: "~root"},
	}
	for i, ctx := range tests {
		if err := store.Select(ctx); err == nil {
			t.Errorf("case %d: Select(%+v) = nil error, want failure", i, ctx)
		}
	}

	// NewContext mints nothing: the caller must supply the identity.
	if _, err := dagrun.NewContext(dagrun.Selection{Name: "skel", Tier: dagrun.TierCluster}, "", ""); err == nil {
		t.Error("NewContext with empty workspace id = nil error")
	}
	// A selection by id and a supplied id must agree.
	if _, err := dagrun.NewContext(dagrun.Selection{WorkspaceID: ctxULID2, Tier: dagrun.TierCluster}, ctxULID, ""); err == nil {
		t.Error("NewContext with disagreeing ids = nil error")
	}
	if _, ok, _ := store.Current(); ok {
		t.Error("rejected contexts must not be persisted")
	}
}

func TestCurrentFailsClosedOnCorruptState(t *testing.T) {
	dir := ctxSafeEnv(t)
	store, err := dagrun.OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ctxStateFile), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed corrupt state: %v", err)
	}
	if _, ok, err := store.Current(); err == nil || ok {
		t.Errorf("Current on corrupt state: ok=%v err=%v, want visible failure", ok, err)
	}
}

func TestLocalEscapeExplicitOnly(t *testing.T) {
	if target := dagrun.LocalEscape(); !target.IsLocal() || target.Tier != dagrun.TierUserland {
		t.Errorf("LocalEscape() = %+v", target)
	}
	// Resolve never yields a local target, even from a hand-built context.
	_, err := dagrun.Resolve(dagrun.Context{WorkspaceID: ctxULID, Tier: dagrun.TierUserland}, &ctxFakeProbe{})
	if err == nil || !strings.Contains(err.Error(), "explicit") {
		t.Errorf("Resolve with userland tier = %v, want explicit-only failure", err)
	}
	// Fail closed without a probe.
	if _, err := dagrun.Resolve(dagrun.Context{WorkspaceID: ctxULID, Tier: dagrun.TierCluster}, nil); err == nil {
		t.Error("Resolve with nil probe = nil error")
	}
}
