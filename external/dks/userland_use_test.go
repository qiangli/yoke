package dks

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/dagrun"
	"github.com/qiangli/yoke/pkg/dagrun/cluster"
)

// fakeRunner records every call and answers from a script; it never execs.
type fakeRunner struct {
	calls []runnerCall
	// script maps a script key — the kubectl action ("get", "apply",
	// "delete") refined by the plane question it asks — to its scripted
	// answer. A key the test does not script falls back to the bare action,
	// exactly the way the original fake answered.
	stdout map[string][]byte
	stderr map[string][]byte
	exit   map[string]int
	err    map[string]error
}

type runnerCall struct {
	argv  []string
	stdin []byte
}

// actionOf returns the first kubectl action of argv ("" when none).
func actionOf(argv []string) string {
	for _, a := range argv {
		switch a {
		case "get", "apply", "delete":
			return a
		}
	}
	return ""
}

// scriptKey refines a get into the plane question it asks, so a test can
// script reachability, the by-name list and the by-id lookup separately:
// "get-namespace" (reachability), "get-by-name" (workspace list by the
// name label) and "get-configmap" (any other configmap get).
func scriptKey(argv []string) string {
	action := actionOf(argv)
	if action != "get" {
		return action
	}
	joined := strings.Join(argv, " ")
	switch {
	case strings.Contains(joined, LabelWorkspaceName+"="):
		return "get-by-name"
	case strings.Contains(joined, "configmap"):
		return "get-configmap"
	case strings.Contains(joined, "namespace"):
		return "get-namespace"
	}
	return "get"
}

func (f *fakeRunner) Run(_ context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error) {
	f.calls = append(f.calls, runnerCall{argv: append([]string(nil), argv...), stdin: append([]byte(nil), stdin...)})
	if f.exit == nil {
		f.exit = map[string]int{}
	}
	if f.stdout == nil {
		f.stdout = map[string][]byte{}
	}
	if f.stderr == nil {
		f.stderr = map[string][]byte{}
	}
	key, action := scriptKey(argv), actionOf(argv)
	stdout, ok := f.stdout[key]
	if !ok {
		stdout = f.stdout[action]
	}
	stderr, ok := f.stderr[key]
	if !ok {
		stderr = f.stderr[action]
	}
	exit, ok := f.exit[key]
	if !ok {
		exit = f.exit[action]
	}
	err, ok := f.err[key]
	if !ok {
		err = f.err[action]
	}
	return stdout, stderr, exit, err
}

// applyCalls returns the recorded runner calls that apply to the plane.
func applyCalls(calls []runnerCall) []runnerCall {
	var out []runnerCall
	for _, c := range calls {
		if actionOf(c.argv) == "apply" {
			out = append(out, c)
		}
	}
	return out
}

// wsListJSON builds the `kubectl get configmap -o json` answer of a plane
// holding exactly the given workspaces of one name.
func wsListJSON(t *testing.T, name string, ids ...string) []byte {
	t.Helper()
	items := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		items = append(items, map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      workspaceObjectName(id),
				"namespace": userlandNamespace,
				"labels": map[string]any{
					dagrun.LabelManagedBy: dagrun.ManagedByBashy,
					dagrun.LabelWorkspace: id,
					LabelWorkspaceName:    name,
				},
			},
			"data": map[string]any{"name": name},
		})
	}
	b, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "ConfigMapList", "items": items})
	if err != nil {
		t.Fatalf("encode configmap list: %v", err)
	}
	return b
}

// unreachable scripts a plane that cannot be reached at all.
func (f *fakeRunner) unreachable() *fakeRunner {
	if f.exit == nil {
		f.exit = map[string]int{}
	}
	if f.stderr == nil {
		f.stderr = map[string][]byte{}
	}
	f.exit["get"] = 1
	f.stderr["get"] = []byte("Unable to connect to the server: dial tcp: no route to host")
	return f
}

// testEnv is the hermetic userlandEnv: fake runner, fake local exec, pinned
// ids, state under t.TempDir via $BASHY_HOME. Nothing execs, nothing networks.
type testEnv struct {
	env       *userlandEnv
	runner    *fakeRunner
	execCalls []execCall
	execExit  int
	home      string
}

type execCall struct {
	name string
	args []string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	te := &testEnv{runner: &fakeRunner{
		stdout: map[string][]byte{},
		stderr: map[string][]byte{},
		exit:   map[string]int{},
		err:    map[string]error{},
	}, home: t.TempDir()}
	t.Setenv("BASHY_HOME", te.home)
	te.env = &userlandEnv{
		stateDir:  func() string { return filepath.Join(te.home, "dks", "context") },
		newRunner: func() cluster.Runner { return te.runner },
		mintID:    func() string { return "01JBQ5E9P2WYZ5ZG4HJ3XRG7W8" },
		execLocal: func(_ context.Context, name string, args []string) int {
			te.execCalls = append(te.execCalls, execCall{name: name, args: append([]string(nil), args...)})
			return te.execExit
		},
	}
	return te
}

// runUserland mounts the userland commands on a silent root (exactly the way
// DispatchUserland does) and returns the mapped exit code plus both streams.
func runUserland(t *testing.T, env *userlandEnv, args ...string) (int, string, string) {
	t.Helper()
	root := &cobra.Command{Use: "dks", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newUserlandCmds(env)...)
	root.SetArgs(args)
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	err := root.Execute()
	return userlandExitCode(err), outBuf.String(), errBuf.String()
}

// currentContext reads the persisted selection under $BASHY_HOME, the way a
// later command would.
func currentContext(t *testing.T) (dagrun.Context, bool) {
	t.Helper()
	home, ok := os.LookupEnv("BASHY_HOME")
	if !ok {
		t.Fatal("BASHY_HOME not set")
	}
	store, err := dagrun.OpenStore(filepath.Join(home, "dks", "context"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	ctx, ok, err := store.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	return ctx, ok
}

// FIRST RED TEST of S481.2 slice 2: selecting a workspace on an unreachable
// plane fails closed with dagrun's actionable message and NEVER executes
// anything locally.
func TestUseUnreachablePlaneNeverRunsLocally(t *testing.T) {
	te := newTestEnv(t)
	te.runner.unreachable()

	code, stdout, stderr := runUserland(t, te.env, "use", "proj@k8s:down-plane")

	if code == 0 {
		t.Fatalf("use on an unreachable plane exited 0 (stdout %q): nothing may succeed by absence of evidence", stdout)
	}
	for _, want := range []string{"unreachable", "nothing ran", "use --none", "down-plane"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr %q does not contain %q", stderr, want)
		}
	}
	if len(te.execCalls) != 0 {
		t.Errorf("local exec ran on an unreachable plane: %v", te.execCalls)
	}
	if len(te.runner.calls) != 1 {
		t.Fatalf("runner calls = %v, want exactly the one reachability probe", te.runner.calls)
	}
	wantArgv := []string{"kubectl", "--context", "down-plane", "get", "namespace", "default", "-o", "name"}
	got := te.runner.calls[0].argv
	if strings.Join(got, " ") != strings.Join(wantArgv, " ") {
		t.Errorf("reachability argv\n got %q\nwant %q", got, wantArgv)
	}
	if _, ok := currentContext(t); ok {
		t.Error("an unreachable plane was persisted as the selected context")
	}
}

// Selecting an existing workspace by NAME asks the plane which workspace
// holds the name (the dhnt.io/workspace-name label) and selects THAT id,
// then persists the context under $BASHY_HOME/dks/context, printing
// workspace and plane.
func TestUseExistingWorkspaceSelectsAndPersists(t *testing.T) {
	te := newTestEnv(t)
	// The plane's skel is NOT this client's mint: the found id must win.
	const planeID = "01M5DGE9P2WYZ5ZG4HJ3XRG7W8"
	te.runner.exit["get-by-name"] = 0
	te.runner.stdout["get-by-name"] = wsListJSON(t, "skel", planeID)

	code, stdout, stderr := runUserland(t, te.env, "use", "skel@k8s:dev")
	if code != 0 {
		t.Fatalf("use exited %d: %q", code, stderr)
	}
	for _, want := range []string{"skel", "k8s:dev"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout %q does not mention %q", stdout, want)
		}
	}
	ctx, ok := currentContext(t)
	if !ok {
		t.Fatal("use did not persist a context")
	}
	if ctx.Name != "skel" || ctx.Plane != "k8s:dev" || ctx.Tier != dagrun.TierCluster {
		t.Errorf("persisted context = %+v, want name skel plane k8s:dev tier cluster", ctx)
	}
	if ctx.WorkspaceID != planeID {
		t.Errorf("persisted workspace id = %q, want the plane's %q: the record the plane holds wins over any private mint", ctx.WorkspaceID, planeID)
	}
	if !dagrun.ValidWorkspaceID(ctx.WorkspaceID) {
		t.Errorf("persisted workspace id %q is not a ULID", ctx.WorkspaceID)
	}
	// Resolve must have probed reachability then listed by name, nothing else.
	if len(te.runner.calls) != 2 {
		t.Fatalf("runner calls = %v, want reachability + list-by-name only", len(te.runner.calls))
	}
	// Label-only selection: the workspace is found by its labels, never by
	// guessing its object name into the selector.
	sel := "app.kubernetes.io/managed-by=bashy," + LabelWorkspaceName + "=skel"
	cmArgv := strings.Join(te.runner.calls[1].argv, " ")
	for _, want := range []string{"--context dev", "get configmap", "-n bashy-userland", "-l " + sel, "-o json"} {
		if !strings.Contains(cmArgv, want) {
			t.Errorf("list-by-name argv %q misses %q", cmArgv, want)
		}
	}
	if strings.Contains(cmArgv, "bashy-ws-") {
		t.Errorf("list-by-name argv names the object instead of selecting by label: %q", cmArgv)
	}
	if len(applyCalls(te.runner.calls)) != 0 {
		t.Errorf("selecting an existing workspace applied something: %v", te.runner.calls)
	}
	if len(te.execCalls) != 0 {
		t.Errorf("use executed locally: %v", te.execCalls)
	}
}

// First use of a NEW name creates the workspace (server-side apply, field
// manager bashy) before persisting — the walking-skeleton `use skel` path.
// The record carries the name BOTH as the dhnt.io/workspace-name label —
// the key a later `use <name>` selects by — and as data.name.
func TestUseFirstUseCreatesWorkspace(t *testing.T) {
	te := newTestEnv(t)
	te.runner.exit["get-by-name"] = 0
	te.runner.stdout["get-by-name"] = wsListJSON(t, "skel") // the plane holds no skel
	te.runner.exit["apply"] = 0

	code, _, stderr := runUserland(t, te.env, "use", "skel@k8s:dev")
	if code != 0 {
		t.Fatalf("first use exited %d: %q", code, stderr)
	}
	applies := applyCalls(te.runner.calls)
	if len(applies) != 1 {
		t.Fatalf("apply calls = %d, want exactly one create", len(applies))
	}
	apply := applies[0]
	for _, want := range []string{"apply", "--server-side", "--field-manager=bashy", "-f", "-"} {
		found := false
		for _, a := range apply.argv {
			if a == want {
				found = true
			}
		}
		if !found {
			t.Errorf("apply argv %q misses %q", apply.argv, want)
		}
	}
	var list struct {
		Kind  string           `json:"kind"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(apply.stdin, &list); err != nil {
		t.Fatalf("apply stdin is not one JSON list: %v\n%s", err, apply.stdin)
	}
	if list.Kind != "List" || len(list.Items) != 2 {
		t.Fatalf("apply payload kind=%q items=%d, want a List of Namespace+ConfigMap", list.Kind, len(list.Items))
	}
	ns, _ := list.Items[0]["metadata"].(map[string]any)
	nsLabels, _ := ns["labels"].(map[string]any)
	if list.Items[0]["kind"] != "Namespace" || ns["name"] != "bashy-userland" || nsLabels["app.kubernetes.io/managed-by"] != "bashy" {
		t.Errorf("first item = %v, want Namespace bashy-userland labelled managed-by=bashy", list.Items[0])
	}
	cm := list.Items[1]
	if cm["kind"] != "ConfigMap" {
		t.Errorf("second item kind = %v, want ConfigMap", cm["kind"])
	}
	md, _ := cm["metadata"].(map[string]any)
	if md["name"] != "bashy-ws-01jbq5e9p2wyz5zg4hj3xrg7w8" || md["namespace"] != "bashy-userland" {
		t.Errorf("applied object = %v/%v, want the lowercased bashy-ws-<id> in bashy-userland", md["namespace"], md["name"])
	}
	labels, _ := md["labels"].(map[string]any)
	if labels["app.kubernetes.io/managed-by"] != "bashy" || labels["dhnt.io/workspace"] != "01JBQ5E9P2WYZ5ZG4HJ3XRG7W8" {
		t.Errorf("applied labels = %v, want managed-by=bashy and the workspace label", labels)
	}
	if labels["dhnt.io/workspace-name"] != "skel" {
		t.Errorf("applied labels = %v, want dhnt.io/workspace-name=skel: the name label is how a later use selects this record", labels)
	}
	data, _ := cm["data"].(map[string]any)
	if data["name"] != "skel" {
		t.Errorf("applied data = %v, want data.name=skel kept", data)
	}
	if _, ok := currentContext(t); !ok {
		t.Error("first use did not persist the new workspace context")
	}
	if len(te.execCalls) != 0 {
		t.Errorf("create executed locally: %v", te.execCalls)
	}
}

// FIRST RED TEST of the S481.2 name-selection fix. LIVE bug (Sprint 481
// walking skeleton): a second client on another host, same account and
// plane, ran `bashy dks use sk481@k8s:cloudbox` and minted a NEW workspace
// (01M5DHRZRR884BPX167EMS11YK) next to the first client's sk481
// (01M5DHMQ5J6JPRHQXDJP18X789) — two workspaces, one name, no shared home
// or sessions. The fix: the fake plane already holds sk481 with id X, so
// `use sk481` selects X and applies nothing.
func TestUseSelectsExistingWorkspaceByName(t *testing.T) {
	te := newTestEnv(t)
	const idA = "01M5DHMQ5J6JPRHQXDJP18X789"
	te.runner.exit["get-by-name"] = 0
	te.runner.stdout["get-by-name"] = wsListJSON(t, "sk481", idA)
	te.runner.exit["apply"] = 0 // must never be needed

	code, stdout, stderr := runUserland(t, te.env, "use", "sk481@k8s:cloudbox")
	if code != 0 {
		t.Fatalf("use of an existing name exited %d: %q %q", code, stdout, stderr)
	}
	ctx, ok := currentContext(t)
	if !ok {
		t.Fatal("use did not persist a context")
	}
	if ctx.WorkspaceID != idA {
		t.Errorf("selected workspace id = %q, want the plane's existing %q: a name must select the record the plane holds, never mint a sibling", ctx.WorkspaceID, idA)
	}
	if ctx.Name != "sk481" || ctx.Plane != "k8s:cloudbox" || ctx.Tier != dagrun.TierCluster {
		t.Errorf("persisted context = %+v, want name sk481 plane k8s:cloudbox tier cluster", ctx)
	}
	if applies := applyCalls(te.runner.calls); len(applies) != 0 {
		t.Errorf("selecting an existing workspace applied %d time(s): the live bug was a second record per client", len(applies))
	}
	if len(te.runner.calls) != 2 {
		t.Fatalf("runner calls = %v, want reachability + list-by-name only", len(te.runner.calls))
	}
	sel := "app.kubernetes.io/managed-by=bashy," + LabelWorkspaceName + "=sk481"
	cmArgv := strings.Join(te.runner.calls[1].argv, " ")
	for _, want := range []string{"--context cloudbox", "get configmap", "-n bashy-userland", "-l " + sel, "-o json"} {
		if !strings.Contains(cmArgv, want) {
			t.Errorf("list-by-name argv %q misses %q", cmArgv, want)
		}
	}
	for _, want := range []string{"sk481", idA, "cloudbox"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout %q does not mention %q", stdout, want)
		}
	}
	if len(te.execCalls) != 0 {
		t.Errorf("use executed locally: %v", te.execCalls)
	}
}

// Create happens ONLY on first use: client A creates sk481 on an empty
// plane; client B — another host, same account and plane, no local state —
// selects A's workspace and applies NOTHING. Pre-fix live evidence: B minted
// 01M5DHRZRR884BPX167EMS11YK next to A's 01M5DHMQ5J6JPRHQXDJP18X789.
func TestUseCreatesOnFirstUseOnly(t *testing.T) {
	te := newTestEnv(t) // client A
	te.runner.exit["get-by-name"] = 0
	te.runner.stdout["get-by-name"] = wsListJSON(t, "sk481") // the plane holds no sk481
	te.runner.exit["apply"] = 0

	code, _, stderr := runUserland(t, te.env, "use", "sk481@k8s:cloudbox")
	if code != 0 {
		t.Fatalf("first use exited %d: %q", code, stderr)
	}
	ctxA, ok := currentContext(t)
	if !ok || ctxA.WorkspaceID != "01JBQ5E9P2WYZ5ZG4HJ3XRG7W8" {
		t.Fatalf("first use persisted %+v ok=%v, want the minted 01JBQ5E9P2WYZ5ZG4HJ3XRG7W8", ctxA, ok)
	}
	if applies := applyCalls(te.runner.calls); len(applies) != 1 {
		t.Fatalf("apply calls = %d, want exactly the one first-use create", len(applies))
	}

	// The plane now holds A's record (the apply above wrote it); script it.
	te.runner.stdout["get-by-name"] = wsListJSON(t, "sk481", ctxA.WorkspaceID)

	// Client B: a fresh host — no local state — same plane, its own minter.
	homeB := t.TempDir()
	t.Setenv("BASHY_HOME", homeB)
	envB := &userlandEnv{
		stateDir:  func() string { return filepath.Join(homeB, "dks", "context") },
		newRunner: func() cluster.Runner { return te.runner },
		mintID:    func() string { return "01M5DHRZRR884BPX167EMS11YK" },
		execLocal: func(context.Context, string, []string) int { return 0 },
	}
	code, stdout, stderr := runUserland(t, envB, "use", "sk481@k8s:cloudbox")
	if code != 0 {
		t.Fatalf("second client's use exited %d: %q %q", code, stdout, stderr)
	}
	ctxB, ok := currentContext(t)
	if !ok {
		t.Fatal("second client's use did not persist a context")
	}
	if ctxB.WorkspaceID != ctxA.WorkspaceID {
		t.Errorf("second client selected %q, want A's %q: one name, one workspace, every client", ctxB.WorkspaceID, ctxA.WorkspaceID)
	}
	if applies := applyCalls(te.runner.calls); len(applies) != 1 {
		t.Fatalf("apply calls = %d after the second client, want still the one create", len(applies))
	}
	for _, c := range te.runner.calls {
		if bytes.Contains(c.stdin, []byte("01M5DHRZRR884BPX167EMS11YK")) || bytes.Contains(c.stdin, []byte(strings.ToLower("01M5DHRZRR884BPX167EMS11YK"))) {
			t.Errorf("the second client's mint leaked to the plane: %s", c.stdin)
		}
	}
}

// Two workspace records of one name are an explicit ambiguity: use fails
// closed listing both ids, selects nothing, creates nothing.
func TestUseAmbiguousNameFailsClosed(t *testing.T) {
	te := newTestEnv(t)
	const (
		idA = "01M5DHMQ5J6JPRHQXDJP18X789"
		idB = "01M5DHRZRR884BPX167EMS11YK"
	)
	te.runner.exit["get-by-name"] = 0
	te.runner.stdout["get-by-name"] = wsListJSON(t, "sk481", idA, idB)
	te.runner.exit["apply"] = 0 // must never be needed

	code, _, stderr := runUserland(t, te.env, "use", "sk481@k8s:cloudbox")
	if code == 0 {
		t.Fatal("use of an ambiguous name exited 0")
	}
	for _, want := range []string{"ambiguous", "sk481", idA, idB} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr %q does not contain %q", stderr, want)
		}
	}
	if applies := applyCalls(te.runner.calls); len(applies) != 0 {
		t.Errorf("an ambiguous name applied something: %v", te.runner.calls)
	}
	if _, ok := currentContext(t); ok {
		t.Error("an ambiguous name was persisted as the selected context")
	}
	if len(te.execCalls) != 0 {
		t.Errorf("an ambiguous name executed locally: %v", te.execCalls)
	}
}

// A ULID is uppercase Crockford base32, but a Kubernetes object name must
// be a LOWERCASE RFC 1123 subdomain — the live API rejected the uppercase
// name (render.go already lowercases run objects). The workspace object
// name lowercases the id; the dhnt.io/workspace label keeps it as minted;
// and the existence lookup matches the plane's answer by the same
// lowercased name, never the uppercase one.
func TestWorkspaceObjectNameIsRFC1123(t *testing.T) {
	id := "01M5DGE9P2WYZ5ZG4HJ3XRG7W8" // a minted ULID: uppercase
	name := workspaceObjectName(id)
	if name != "bashy-ws-"+strings.ToLower(id) {
		t.Errorf("workspaceObjectName(%q) = %q, want bashy-ws-<lowercased id>", id, name)
	}
	rfc1123Subdomain := regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	if !rfc1123Subdomain.MatchString(name) {
		t.Errorf("workspace object name %q is not a lowercase RFC 1123 subdomain", name)
	}

	// The lookup agrees: an answer naming the lowercased object finds the
	// workspace; an answer spelling the name in UPPERCASE — the spelling the
	// API rejects — does not.
	te := newTestEnv(t)
	te.runner.exit["get"] = 0
	te.runner.stdout["get"] = []byte("configmap/" + name + "\n")
	if err := (&KubeProbe{Runner: te.runner}).WorkspaceExists("k8s:dev", id); err != nil {
		t.Errorf("WorkspaceExists did not match the plane's lowercased answer: %v", err)
	}

	te2 := newTestEnv(t)
	te2.runner.exit["get"] = 0
	te2.runner.stdout["get"] = []byte("configmap/bashy-ws-" + id + "\n")
	if err := (&KubeProbe{Runner: te2.runner}).WorkspaceExists("k8s:dev", id); err == nil {
		t.Error("WorkspaceExists matched an uppercase object name the API would have rejected")
	}

	// The label selector keeps the id as minted: labels allow uppercase.
	sel := "app.kubernetes.io/managed-by=bashy,dhnt.io/workspace=" + id
	if len(te.runner.calls) != 1 {
		t.Fatalf("WorkspaceExists runner calls = %v, want exactly one", te.runner.calls)
	}
	if got := strings.Join(te.runner.calls[0].argv, " "); !strings.Contains(got, "-l "+sel) {
		t.Errorf("workspace-exists argv %q does not select by the minted (uppercase) label %q", got, sel)
	}
}

// FIRST RED TEST of S481.2: on a FRESH plane — reachable, but no
// bashy-userland namespace — first use of a new name must bootstrap the
// namespace and the workspace record in ONE apply. The fake plays the API
// server's namespace rule faithfully: a namespaced apply against a namespace
// that does not exist yet is refused with the live NotFound error, and a
// Namespace object in the payload creates it, items applied in order.
func TestUseCreatesNamespaceOnFreshPlane(t *testing.T) {
	te := newTestEnv(t)
	te.runner.exit["get-by-name"] = 0
	te.runner.stdout["get-by-name"] = wsListJSON(t, "sk481") // reachable; no sk481 anywhere
	te.runner.exit["apply"] = 0

	code, stdout, stderr := runUserland(t, te.env, "use", "sk481@k8s:cloudbox")
	if code != 0 {
		t.Fatalf("use on a fresh plane exited %d: %q %q", code, stdout, stderr)
	}

	var apply *runnerCall
	for i := range te.runner.calls {
		for _, a := range te.runner.calls[i].argv {
			if a == "apply" {
				c := te.runner.calls[i]
				apply = &c
			}
		}
	}
	if apply == nil {
		t.Fatalf("no apply issued on a fresh plane: %v", te.runner.calls)
	}
	var list struct {
		Kind  string           `json:"kind"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(apply.stdin, &list); err != nil {
		t.Fatalf("apply stdin is not one JSON payload: %v\n%s", err, apply.stdin)
	}
	if list.Kind != "List" || len(list.Items) != 2 {
		t.Fatalf("apply payload kind=%q items=%d, want one List of exactly Namespace+ConfigMap (a single apply call)", list.Kind, len(list.Items))
	}
	if list.Items[0]["kind"] != "Namespace" {
		t.Errorf("first item kind = %v, want the Namespace BEFORE the ConfigMap", list.Items[0]["kind"])
	}
	ns, _ := list.Items[0]["metadata"].(map[string]any)
	if ns["name"] != "bashy-userland" {
		t.Errorf("namespace item name = %v, want bashy-userland", ns["name"])
	}
	if cm, _ := list.Items[1]["metadata"].(map[string]any); cm["namespace"] != "bashy-userland" || !strings.Contains(cm["name"].(string), "bashy-ws-") {
		t.Errorf("configmap item = %v/%v, want bashy-ws-<id> in bashy-userland", cm["namespace"], cm["name"])
	}
	if ctx, ok := currentContext(t); !ok || ctx.WorkspaceID != "01JBQ5E9P2WYZ5ZG4HJ3XRG7W8" {
		t.Errorf("selection after fresh-plane create = %+v ok=%v, want the minted workspace", ctx, ok)
	}
	if !strings.Contains(stdout, "cloudbox") {
		t.Errorf("stdout %q does not name the plane", stdout)
	}
}

// A payload WITHOUT the namespace first is refused the way the live fresh
// plane refused it — the regression the fix closes: the ConfigMap apply used
// to run alone against a namespace that did not exist.
func TestUseApplyWithoutNamespaceFailsOnFreshPlane(t *testing.T) {
	te := newTestEnv(t)
	te.runner.exit["get-by-name"] = 0
	te.runner.stdout["get-by-name"] = wsListJSON(t, "sk481")
	te.runner.exit["apply"] = 1
	te.runner.stderr["apply"] = []byte(`Error from server (NotFound): namespaces "bashy-userland" not found`)

	code, _, stderr := runUserland(t, te.env, "use", "sk481@k8s:cloudbox")
	if code == 0 {
		t.Fatal("use exited 0 although the plane refused the apply")
	}
	if !strings.Contains(stderr, "bashy-userland") || !strings.Contains(stderr, "exited 1") {
		t.Errorf("stderr %q does not carry the plane's refusal", stderr)
	}
	if _, ok := currentContext(t); ok {
		t.Error("a refused apply was persisted as the selected context")
	}
}

// A selected workspace that vanished from its plane fails closed with the
// actionable missing-workspace message; the selection stays, nothing runs.
func TestUseMissingWorkspaceFailsClosedWithoutCreate(t *testing.T) {
	te := newTestEnv(t)
	// An existing selection by name: reuse its id, do NOT auto-recreate.
	store, err := dagrun.OpenStore(filepath.Join(te.home, "dks", "context"))
	if err != nil {
		t.Fatal(err)
	}
	prev := dagrun.Context{
		WorkspaceID: "01JBQ5E9P2WYZ5ZG4HJ3XRG7W8",
		Name:        "skel",
		Plane:       "k8s:dev",
		Tier:        dagrun.TierCluster,
	}
	if err := store.Select(prev); err != nil {
		t.Fatal(err)
	}
	te.runner.exit["get-by-name"] = 0 // plane reachable; no skel record anywhere
	te.runner.stdout["get-by-name"] = wsListJSON(t, "skel")

	code, _, stderr := runUserland(t, te.env, "use", "skel@k8s:dev")
	if code == 0 {
		t.Fatal("use of a missing workspace exited 0")
	}
	for _, want := range []string{"missing", "nothing ran"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr %q does not contain %q", stderr, want)
		}
	}
	for _, c := range te.runner.calls {
		if len(c.argv) > 1 && c.argv[1] == "apply" {
			t.Errorf("missing workspace was silently re-created: %v", c.argv)
		}
	}
	if len(te.execCalls) != 0 {
		t.Errorf("missing workspace executed locally: %v", te.execCalls)
	}
	cur, ok := currentContext(t)
	if !ok || cur.WorkspaceID != prev.WorkspaceID {
		t.Errorf("selection = %+v ok=%v, want it to stay %s", cur, ok, prev.WorkspaceID)
	}
}

// Malformed selectors are rejected by the grammar with the actionable
// dagrun message; no probe, no exec, no persistence.
func TestUseMalformedSelectorFailsClosed(t *testing.T) {
	for _, bad := range []string{"run:abc", "@ws:not-a-ulid", "a/b", " name", ""} {
		te := newTestEnv(t)
		args := []string{"use"}
		if bad != "" {
			args = append(args, bad)
		} else {
			args = append(args, "")
		}
		code, _, stderr := runUserland(t, te.env, args...)
		if code == 0 {
			t.Errorf("use %q exited 0", bad)
		}
		if !strings.Contains(stderr, "dagrun") && !strings.Contains(stderr, "usage") && !strings.Contains(stderr, "workspace") {
			t.Errorf("use %q stderr %q is not actionable", bad, stderr)
		}
		if len(te.runner.calls) != 0 || len(te.execCalls) != 0 {
			t.Errorf("use %q touched the plane or ran locally: %v %v", bad, te.runner.calls, te.execCalls)
		}
	}
}

// Bare `use` reports the current selection, or 'local' when none / after
// `use --none` cleared it explicitly.
func TestBareUseAndUseNone(t *testing.T) {
	te := newTestEnv(t)
	code, stdout, stderr := runUserland(t, te.env, "use")
	if code != 0 {
		t.Fatalf("bare use exited %d: %q", code, stderr)
	}
	if !strings.Contains(stdout, "local") {
		t.Errorf("bare use with no selection printed %q, want local", stdout)
	}

	te2 := newTestEnv(t)
	te2.runner.exit["get-by-name"] = 0
	te2.runner.stdout["get-by-name"] = wsListJSON(t, "skel") // plane holds no skel: first use creates it
	te2.runner.exit["apply"] = 0
	if code, _, stderr := runUserland(t, te2.env, "use", "skel"); code != 0 {
		t.Fatalf("use skel exited %d: %q", code, stderr)
	}
	code, stdout, _ = runUserland(t, te2.env, "use")
	if code != 0 || !strings.Contains(stdout, "skel") {
		t.Fatalf("bare use after selection: code=%d stdout=%q, want skel", code, stdout)
	}

	code, stdout, stderr = runUserland(t, te2.env, "use", "--none")
	if code != 0 {
		t.Fatalf("use --none exited %d: %q", code, stderr)
	}
	if !strings.Contains(stdout, "local") {
		t.Errorf("use --none printed %q, want an explicit local confirmation", stdout)
	}
	if _, ok := currentContext(t); ok {
		t.Error("use --none left a selection behind")
	}
	if len(te2.execCalls) != 0 {
		t.Errorf("use --none executed something: %v", te2.execCalls)
	}
}

// `local -- <cmd>` is the ONLY local escape: it runs exactly the explicit
// command, depends on no cluster state (the runner is never consulted), and
// propagates the command's exit code.
func TestUseNoneThenLocalEscapeRunsOnlyExplicitly(t *testing.T) {
	te := newTestEnv(t)
	te.runner.unreachable() // local must not care: no cluster dependency
	te.execExit = 7

	if code, _, stderr := runUserland(t, te.env, "use", "--none"); code != 0 {
		t.Fatalf("use --none exited %d: %q", code, stderr)
	}

	code, stdout, stderr := runUserland(t, te.env, "local", "--", "render", "report.md", "-o", "out/x.pdf")
	if code != 7 {
		t.Fatalf("local -- render exited %d, want the command's own 7", code)
	}
	if len(te.execCalls) != 1 {
		t.Fatalf("local exec calls = %v, want exactly one", te.execCalls)
	}
	got := te.execCalls[0]
	if got.name != "render" || strings.Join(got.args, " ") != "report.md -o out/x.pdf" {
		t.Errorf("local exec'd %q %q, want render report.md -o out/x.pdf", got.name, got.args)
	}
	if len(te.runner.calls) != 0 {
		t.Errorf("local escape consulted the cluster: %v", te.runner.calls)
	}
	_ = stdout
	_ = stderr
}

// `local` without the explicit `--` separator is a usage error, never an
// implicit local run.
func TestLocalRequiresDoubleDash(t *testing.T) {
	for _, args := range [][]string{{"local", "render", "x"}, {"local", "--"}, {"local"}} {
		te := newTestEnv(t)
		code, _, stderr := runUserland(t, te.env, args...)
		if code == 0 {
			t.Errorf("local %v exited 0, want a usage failure", args)
		}
		if !strings.Contains(stderr, "--") {
			t.Errorf("local %v stderr %q does not show the -- form", args, stderr)
		}
		if len(te.execCalls) != 0 {
			t.Errorf("local %v executed implicitly: %v", args, te.execCalls)
		}
	}
}

// Non-Kubernetes planes fail closed with the plane named; no probe exec.
func TestUseNonKubernetesPlaneFailsClosed(t *testing.T) {
	te := newTestEnv(t)
	code, _, stderr := runUserland(t, te.env, "use", "proj@sandbox:host9")
	if code == 0 {
		t.Fatal("use on a sandbox plane exited 0")
	}
	if !strings.Contains(stderr, "sandbox:host9") {
		t.Errorf("stderr %q does not name the plane", stderr)
	}
	if len(te.runner.calls) != 0 || len(te.execCalls) != 0 {
		t.Errorf("sandbox-plane use reached the runner or local exec: %v %v", te.runner.calls, te.execCalls)
	}
}

// The default plane (no scope) probes kubectl with no --context.
func TestUseDefaultPlaneProbesWithoutContext(t *testing.T) {
	te := newTestEnv(t)
	te.runner.exit["get-by-name"] = 0
	te.runner.stdout["get-by-name"] = wsListJSON(t, "skel", "01JBQ5E9P2WYZ5ZG4HJ3XRG7W8")
	if code, _, stderr := runUserland(t, te.env, "use", "skel"); code != 0 {
		t.Fatalf("use skel exited %d: %q", code, stderr)
	}
	for _, c := range te.runner.calls {
		for _, a := range c.argv {
			if a == "--context" {
				t.Errorf("default-plane probe passed --context: %q", c.argv)
			}
		}
	}
	if got := strings.Join(te.runner.calls[0].argv, " "); got != "kubectl get namespace default -o name" {
		t.Errorf("reachability argv = %q, want the context-less probe", got)
	}
}

// The state dir falls back to $HOME/.bashy when BASHY_HOME is unset.
func TestUserlandStateDirFallsBackToDotBashy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_HOME", "")
	if got := userlandStateDir(); got != filepath.Join(home, ".bashy", "dks", "context") {
		t.Errorf("userlandStateDir() = %q, want %q", got, filepath.Join(home, ".bashy", "dks", "context"))
	}
}

// The ULID minter mints valid, distinct, monotonic ids.
func TestMintWorkspaceID(t *testing.T) {
	prev := ""
	for i := 0; i < 8; i++ {
		id := mintULID()
		if !dagrun.ValidWorkspaceID(id) {
			t.Fatalf("minted %q is not a valid workspace id", id)
		}
		if id == prev {
			t.Fatalf("minted the same id twice: %q", id)
		}
		if prev != "" && id[:10] < prev[:10] {
			t.Fatalf("timestamp went backwards: %q after %q", id, prev)
		}
		prev = id
	}
}
