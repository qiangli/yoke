package dks

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/qiangli/yoke/pkg/dagrun"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// RED (S487.3, reshaped in S487.2): proxy:egress requires --allow, registers
// egress:// with the allow list, and records port 3128 on its Secret — the
// port the run pod's own `relay --all` serves the CONNECT proxy on. There is
// no relay Deployment and no Service any more.
func TestExposeEgressRequiresAllowAndPassesItToOutpost(t *testing.T) {
	// Without --allow: refused, nothing ran.
	et := newExposeTest(t)
	et.selectWorkspace(t, "skel")
	code, _, stderr := et.exec(t, "expose", "proxy:egress", "--as", "proxy")
	if code == 0 {
		t.Fatal("expose proxy:egress without --allow exited 0")
	}
	for _, want := range []string{"--allow", "nothing ran"} {
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

	// With --allow: lands on Outpost as egress:// with the list.
	et2 := newExposeTest(t)
	et2.selectWorkspace(t, "skel")
	code, _, stderr = et2.exec(t, "expose", "proxy:egress", "--as", "proxy",
		"--allow", "a.example.com,*.example.org")
	if code != 0 {
		t.Fatalf("expose proxy:egress exited %d: %s", code, stderr)
	}
	if len(et2.apps.adds) != 1 {
		t.Fatalf("outpost apps add called %d times, want 1", len(et2.apps.adds))
	}
	spec := et2.apps.adds[0]
	if spec.Target != "egress://" {
		t.Errorf("registered target = %q, want %q", spec.Target, "egress://")
	}
	argv := strings.Join(execOutpostApps{}.addArgv(spec), " ")
	if !strings.Contains(argv, "--allow") || !strings.Contains(argv, "a.example.com") || !strings.Contains(argv, "*.example.org") {
		t.Errorf("outpost argv %q carries no --allow list", argv)
	}
	if strings.Contains(argv, testExposeToken) {
		t.Fatal("outpost argv leaks the token")
	}

	objs := appliedObjects(t, et2.runner.calls)
	if len(objs) != 1 {
		t.Fatalf("expose applied %d kinds, want exactly the Secret: %v", len(objs), objs)
	}
	for _, gone := range []string{"Deployment", "Service"} {
		if _, ok := objs[gone]; ok {
			t.Errorf("an egress expose still applies a %s", gone)
		}
	}
	annots := exposeAnnotations(t, objs["Secret"])
	for annot, want := range map[string]string{
		exposeKindAnnot:   exposeEgressKind,
		exposeAllowAnnot:  "a.example.com,*.example.org",
		exposePortAnnot:   "3128",
		exposeAppURLAnnot: testSatellite + "/matrix/h/" + testOutpostHost + "/app/proxy/",
	} {
		if got := annots[annot]; got != want {
			t.Errorf("Secret annotation %s = %q, want %q", annot, got, want)
		}
	}
}

// exposeAnnotations digs the record annotations out of an applied expose
// Secret.
func exposeAnnotations(t *testing.T, obj map[string]any) map[string]string {
	t.Helper()
	meta, _ := obj["metadata"].(map[string]any)
	raw, _ := meta["annotations"].(map[string]any)
	if len(raw) == 0 {
		t.Fatalf("expose Secret carries no annotations:\n%s", mustJSON(t, obj))
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		s, _ := v.(string)
		out[k] = s
	}
	return out
}

// RED (S487.3): relay --mode egress bridges CONNECT over a ?dst= websocket,
// refuses policy-closed destinations with 403, and rejects plain HTTP with 405.
func TestRelayEgressConnectBridgesAndRefusesPolicyClose(t *testing.T) {
	const allowed = "allowed.example.com:443"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(exposeTokenHeader) != testExposeToken {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("dst") != allowed {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			_ = c.Close(websocket.StatusPolicyViolation, "not allowed")
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		nc := websocket.NetConn(r.Context(), c, websocket.MessageBinary)
		buf := make([]byte, 4)
		if _, err := io.ReadFull(nc, buf); err != nil {
			return
		}
		_, _ = nc.Write([]byte(strings.ToUpper(string(buf))))
		time.Sleep(50 * time.Millisecond)
	}))
	defer srv.Close()

	upstream := srv.URL + "/app/proxy/"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- relayEgressServe(ctx, ln, upstream, testExposeToken) }()
	defer func() {
		cancel()
		_ = ln.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("relayEgressServe did not return after cancel")
		}
	}()

	dialRelay := func() (net.Conn, *bufio.Reader) {
		t.Helper()
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial the egress relay: %v", err)
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		return c, bufio.NewReader(c)
	}

	// Allowed CONNECT bridges bytes.
	c1, br1 := dialRelay()
	defer c1.Close()
	if _, err := io.WriteString(c1, "CONNECT "+allowed+" HTTP/1.1\r\nHost: "+allowed+"\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	line, err := br1.ReadString('\n')
	if err != nil {
		t.Fatalf("read CONNECT reply: %v", err)
	}
	if !strings.Contains(line, "200") {
		t.Fatalf("allowed CONNECT reply = %q, want 200", line)
	}
	// Drain the 200 headers.
	for {
		l, err := br1.ReadString('\n')
		if err != nil {
			t.Fatalf("read 200 headers: %v", err)
		}
		if l == "\r\n" || l == "\n" {
			break
		}
	}
	if _, err := c1.Write([]byte("ping")); err != nil {
		t.Fatalf("write through the bridge: %v", err)
	}
	back := make([]byte, 4)
	if _, err := io.ReadFull(br1, back); err != nil {
		t.Fatalf("read back through the bridge: %v", err)
	}
	if string(back) != "PING" {
		t.Errorf("bridged bytes = %q, want %q", back, "PING")
	}

	// Policy-closed destination replies 403 before any 200.
	c2, br2 := dialRelay()
	defer c2.Close()
	if _, err := io.WriteString(c2, "CONNECT evil.example.com:443 HTTP/1.1\r\nHost: evil.example.com:443\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	line, err = br2.ReadString('\n')
	if err != nil {
		t.Fatalf("read refused CONNECT reply: %v", err)
	}
	if !strings.Contains(line, "403") {
		t.Errorf("refused CONNECT reply = %q, want 403", line)
	}

	// Plain-HTTP absolute-URI requests are not proxied: 405 with an https hint.
	c3, br3 := dialRelay()
	defer c3.Close()
	if _, err := io.WriteString(c3, "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n"); err != nil {
		t.Fatalf("write GET: %v", err)
	}
	line, err = br3.ReadString('\n')
	if err != nil {
		t.Fatalf("read GET reply: %v", err)
	}
	if !strings.Contains(line, "405") {
		t.Errorf("plain-HTTP reply = %q, want 405", line)
	}
	rest, _ := io.ReadAll(br3)
	if !strings.Contains(strings.ToLower(string(rest)), "https") {
		t.Errorf("405 body %q carries no https hint", rest)
	}
}

// RED (S487.3, reshaped in S487.2): a live egress expose reaches runs as
// proxy env plus DKS_<NAME>_URL, all pointed at the LOOPBACK port the pod's
// own relay serves. Comma-free values ride the manifest Env line; the
// comma-separated NO_PROXY bypass list rides the run Secret under a
// dks-expose ref, so generation still succeeds and the pod still receives
// every variable.
func TestRunInjectsProxyEnvForEgress(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	now := rt.renv.now()
	recs := []exposeRecord{
		{Name: "proxy", Kind: "egress", Target: "a.example.com,*.example.org", Port: 3128, Expires: now.Add(time.Hour)},
		{Name: "old", Kind: "egress", Target: "a.example.com", Port: 3128, Expires: now.Add(-time.Second)},
	}
	got := exposeRunEnv(recs, now)
	for k, want := range map[string]string{
		"DKS_PROXY_URL": "http://127.0.0.1:3128",
		"HTTPS_PROXY":   "http://127.0.0.1:3128",
		"HTTP_PROXY":    "http://127.0.0.1:3128",
		"NO_PROXY":      "localhost,127.0.0.1,.svc,.cluster.local",
	} {
		if got[k] != want {
			t.Errorf("run env %s = %q, want %q", k, got[k], want)
		}
	}
	if _, ok := got["DKS_OLD_URL"]; ok {
		t.Error("an EXPIRED egress expose reached the run's environment")
	}

	// End to end through the manifest: a live egress expose must not fail
	// generation. The comma-free proxy addresses ride Env; NO_PROXY rides
	// a dks-expose Secrets ref to the run Secret.
	rt.renv.listExposes = func(context.Context, dagrun.Context) ([]exposeRecord, error) {
		return []exposeRecord{
			{Name: "proxy", Kind: "egress", Target: "a.example.com", Port: 3128, Expires: now.Add(time.Hour)},
		}, nil
	}
	code, stdout, stderr := rt.exec(t, "run", "--emit", "manifest", "--", "echo", "hi")
	if code != 0 {
		t.Fatalf("run with a live egress expose exited %d: %s", code, stderr)
	}
	m, err := dagrun.ParseManifest([]byte(stdout))
	if err != nil {
		t.Fatalf("parse the emitted manifest: %v", err)
	}
	envGot := map[string]string{}
	for _, kv := range m.Task.Env {
		k, v, _ := strings.Cut(kv, "=")
		envGot[k] = v
	}
	for k, want := range map[string]string{
		"DKS_PROXY_URL": "http://127.0.0.1:3128",
		"HTTPS_PROXY":   "http://127.0.0.1:3128",
		"HTTP_PROXY":    "http://127.0.0.1:3128",
	} {
		if envGot[k] != want {
			t.Errorf("manifest env %s = %q, want %q", k, envGot[k], want)
		}
	}
	foundRef := false
	for _, sec := range m.Task.Secrets {
		if sec == exposeSecretRef("NO_PROXY") {
			foundRef = true
		}
	}
	if !foundRef {
		t.Errorf("manifest secrets = %q, want a %q ref carrying NO_PROXY", m.Task.Secrets, exposeSecretRef("NO_PROXY"))
	}
	if strings.Contains(stdout, egressNoProxy) {
		t.Errorf("the manifest carries the bypass list inline, where the Env line cannot hold it")
	}

	// End to end through a real run: the bypass list rides the run Secret
	// and the pod receives it via secretKeyRef, commas intact.
	rt2 := newRunTest(t)
	rt2.selectWorkspace(t, "skel@k8s:dev")
	now2 := rt2.renv.now()
	rt2.renv.listExposes = func(context.Context, dagrun.Context) ([]exposeRecord, error) {
		return []exposeRecord{
			{Name: "proxy", Kind: "egress", Target: "a.example.com", Port: 3128, Expires: now2.Add(time.Hour)},
		}, nil
	}
	rt2.runner.gets = [][]byte{
		runsJSON(jobJSON(testWorkspaceID, testRunID, "echo", "Complete")),
	}
	code, _, stderr = rt2.exec(t, "run", "--", "echo", "hi")
	if code != 0 {
		t.Fatalf("run with a live egress expose exited %d: %s", code, stderr)
	}
	if len(rt2.runner.calls) < 2 {
		t.Fatalf("expected Secret and workload applies, got %d plane calls", len(rt2.runner.calls))
	}
	var secretObj map[string]any
	if err := json.Unmarshal(rt2.runner.calls[0].stdin, &secretObj); err != nil {
		t.Fatalf("parse Secret apply stdin: %v", err)
	}
	stringData, _ := secretObj["stringData"].(map[string]any)
	if stringData[egressNoProxyVar] != egressNoProxy {
		t.Errorf("Secret stringData[%s] = %v, want the bypass list", egressNoProxyVar, stringData[egressNoProxyVar])
	}
	var jobList map[string]any
	if err := json.Unmarshal(rt2.runner.calls[1].stdin, &jobList); err != nil {
		t.Fatalf("parse workload apply stdin: %v", err)
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
	if len(containers) == 0 {
		t.Fatal("workload containers empty")
	}
	container, _ := containers[0].(map[string]any)
	envList, _ := container["env"].([]any)
	wired := false
	for _, e := range envList {
		entry, _ := e.(map[string]any)
		if entry["name"] != egressNoProxyVar {
			continue
		}
		vf, _ := entry["valueFrom"].(map[string]any)
		skr, _ := vf["secretKeyRef"].(map[string]any)
		if skr["key"] == egressNoProxyVar {
			wired = true
		}
	}
	if !wired {
		t.Errorf("workload container env does not receive %s via secretKeyRef: %v", egressNoProxyVar, envList)
	}
}
