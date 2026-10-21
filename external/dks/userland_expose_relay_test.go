package dks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dagrun"
)

// relayChildPortsEnv tells the helper child which loopback ports its parent
// claims the relays are listening on. Unset, the helper is not the child.
const relayChildPortsEnv = "DKS_RELAY_TEST_CHILD_PORTS"

// relayChildExitCode is the code the helper child exits with once every
// relay answered: an arbitrary non-zero value, so the parent asserting it
// proves both that the child ran AND that its code (not the wrapper's) is
// what the process exits with.
const relayChildExitCode = 7

// relayChildGetEnv asks the helper child to GET through one relay port and
// require the body "ok", so the assertion runs WHILE the relays are up.
const relayChildGetEnv = "DKS_RELAY_TEST_CHILD_GET"

// TestRelayAllChild is NOT a test of its own: it is the child process the
// `relay --all` tests run through the wrapper. It dials every port its
// parent planned — and optionally GETs through one of them — then exits
// relayChildExitCode when all of it answered and 9 when it did not. Doing
// the checks in the CHILD is the point: a wrapper that execs before its
// relays listen fails here loudly rather than flaking.
func TestRelayAllChild(t *testing.T) {
	ports := strings.TrimSpace(os.Getenv(relayChildPortsEnv))
	if ports == "" {
		t.Skip("not the 'relay --all' helper child")
	}
	fmt.Fprint(os.Stdout, "child-ran")
	for _, p := range strings.Split(ports, ",") {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+p, 5*time.Second)
		if err != nil {
			fmt.Fprintf(os.Stderr, "child: relay port %s is not listening: %v\n", p, err)
			os.Exit(9)
		}
		_ = c.Close()
	}
	if p := strings.TrimSpace(os.Getenv(relayChildGetEnv)); p != "" {
		resp, err := http.Get("http://127.0.0.1:" + p + "/v1/messages")
		if err != nil {
			fmt.Fprintf(os.Stderr, "child: GET through relay %s: %v\n", p, err)
			os.Exit(9)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "ok" {
			fmt.Fprintf(os.Stderr, "child: GET through relay %s answered %d %q\n", p, resp.StatusCode, body)
			os.Exit(9)
		}
	}
	os.Exit(relayChildExitCode)
}

// helperChildArgv re-execs this test binary as the helper child.
func helperChildArgv(test string) []string {
	return []string{os.Args[0], "-test.run=^" + test + "$"}
}

// RED (S487.2): `dks relay --all -- CMD` is the run pod's PID 1. It reads
// $DKS_RELAYS, binds EVERY relay on 127.0.0.1 before anything runs, then
// runs CMD as a child with this process's stdio and exits with the child's
// code. A relay already past its expiry is reported and skipped rather than
// failing the run — the run is not the expose.
func TestRelayAllStartsListenersThenRunsChildAndPropagatesExit(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(exposeTokenHeader) != testExposeToken {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer app.Close()

	ports := freeLoopbackPorts(t, 3)
	relays := []dagrun.Relay{
		{Name: "code-db", Mode: dagrun.RelayModeTCP, Upstream: app.URL + "/matrix/h/host-a/app/code-db/", LocalPort: ports[1]},
		{Name: "old", Mode: dagrun.RelayModeHTTP, Upstream: app.URL + "/matrix/h/host-a/app/old/", LocalPort: ports[2],
			Expires: time.UnixMilli(1_730_000_000_000).UTC().Format(time.RFC3339)},
		{Name: "web", Mode: dagrun.RelayModeHTTP, Upstream: app.URL + "/matrix/h/host-a/app/web/", LocalPort: ports[0]},
	}
	spec, err := json.Marshal(relays)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(dagrun.RelaysEnv, string(spec))
	for _, r := range relays {
		t.Setenv(r.TokenEnv(), testExposeToken)
	}
	// The child is told the two LIVE ports only: the expired relay must not
	// be listening at all.
	t.Setenv(relayChildPortsEnv, strconv.Itoa(ports[0])+","+strconv.Itoa(ports[1]))

	et := newExposeTest(t)
	code, stdout, stderr := et.exec(t, append([]string{"relay", "--all", "--"}, helperChildArgv("TestRelayAllChild")...)...)
	if code != relayChildExitCode {
		t.Fatalf("relay --all exited %d, want the child's %d\nstdout %q\nstderr %q", code, relayChildExitCode, stdout, stderr)
	}
	if !strings.Contains(stdout, "child-ran") {
		t.Errorf("the child's stdout did not come through: %q", stdout)
	}
	if !strings.Contains(stderr, "old") || !strings.Contains(stderr, "expired") {
		t.Errorf("stderr %q does not report the expired relay", stderr)
	}
	if strings.Contains(stdout, testExposeToken) || strings.Contains(stderr, testExposeToken) {
		t.Error("relay --all printed a token")
	}
	// The expired relay never bound: the port is still free.
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(ports[2]))
	if err != nil {
		t.Errorf("the EXPIRED relay is holding %d: %v", ports[2], err)
	} else {
		_ = ln.Close()
	}
	// Every listener is closed again once the child is gone.
	for _, p := range ports[:2] {
		l2, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err != nil {
			t.Errorf("relay --all left %d bound after the child exited: %v", p, err)
			continue
		}
		_ = l2.Close()
	}
}

// RED (S487.2): the http relay a `--all` run started really does front the
// app route, token header and all — the wrapper is wiring, not a stub. The
// request is made by the CHILD, so it happens while the relay is up.
func TestRelayAllHTTPRelayReachesTheAppRoute(t *testing.T) {
	var gotToken, gotAuth, gotPath string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get(exposeTokenHeader)
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		fmt.Fprint(w, "ok")
	}))
	defer app.Close()

	port := freeLoopbackPorts(t, 1)[0]
	relays := []dagrun.Relay{{
		Name: "web", Mode: dagrun.RelayModeHTTP,
		Upstream: app.URL + "/matrix/h/host-a/app/web/", LocalPort: port,
	}}
	spec, err := json.Marshal(relays)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(dagrun.RelaysEnv, string(spec))
	t.Setenv(relays[0].TokenEnv(), testExposeToken)
	t.Setenv(relayChildPortsEnv, strconv.Itoa(port))
	t.Setenv(relayChildGetEnv, strconv.Itoa(port))

	et := newExposeTest(t)
	code, stdout, stderr := et.exec(t, append([]string{"relay", "--all", "--"}, helperChildArgv("TestRelayAllChild")...)...)
	if code != relayChildExitCode {
		t.Fatalf("relay --all exited %d, want the child's %d\nstdout %q\nstderr %q", code, relayChildExitCode, stdout, stderr)
	}
	if gotToken != testExposeToken {
		t.Errorf("app route %s = %q, want the expose token", exposeTokenHeader, gotToken)
	}
	if gotAuth != "Bearer "+testExposeToken {
		t.Errorf("app route Authorization = %q, want a Bearer-shaped expose token", gotAuth)
	}
	if want := "/matrix/h/host-a/app/web/v1/messages"; gotPath != want {
		t.Errorf("app route path = %q, want %q", gotPath, want)
	}
}

// RED (S487.2): --all fails CLOSED on anything that would leave the pod with
// a silently missing service — and when it fails, the command never runs.
func TestRelayAllFailsClosedWithoutRunningTheCommand(t *testing.T) {
	port := freeLoopbackPorts(t, 1)[0]
	good := []dagrun.Relay{{
		Name: "web", Mode: dagrun.RelayModeHTTP,
		Upstream: "https://sat.example.invalid/matrix/h/host-a/app/web/", LocalPort: port,
	}}
	goodJSON, err := json.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		relays string
		token  bool
		want   string
	}{
		{"no-list", "", true, dagrun.RelaysEnv},
		{"not-json", "{{", true, dagrun.RelaysEnv},
		{"bad-relay", `[{"name":"web","mode":"socks","upstream":"https://x/","local_port":1}]`, true, "mode"},
		{"no-token", string(goodJSON), false, good[0].TokenEnv()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(dagrun.RelaysEnv, tc.relays)
			if tc.token {
				t.Setenv(good[0].TokenEnv(), testExposeToken)
			} else {
				t.Setenv(good[0].TokenEnv(), "")
			}
			// The child, had it run, would exit 0: a non-zero exit is proof
			// the command never ran.
			t.Setenv(relayChildPortsEnv, "")
			t.Setenv(relayChildGetEnv, "")
			et := newExposeTest(t)
			code, stdout, stderr := et.exec(t, append([]string{"relay", "--all", "--"}, helperChildArgv("TestRelayAllChild")...)...)
			if code == 0 {
				t.Fatalf("relay --all exited 0 (stdout %q)", stdout)
			}
			if strings.Contains(stdout, "child-ran") {
				t.Errorf("the command ran anyway: %q", stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q does not name %q", stderr, tc.want)
			}
		})
	}

	// --all with nothing after -- has no command to wrap.
	t.Setenv(dagrun.RelaysEnv, string(goodJSON))
	et := newExposeTest(t)
	code, _, stderr := et.exec(t, "relay", "--all")
	if code == 0 || !strings.Contains(stderr, "--") {
		t.Errorf("relay --all with no command exited %d: %q", code, stderr)
	}
}

// RED (S487.2): `dks run` plans a deterministic loopback port per LIVE
// expose and injects both halves off that ONE plan — the manifest relay
// list the pod's wrapper reads, and the environment that names the very
// port each relay listens on. An expired expose contributes neither.
func TestRunAssignsDeterministicLocalPortsAndEnv(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	now := rt.renv.now()
	appURL := testAppURL
	// Deliberately NOT in name order: the plan sorts, the caller need not.
	recs := []exposeRecord{
		{Name: "web", Kind: exposeKindHTTP, Target: "http://127.0.0.1:9000", Port: exposeHTTPPort,
			AppURL: appURL("web"), Expires: now.Add(time.Hour), Env: []string{"ANTHROPIC_BASE_URL={url}v1"}},
		{Name: "proxy", Kind: exposeEgressKind, Target: "a.example.com", Port: exposeEgressPort,
			AppURL: appURL("proxy"), Expires: now.Add(time.Hour)},
		{Name: "code-db", Kind: exposeKindTCP, Target: "tcp://127.0.0.1:5432", Port: 5432,
			AppURL: appURL("code-db"), Expires: now.Add(time.Hour)},
		{Name: "gitproxy", Kind: exposeGitKind, Target: "octo/*", Port: exposeHTTPPort,
			Upstream: defaultGitUpstream, AppURL: appURL("gitproxy"), Expires: now.Add(time.Hour)},
		{Name: "zeta", Kind: exposeKindHTTP, Target: "http://127.0.0.1:9100", Port: exposeHTTPPort,
			AppURL: appURL("zeta"), Expires: now.Add(time.Hour)},
		{Name: "old", Kind: exposeKindHTTP, Target: "http://127.0.0.1:9200", Port: exposeHTTPPort,
			AppURL: appURL("old"), Expires: now.Add(-time.Second)},
	}
	rt.renv.listExposes = func(context.Context, dagrun.Context) ([]exposeRecord, error) { return recs, nil }

	code, stdout, stderr := rt.exec(t, "run", "--emit", "manifest", "--", "echo", "hi")
	if code != 0 {
		t.Fatalf("run exited %d: %s", code, stderr)
	}
	m, err := dagrun.ParseManifest([]byte(stdout))
	if err != nil {
		t.Fatalf("parse the emitted manifest: %v", err)
	}

	// The plan: a tcp expose keeps its own target port, egress takes the
	// 3128 every proxy-aware tool expects, and http/git fill 18080 upward
	// in NAME order around them.
	want := []dagrun.Relay{
		{Name: "code-db", Mode: dagrun.RelayModeTCP, Upstream: appURL("code-db"), LocalPort: 5432},
		{Name: "gitproxy", Mode: dagrun.RelayModeHTTP, Upstream: appURL("gitproxy"), LocalPort: 18080},
		{Name: "proxy", Mode: dagrun.RelayModeEgress, Upstream: appURL("proxy"), LocalPort: exposeEgressPort},
		{Name: "web", Mode: dagrun.RelayModeHTTP, Upstream: appURL("web"), LocalPort: 18081},
		{Name: "zeta", Mode: dagrun.RelayModeHTTP, Upstream: appURL("zeta"), LocalPort: 18082},
	}
	if len(m.Meta.Relays) != len(want) {
		t.Fatalf("manifest relays = %+v, want %d of them", m.Meta.Relays, len(want))
	}
	for i, w := range want {
		got := m.Meta.Relays[i]
		if got.Name != w.Name || got.Mode != w.Mode || got.Upstream != w.Upstream || got.LocalPort != w.LocalPort {
			t.Errorf("relay %d = %+v, want %+v", i, got, w)
		}
		if got.Expires == "" {
			t.Errorf("relay %s carries no expiry: the TTL cut needs one", got.Name)
		}
	}
	for _, r := range m.Meta.Relays {
		if r.Name == "old" {
			t.Error("an EXPIRED expose was given a relay")
		}
	}

	// The environment names the very ports the relay list listens on.
	envGot := map[string]string{}
	for _, kv := range m.Task.Env {
		k, v, _ := strings.Cut(kv, "=")
		envGot[k] = v
	}
	for k, w := range map[string]string{
		"DKS_CODE_DB_ADDR":   "127.0.0.1:5432",
		"DKS_GITPROXY_URL":   "http://127.0.0.1:18080",
		"DKS_PROXY_URL":      "http://127.0.0.1:3128",
		"DKS_WEB_URL":        "http://127.0.0.1:18081",
		"DKS_ZETA_URL":       "http://127.0.0.1:18082",
		"ANTHROPIC_BASE_URL": "http://127.0.0.1:18081v1",
		"HTTPS_PROXY":        "http://127.0.0.1:3128",
		"HTTP_PROXY":         "http://127.0.0.1:3128",
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "url.http://127.0.0.1:18080/.insteadOf",
		"GIT_CONFIG_VALUE_0": defaultGitUpstream + "/",
	} {
		if envGot[k] != w {
			t.Errorf("manifest env %s = %q, want %q", k, envGot[k], w)
		}
	}
	if _, ok := envGot["DKS_OLD_URL"]; ok {
		t.Error("an EXPIRED expose reached the run's environment")
	}

	// Deterministic: the same exposes always plan the same ports.
	rt2 := newRunTest(t)
	rt2.selectWorkspace(t, "skel")
	rt2.renv.listExposes = func(context.Context, dagrun.Context) ([]exposeRecord, error) {
		return append([]exposeRecord(nil), recs[len(recs)-1], recs[0], recs[3], recs[2], recs[4], recs[1]), nil
	}
	code, stdout2, stderr := rt2.exec(t, "run", "--emit", "manifest", "--", "echo", "hi")
	if code != 0 {
		t.Fatalf("second run exited %d: %s", code, stderr)
	}
	if stdout2 != stdout {
		t.Errorf("the relay plan is not deterministic:\n--- a\n%s--- b\n%s", stdout, stdout2)
	}
}

// RED (S487.2): end to end, a run with live exposes renders a pod whose PID
// 1 is the relay wrapper and whose tokens arrive by secretKeyRef out of the
// exposes' own Secrets — the pod never sees a token in argv or a literal.
func TestRunRendersRelayWrapperAndSecretRefs(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	now := rt.renv.now()
	rt.renv.listExposes = func(context.Context, dagrun.Context) ([]exposeRecord, error) {
		return []exposeRecord{{
			Name: "web", Kind: exposeKindHTTP, Target: "http://127.0.0.1:9000", Port: exposeHTTPPort,
			AppURL:  testSatellite + "/matrix/h/" + testOutpostHost + "/app/web/",
			Expires: now.Add(time.Hour),
		}}, nil
	}
	code, stdout, stderr := rt.exec(t, "run", "--emit", "yaml", "--", "echo", "hi")
	if code != 0 {
		t.Fatalf("run --emit yaml exited %d: %s", code, stderr)
	}
	var list struct {
		Items []struct {
			Kind string `json:"kind"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Command []string `json:"command"`
							Env     []struct {
								Name      string `json:"name"`
								Value     string `json:"value"`
								ValueFrom *struct {
									SecretKeyRef struct {
										Name string `json:"name"`
										Key  string `json:"key"`
									} `json:"secretKeyRef"`
								} `json:"valueFrom"`
							} `json:"env"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatalf("parse the rendered objects: %v\n%s", err, stdout)
	}
	if len(list.Items) == 0 || len(list.Items[0].Spec.Template.Spec.Containers) == 0 {
		t.Fatalf("no rendered container:\n%s", stdout)
	}
	c := list.Items[0].Spec.Template.Spec.Containers[0]
	if got := strings.Join(c.Command, " "); !strings.HasPrefix(got, "bashy dks relay --all -- ") {
		t.Errorf("container command = %q, want the relay wrapper first", got)
	}
	var relaysJSON string
	var ref [2]string
	for _, e := range c.Env {
		switch {
		case e.Name == dagrun.RelaysEnv:
			relaysJSON = e.Value
		case e.Name == dagrun.RelayTokenEnvName("web"):
			if e.ValueFrom == nil {
				t.Fatalf("$%s is the literal %q, not a secretKeyRef", e.Name, e.Value)
			}
			ref = [2]string{e.ValueFrom.SecretKeyRef.Name, e.ValueFrom.SecretKeyRef.Key}
		}
	}
	if !strings.Contains(relaysJSON, `"local_port":18080`) {
		t.Errorf("$%s = %q, want the planned port", dagrun.RelaysEnv, relaysJSON)
	}
	if ref != [2]string{dagrun.ExposeSecretName("web"), exposeTokenKey} {
		t.Errorf("token env wired to %v, want {%s %s}", ref, dagrun.ExposeSecretName("web"), exposeTokenKey)
	}
}
