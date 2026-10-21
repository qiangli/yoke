package dks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/dagrun"
)

// testExposeToken is the pinned 32-byte hex token the fake minter hands out:
// the tests assert it reaches the Secret and NOTHING else.
const testExposeToken = "3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f"

const testSatellite = "https://satellite.example.com"

const testOutpostHost = "host-a"

// fakeOutpostApps records the Outpost host-side calls without execing the
// installed outpost.
type fakeOutpostApps struct {
	adds   []exposeAppSpec
	rms    []string
	addErr error
	rmErr  error
}

func (f *fakeOutpostApps) Add(_ context.Context, spec exposeAppSpec) error {
	f.adds = append(f.adds, spec)
	return f.addErr
}

func (f *fakeOutpostApps) Rm(_ context.Context, app string) error {
	f.rms = append(f.rms, app)
	return f.rmErr
}

// exposeTest is the hermetic exposeEnv: the run fake plus a fake Outpost
// host side, a pinned token and a pinned satellite base.
type exposeTest struct {
	*runTest
	apps *fakeOutpostApps
	eenv *exposeEnv
}

func newExposeTest(t *testing.T) *exposeTest {
	t.Helper()
	rt := newRunTest(t)
	et := &exposeTest{runTest: rt, apps: &fakeOutpostApps{}}
	et.eenv = &exposeEnv{
		runEnv:       rt.renv,
		apps:         et.apps,
		mintToken:    func() (string, error) { return testExposeToken, nil },
		tunnelBase:   func(dagrun.Context) (string, error) { return testSatellite, nil },
		outpostHost:  func() (string, error) { return testOutpostHost, nil },
		relayDialCtx: context.Background,
	}
	return et
}

func (et *exposeTest) exec(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	root := &cobra.Command{Use: "dks", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newExposeCmds(et.eenv)...)
	root.SetArgs(args)
	var outBuf, errBuf strings.Builder
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	return userlandExitCode(root.Execute()), outBuf.String(), errBuf.String()
}

// appliedObjects decodes every object the recorded apply calls sent on stdin,
// keyed by kind.
func appliedObjects(t *testing.T, calls []runnerCall) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, c := range calls {
		if action(c.argv) != "apply" || len(c.stdin) == 0 {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(c.stdin, &obj); err != nil {
			t.Fatalf("apply stdin is not one JSON object: %v\n%s", err, c.stdin)
		}
		kind, _ := obj["kind"].(string)
		out[kind] = obj
	}
	return out
}

// FIRST RED TEST of S487.2: an admin-grade target or an app name colliding
// with an Outpost built-in is REFUSED without --force, having registered
// nothing with Outpost and applied nothing to the plane.
func TestExposeRefusesAdminAppWithoutForce(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"builtin-name", []string{"expose", "http://127.0.0.1:9000", "--as", "files"}, "files"},
		{"admin-port", []string{"expose", "tcp://127.0.0.1:22", "--as", "bastion"}, "22"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			et := newExposeTest(t)
			et.selectWorkspace(t, "skel")
			code, stdout, stderr := et.exec(t, tc.args...)
			if code == 0 {
				t.Fatalf("exposing an admin-grade app exited 0 (stdout %q)", stdout)
			}
			for _, want := range []string{tc.want, "--force", "nothing"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr %q does not carry %q", stderr, want)
				}
			}
			if len(et.apps.adds) != 0 {
				t.Errorf("a refused expose registered an app with Outpost: %+v", et.apps.adds)
			}
			if got := applyCalls(et.runner.calls); len(got) != 0 {
				t.Errorf("a refused expose applied to the plane: %v", got)
			}
		})
	}

	// --force is the operator's explicit override: the same expose lands.
	et := newExposeTest(t)
	et.selectWorkspace(t, "skel")
	code, _, stderr := et.exec(t, "expose", "http://127.0.0.1:9000", "--as", "files", "--force")
	if code != 0 {
		t.Fatalf("--force expose exited %d: %s", code, stderr)
	}
	if len(et.apps.adds) != 1 {
		t.Fatalf("--force expose registered %d apps, want 1", len(et.apps.adds))
	}
}

// expose applies exactly a Secret, a Deployment and a Service, all labelled
// dks.dhnt.io/expose=<name> and annotated with the expiry; the token lives
// ONLY in the Secret, and only its sha256 ever reaches outpost's argv.
func TestExposeAppliesLabelledSecretDeploymentServiceAndNoTokenInArgv(t *testing.T) {
	et := newExposeTest(t)
	et.selectWorkspace(t, "skel")

	code, stdout, stderr := et.exec(t, "expose", "http://127.0.0.1:9000", "--as", "web", "--ttl", "2h", "--env", "API={url}v1")
	if code != 0 {
		t.Fatalf("expose exited %d: %s", code, stderr)
	}

	// The Outpost host side learns the hash, never the token.
	if len(et.apps.adds) != 1 {
		t.Fatalf("outpost apps add called %d times, want 1", len(et.apps.adds))
	}
	spec := et.apps.adds[0]
	sum := sha256.Sum256([]byte(testExposeToken))
	if spec.TokenSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("registered hash = %q, want sha256 of the minted token", spec.TokenSHA256)
	}
	if spec.App != "web" || spec.Target != "http://127.0.0.1:9000" {
		t.Errorf("registered app = %+v, want app web target http://127.0.0.1:9000", spec)
	}
	if spec.Expires.IsZero() {
		t.Error("registered app carries no expiry: the TTL cut needs one")
	}
	// The DEFAULT host-side impl puts the hash — never the token — in argv.
	argv := execOutpostApps{}.addArgv(spec)
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, spec.TokenSHA256) {
		t.Errorf("outpost argv %q carries no --expose-token-sha256 value", joined)
	}
	if strings.Contains(joined, testExposeToken) {
		t.Fatalf("outpost argv LEAKS the token: %q", joined)
	}

	// Three objects, one per kind, every one labelled and annotated.
	objs := appliedObjects(t, et.runner.calls)
	for _, kind := range []string{"Secret", "Deployment", "Service"} {
		obj, ok := objs[kind]
		if !ok {
			t.Fatalf("expose applied no %s (applied %v)", kind, objs)
		}
		meta, _ := obj["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		if labels[exposeLabel] != "web" {
			t.Errorf("%s labels = %v, want %s=web", kind, labels, exposeLabel)
		}
		if labels[dagrun.LabelManagedBy] != dagrun.ManagedByBashy {
			t.Errorf("%s is not label-owned by bashy: %v", kind, labels)
		}
		annots, _ := meta["annotations"].(map[string]any)
		expires, _ := annots[exposeExpiresAnnot].(string)
		if _, err := time.Parse(time.RFC3339, expires); err != nil {
			t.Errorf("%s annotation %s = %q, want an RFC3339 instant", kind, exposeExpiresAnnot, expires)
		}
	}
	if name, _ := objs["Service"]["metadata"].(map[string]any)["name"].(string); name != "web" {
		t.Errorf("Service name = %q, want the expose name web (pods reach http://web:8080)", name)
	}

	// The token is in the Secret and nowhere else — not in another object,
	// not on stdout, not on stderr.
	secretRaw, _ := json.Marshal(objs["Secret"])
	if !strings.Contains(string(secretRaw), testExposeToken) {
		t.Errorf("the Secret does not carry %s", exposeTokenKey)
	}
	for kind, obj := range objs {
		if kind == "Secret" {
			continue
		}
		raw, _ := json.Marshal(obj)
		if strings.Contains(string(raw), testExposeToken) {
			t.Errorf("%s LEAKS the token: %s", kind, raw)
		}
	}
	if strings.Contains(stdout, testExposeToken) || strings.Contains(stderr, testExposeToken) {
		t.Error("expose printed the token")
	}

	// The relay reaches the app only through the matrix-tunnel app route.
	wantUpstream := testSatellite + "/matrix/h/" + testOutpostHost + "/app/web/"
	depRaw, _ := json.Marshal(objs["Deployment"])
	if !strings.Contains(string(depRaw), wantUpstream) {
		t.Errorf("Deployment does not point the relay at %q:\n%s", wantUpstream, depRaw)
	}
	if !strings.Contains(string(depRaw), `"relay"`) {
		t.Errorf("Deployment does not run 'dks relay':\n%s", depRaw)
	}
}

// unexpose removes the app from Outpost and deletes ONLY the objects the
// expose label selects — never a workspace-wide or unlabelled delete.
func TestUnexposeDeletesOnlyLabelled(t *testing.T) {
	et := newExposeTest(t)
	et.selectWorkspace(t, "skel")

	code, _, stderr := et.exec(t, "unexpose", "web")
	if code != 0 {
		t.Fatalf("unexpose exited %d: %s", code, stderr)
	}
	if len(et.apps.rms) != 1 || et.apps.rms[0] != "web" {
		t.Errorf("outpost apps rm calls = %v, want exactly [web]", et.apps.rms)
	}
	var deletes []runnerCall
	for _, c := range et.runner.calls {
		if action(c.argv) == "delete" {
			deletes = append(deletes, c)
		}
	}
	if len(deletes) != 1 {
		t.Fatalf("delete calls = %d, want exactly 1", len(deletes))
	}
	argv := strings.Join(deletes[0].argv, " ")
	wantSel := dagrun.LabelManagedBy + "=" + dagrun.ManagedByBashy + "," + exposeLabel + "=web"
	if !strings.Contains(argv, wantSel) {
		t.Errorf("delete argv %q does not select %q", argv, wantSel)
	}
	if strings.Contains(argv, "--all") {
		t.Errorf("delete argv %q is not label-scoped", argv)
	}
	for _, kind := range []string{"secrets", "deployments", "services"} {
		if !strings.Contains(argv, kind) {
			t.Errorf("delete argv %q does not remove %s", argv, kind)
		}
	}
	// Nothing of another expose's, and no run objects, may be in scope.
	if strings.Contains(argv, dagrun.LabelRun+"=") || strings.Contains(argv, "jobs") || strings.Contains(argv, "pods") {
		t.Errorf("delete argv %q reaches beyond the expose's own objects", argv)
	}
}

// exposed lists the labelled objects as NAME KIND TARGET EXPIRES STATE, with
// an expired expose reported expired rather than hidden.
func TestExposedListsLiveAndExpired(t *testing.T) {
	et := newExposeTest(t)
	et.selectWorkspace(t, "skel")
	now := et.renv.now()
	et.renv.listExposes = func(context.Context, dagrun.Context) ([]exposeRecord, error) {
		return []exposeRecord{
			{Name: "web", Kind: exposeKindHTTP, Target: "http://127.0.0.1:9000", Expires: now.Add(time.Hour)},
			{Name: "db", Kind: exposeKindTCP, Target: "tcp://127.0.0.1:5432", Port: 5432, Expires: now.Add(-time.Minute)},
		}, nil
	}
	code, stdout, stderr := et.exec(t, "exposed")
	if code != 0 {
		t.Fatalf("exposed exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "NAME") || !strings.Contains(stdout, "STATE") {
		t.Errorf("exposed printed no header:\n%s", stdout)
	}
	for _, want := range []string{"web", "http", "live", "db", "tcp", "expired"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("exposed output %q misses %q", stdout, want)
		}
	}
}

// The HTTP relay carries BOTH headers upstream (the expose token and the
// Bearer shape the satellite edge needs) and refuses to serve at all once
// the expiry has passed.
func TestRelayHTTPAddsTokenHeadersAndRefusesAfterExpiry(t *testing.T) {
	var gotToken, gotAuth, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get(exposeTokenHeader)
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()

	h, err := relayHTTPHandler(upstream.URL+"/matrix/h/host-a/app/web/", testExposeToken, "/k/door/")
	if err != nil {
		t.Fatalf("relayHTTPHandler: %v", err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	resp, err := http.Get(front.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("GET through the relay: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("relay answered %d %q", resp.StatusCode, body)
	}
	if gotToken != testExposeToken {
		t.Errorf("upstream %s = %q, want the expose token", exposeTokenHeader, gotToken)
	}
	if gotAuth != "Bearer "+testExposeToken {
		t.Errorf("upstream Authorization = %q, want a Bearer-shaped expose token", gotAuth)
	}
	wantPath := "/matrix/h/host-a/app/web/k/door/v1/messages"
	if gotPath != wantPath {
		t.Errorf("upstream path = %q, want %q", gotPath, wantPath)
	}

	// Past its expiry the relay runs NOTHING and exits non-zero: the TTL cut
	// is immediate, it never waits for a request to refuse.
	et := newExposeTest(t)
	t.Setenv("EXPOSE_TOKEN", testExposeToken)
	code, stdout, stderr := et.exec(t, "relay", "--mode", "http",
		"--upstream", upstream.URL+"/matrix/h/host-a/app/web/",
		"--expires", et.renv.now().Add(-time.Second).UTC().Format(time.RFC3339))
	if code == 0 {
		t.Fatalf("an expired relay exited 0 (stdout %q)", stdout)
	}
	if !strings.Contains(stderr, "expired") {
		t.Errorf("stderr %q does not say the expose expired", stderr)
	}
	if strings.Contains(stdout, testExposeToken) || strings.Contains(stderr, testExposeToken) {
		t.Error("the relay printed its token")
	}
}

// The TCP relay bridges each accepted connection over one WebSocket upgrade
// of the app route, carrying both headers, byte for byte in both directions.
func TestRelayTCPBridgesOverWebSocket(t *testing.T) {
	var gotToken, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get(exposeTokenHeader)
		gotAuth = r.Header.Get("Authorization")
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("websocket accept: %v", err)
			return
		}
		defer c.CloseNow()
		nc := websocket.NetConn(r.Context(), c, websocket.MessageBinary)
		buf := make([]byte, 4)
		if _, err := io.ReadFull(nc, buf); err != nil {
			return
		}
		_, _ = nc.Write([]byte(strings.ToUpper(string(buf))))
		// Hold the upgrade open until the client has read the answer.
		time.Sleep(50 * time.Millisecond)
	}))
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- relayTCPServe(ctx, ln, srv.URL+"/matrix/h/host-a/app/db/", testExposeToken) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial the relay: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	back := make([]byte, 4)
	if _, err := io.ReadFull(conn, back); err != nil {
		t.Fatalf("read back through the bridge: %v", err)
	}
	if string(back) != "PING" {
		t.Errorf("bridged bytes = %q, want %q", back, "PING")
	}
	if gotToken != testExposeToken {
		t.Errorf("upgrade %s = %q, want the expose token", exposeTokenHeader, gotToken)
	}
	if gotAuth != "Bearer "+testExposeToken {
		t.Errorf("upgrade Authorization = %q, want a Bearer-shaped expose token", gotAuth)
	}
	cancel()
	_ = ln.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("relayTCPServe did not return after its context was cancelled")
	}
}

// Every LIVE expose of the workspace reaches a run as environment: a URL for
// an http expose, an address for a tcp one, plus the recorded --env entries
// with {url} substituted. An expired expose reaches nothing.
func TestRunInjectsExposeEnv(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	now := rt.renv.now()
	rt.renv.listExposes = func(context.Context, dagrun.Context) ([]exposeRecord, error) {
		return []exposeRecord{
			{Name: "web", Kind: exposeKindHTTP, Target: "http://127.0.0.1:9000", Expires: now.Add(time.Hour),
				Env: []string{"ANTHROPIC_BASE_URL={url}v1"}},
			{Name: "code-db", Kind: exposeKindTCP, Target: "tcp://127.0.0.1:5432", Port: 5432, Expires: now.Add(time.Hour)},
			{Name: "old", Kind: exposeKindHTTP, Target: "http://127.0.0.1:9100", Expires: now.Add(-time.Second)},
		}, nil
	}

	code, stdout, stderr := rt.exec(t, "run", "--emit", "manifest", "--", "echo", "hi")
	if code != 0 {
		t.Fatalf("run exited %d: %s", code, stderr)
	}
	m, err := dagrun.ParseManifest([]byte(stdout))
	if err != nil {
		t.Fatalf("parse the emitted manifest: %v", err)
	}
	got := map[string]string{}
	for _, kv := range m.Task.Env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for k, want := range map[string]string{
		"DKS_WEB_URL":        "http://web:8080",
		"ANTHROPIC_BASE_URL": "http://web:8080v1",
		"DKS_CODE_DB_ADDR":   "code-db:5432",
	} {
		if got[k] != want {
			t.Errorf("manifest env %s = %q, want %q", k, got[k], want)
		}
	}
	if _, ok := got["DKS_OLD_URL"]; ok {
		t.Error("an EXPIRED expose reached the run's environment")
	}
}

// relayHTTPHandler refuses an upstream that is not an http(s) URL rather
// than proxying somewhere unexpected.
func TestRelayHTTPHandlerRefusesBadUpstream(t *testing.T) {
	if _, err := relayHTTPHandler("ftp://example.com/", testExposeToken, ""); err == nil {
		t.Error("relayHTTPHandler accepted a non-http upstream")
	}
	if _, err := url.Parse("http://example.com/"); err != nil {
		t.Fatal(err)
	}
}
