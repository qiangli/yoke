// userland_session_test.go — tests for the S481.6 session verbs: the hidden
// executor-side `session-server`, `attach` and `logs` over the attach
// transport (contract §4). Every test is hermetic: pods come from a fake
// Runner, the tunnel is an httptest attach server, HOME is a TempDir, and no
// test execs a binary or touches the network.
package dks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/dagrun"
	"github.com/qiangli/yoke/pkg/dagrun/attach"
	"github.com/qiangli/yoke/pkg/dagrun/cluster"
)

// sessionRunner is the argv-aware fake for the session verbs: `get pods`
// answers the session-pod lookup, `get jobs,pods` answers cluster.List, and
// anything else answers empty. It never execs and never networks.
type sessionRunner struct {
	calls    []runnerCall
	podsJSON []byte
	listJSON []byte
	runErr   error
}

func (r *sessionRunner) Run(_ context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error) {
	r.calls = append(r.calls, runnerCall{argv: append([]string(nil), argv...), stdin: append([]byte(nil), stdin...)})
	if r.runErr != nil {
		return nil, nil, 0, r.runErr
	}
	for _, a := range argv {
		if a == "jobs,pods" {
			if r.listJSON != nil {
				return r.listJSON, nil, 0, nil
			}
			return []byte(`{"items":[]}`), nil, 0, nil
		}
	}
	for _, a := range argv {
		if a == "pods" {
			if r.podsJSON != nil {
				return r.podsJSON, nil, 0, nil
			}
			return []byte(`{"items":[]}`), nil, 0, nil
		}
	}
	return []byte(`{}`), nil, 0, nil
}

func (r *sessionRunner) podLookups() int {
	n := 0
	for _, c := range r.calls {
		for _, a := range c.argv {
			if a == "pods" {
				n++
				break
			}
		}
	}
	return n
}

// sessionPodJSON is one session pod as `kubectl get pods -o json` reports
// it: the run label the verbs select by, the executor-host label, the
// attach-port annotation render writes, and a nodeName fallback host.
func sessionPodJSON(namespace, name, uid, host, port string) []byte {
	return []byte(fmt.Sprintf(`{"items":[{"metadata":{"name":%q,"namespace":%q,"uid":%q,`+
		`"labels":{"app.kubernetes.io/managed-by":"bashy","dhnt.io/workspace":%q,"dhnt.io/run":%q,"outpost.dhnt.io/host":%q},`+
		`"annotations":{"dhnt.io/attach-port":%q}},`+
		`"spec":{"nodeName":"node-fallback"},"status":{"phase":"Running"}}]}`,
		name, namespace, uid, testWorkspaceID, testRunID, host, port))
}

// stubTerminal is the no-op terminal behind the raw-mode interface: tests
// never touch a TTY.
type stubTerminal struct {
	cols, rows int
	sizeErr    error
	restores   int
}

func (s stubTerminal) MakeRaw() (func(), error) { return func() {}, nil }

func (s stubTerminal) Size() (int, int, error) {
	if s.sizeErr != nil {
		return 0, 0, s.sizeErr
	}
	return s.cols, s.rows, nil
}

// fakePTYHandle is the session-server's fake child: no process, no PTY, just
// a canned exit code and captured input.
type fakePTYHandle struct {
	code  int
	input []byte
}

func (f *fakePTYHandle) WriteInput(p []byte) (int, error) {
	f.input = append(f.input, p...)
	return len(p), nil
}

func (f *fakePTYHandle) Resize(_, _ uint16) error { return nil }

func (f *fakePTYHandle) Signal(_ string) error { return nil }

func (f *fakePTYHandle) Wait() (int, error) { return f.code, nil }

// scriptRead is one canned Read: bytes, then its error.
type scriptRead struct {
	data string
	err  error
}

// scriptStream is a stub attachStream: canned reads, counted reconnects, and
// a canned Wait outcome. It proves the pump reconnect loop without a socket.
type scriptStream struct {
	reads      []scriptRead
	i          int
	reconnects int
	callErr    error
	waitCode   int
	waitLost   bool
	waitErr    error
	writes     []byte
}

func (s *scriptStream) Read(p []byte) (int, error) {
	if s.i >= len(s.reads) {
		return 0, io.EOF
	}
	r := s.reads[s.i]
	s.i++
	if len(r.data) > 0 {
		n := copy(p, r.data)
		return n, r.err
	}
	return 0, r.err
}

func (s *scriptStream) Write(p []byte) error {
	if s.callErr != nil {
		return s.callErr
	}
	s.writes = append(s.writes, p...)
	return nil
}

func (s *scriptStream) Resize(_, _ uint16) error { return nil }

func (s *scriptStream) Signal(_ string) error { return nil }

func (s *scriptStream) Wait() (int, bool, error) { return s.waitCode, s.waitLost, s.waitErr }

func (s *scriptStream) Reconnect(_ context.Context) error {
	s.reconnects++
	return s.callErr
}

func (s *scriptStream) Offset() int64 { return 0 }

func (s *scriptStream) Close() error { return nil }

// sessionTest is the hermetic session environment: state under a TempDir
// via $BASHY_HOME, the argv-aware runner, stubbed tunnel/client/terminal
// seams, and a local-exec trap (the verbs never run locally).
type sessionTest struct {
	home       string
	runner     *sessionRunner
	senv       *sessionEnv
	localCalls []execCall
}

func newSessionTest(t *testing.T) *sessionTest {
	t.Helper()
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv("KUBECONFIG", filepath.Join(home, "no-kubeconfig"))
	st := &sessionTest{home: home, runner: &sessionRunner{}}
	uenv := &userlandEnv{
		stateDir:  func() string { return filepath.Join(home, "dks", "context") },
		newRunner: func() cluster.Runner { return st.runner },
		mintID:    func() string { return testRunID },
		execLocal: func(_ context.Context, name string, args []string) int {
			st.localCalls = append(st.localCalls, execCall{name: name, args: append([]string(nil), args...)})
			return 1
		},
	}
	st.senv = defaultSessionEnv(uenv)
	st.senv.tunnelBase = func(dagrun.Context) (string, error) {
		return "", errors.New("test tunnel base not stubbed")
	}
	st.senv.newClientID = func() string { return "test-client" }
	st.senv.terminal = func() sessionTerminal { return stubTerminal{cols: 80, rows: 24} }
	st.senv.now = func() time.Time { return time.UnixMilli(1_730_000_000_000) }
	return st
}

// selectWorkspace persists a selection the way `dks use` would.
func (st *sessionTest) selectWorkspace(t *testing.T, arg string) dagrun.Context {
	t.Helper()
	sel, err := dagrun.ParseSelection(arg)
	if err != nil {
		t.Fatal(err)
	}
	ctxRec, err := dagrun.NewContext(sel, testWorkspaceID, "~/docs")
	if err != nil {
		t.Fatal(err)
	}
	store, err := dagrun.OpenStore(st.senv.stateDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Select(ctxRec); err != nil {
		t.Fatal(err)
	}
	return ctxRec
}

func (st *sessionTest) exec(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	root := &cobra.Command{Use: "dks", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newSessionCmds(st.senv)...)
	root.SetArgs(args)
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader(""))
	return userlandExitCode(root.Execute()), outBuf.String(), errBuf.String()
}

func (st *sessionTest) noLocal(t *testing.T) {
	t.Helper()
	if len(st.localCalls) != 0 {
		t.Errorf("ran locally: %v", st.localCalls)
	}
}

// sessionCommandHub opens a Hub with an attach server standing in for the
// matrix tunnel: the verbs dial it through the stubbed tunnel base.
func sessionCommandHub(t *testing.T, id string) (*dagrun.Hub, string) {
	t.Helper()
	sess, err := dagrun.NewSession(id, testWorkspaceID, id, dagrun.KindInteractive, "exec-1", "linux", "amd64", "", time.Now())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	hub, err := dagrun.NewHub(t.TempDir(), sess, nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	return hub, id
}

// tunnelDial records one dialStream call: the endpoint and the viewer flag.
// The viewer query itself is the attach package's job; this pins that the
// verbs pass the flag through.
type tunnelDial struct {
	endpoint string
	viewer   bool
}

// attachTunnelServer serves hub as the tunnel endpoint and points the test
// env at it, recording every dial the verbs make.
func attachTunnelServer(t *testing.T, st *sessionTest, hub *dagrun.Hub) (*httptest.Server, *[]tunnelDial) {
	t.Helper()
	srv := httptest.NewServer(attach.Handler(hub))
	t.Cleanup(srv.Close)
	var dialed []tunnelDial
	st.senv.tunnelBase = func(dagrun.Context) (string, error) { return srv.URL, nil }
	realDial := st.senv.dialStream
	st.senv.dialStream = func(ctx context.Context, endpoint, clientID string, since int64, viewer bool) (attachStream, error) {
		dialed = append(dialed, tunnelDial{endpoint: endpoint, viewer: viewer})
		return realDial(ctx, endpoint, clientID, since, viewer)
	}
	return srv, &dialed
}

// wsDialURL converts an httptest server URL to its ws attach URL.
func wsDialURL(srv *httptest.Server, query string) string {
	return "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/attach" + query
}

// TestAttachVerbReplaysAndExitsWithSessionCode is the slice acceptance test:
// client A's stream drops mid-session, the executor records the original
// nonzero exit, and `attach` from a second client replays every byte from
// offset zero and exits with that same code. The tunnel is an httptest
// attach server; the pod comes from the fake Runner.
func TestAttachVerbReplaysAndExitsWithSessionCode(t *testing.T) {
	st := newSessionTest(t)
	st.selectWorkspace(t, "skel")
	hub, id := sessionCommandHub(t, testRunID)
	srv, dialed := attachTunnelServer(t, st, hub)
	podName := "bashy-run-" + strings.ToLower(testRunID)
	st.runner.podsJSON = sessionPodJSON("default", podName, "abcdef1234567890", "worker-a", "41234")

	// Client A: a raw dial that reads the first bytes, then drops without a
	// close frame — a dead terminal, not a detach.
	raw, _, err := websocket.DefaultDialer.Dial(wsDialURL(srv, "?client=a&since=0"), nil)
	if err != nil {
		t.Fatalf("client A dial: %v", err)
	}
	if _, err := hub.Append([]byte("hello ")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := readBinaryBytes(t, raw, len("hello ")); err != nil {
		t.Fatalf("client A read: %v", err)
	}
	_ = raw.UnderlyingConn().Close()
	if _, err := hub.Append([]byte("world")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := hub.RecordExit(3, time.Now()); err != nil {
		t.Fatalf("RecordExit: %v", err)
	}

	// Client B: the verb under test.
	code, stdout, stderr := st.exec(t, "attach", id)
	if code != 3 {
		t.Errorf("attach exited %d, want the session's exit code 3 (stderr %q)", code, stderr)
	}
	if stdout != "hello world" {
		t.Errorf("attach replayed %q, want %q", stdout, "hello world")
	}
	if !strings.Contains(stderr, "exited") || !strings.Contains(stderr, "3") {
		t.Errorf("attach stderr %q does not report the exit", stderr)
	}
	if len(*dialed) != 1 {
		t.Fatalf("dialed endpoints = %v, want exactly one tunnel dial", *dialed)
	}
	got := (*dialed)[0]
	wantURL := "/matrix/h/worker-a/app/vk-default-" + podName + "-abcdef12/attach"
	if !strings.Contains(got.endpoint, wantURL) {
		t.Errorf("tunnel endpoint %q misses %q", got.endpoint, wantURL)
	}
	if got.viewer {
		t.Errorf("non-viewer attach dialled as viewer: %+v", got)
	}
	if st.runner.podLookups() == 0 {
		t.Error("attach never looked the pod up by label")
	}
	st.noLocal(t)
}

// readBinaryBytes reads binary frames until n bytes arrive or the deadline
// passes; control frames are skipped.
func readBinaryBytes(t *testing.T, ws *websocket.Conn, n int) error {
	t.Helper()
	var got []byte
	deadline := time.Now().Add(10 * time.Second)
	for len(got) < n {
		_ = ws.SetReadDeadline(deadline)
		typ, msg, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		if typ == websocket.BinaryMessage {
			got = append(got, msg...)
		}
	}
	return nil
}

// TestSessionServerRecordsExitOnce runs the hidden verb with a fake child
// that prints and exits 3: the process exits 3, the log holds the output,
// and the persisted record carries the exit exactly once.
func TestSessionServerRecordsExitOnce(t *testing.T) {
	st := newSessionTest(t)
	home := t.TempDir()
	sessID := testRunID
	var started []string
	var listened string
	st.senv.startPTY = func(argv []string, sink io.Writer, _ string) (ptyHandle, error) {
		started = append([]string(nil), argv...)
		if _, err := io.WriteString(sink, "hi"); err != nil {
			return nil, err
		}
		return &fakePTYHandle{code: 3}, nil
	}
	st.senv.listen = func(addr string) (net.Listener, error) {
		listened = addr
		return net.Listen("tcp", "127.0.0.1:0")
	}

	code, _, stderr := st.exec(t, "session-server",
		"--session", sessID, "--home", home, "--port", "41234",
		"--", "bashy", "-c", "echo hi")
	if code != 3 {
		t.Fatalf("session-server exited %d, want the child's 3 (stderr %q)", code, stderr)
	}
	if got, want := started, []string{"bashy", "-c", "echo hi"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("child argv = %q, want %q", got, want)
	}
	if !strings.HasSuffix(listened, ":41234") {
		t.Errorf("listen addr = %q, want the flagged port", listened)
	}

	// The record survives: reopening the session home sees the exit, and a
	// second exit is refused — the code was recorded exactly once.
	dir := filepath.Join(home, ".dks", "sessions", sessID)
	sess, err := dagrun.NewSession(sessID, testWorkspaceID, sessID, dagrun.KindInteractive, "exec-1", "linux", "amd64", "", time.Now())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	hub, err := dagrun.NewHub(dir, sess, nil)
	if err != nil {
		t.Fatalf("reopen Hub: %v", err)
	}
	defer hub.Close()
	snap := hub.Session()
	if snap.State != dagrun.StateExited {
		t.Fatalf("reopened state = %q, want exited", snap.State)
	}
	if snap.ExitCode == nil || *snap.ExitCode != 3 {
		t.Fatalf("reopened exit = %v, want 3", snap.ExitCode)
	}
	body, _, err := hub.ReadFrom(0)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if string(body) != "hi" {
		t.Errorf("session log = %q, want %q", body, "hi")
	}
	if err := hub.RecordExit(9, time.Now()); !errors.Is(err, dagrun.ErrSecondExit) {
		t.Errorf("second RecordExit err = %v, want ErrSecondExit", err)
	}
	st.noLocal(t)
}

// TestLogsWithoutFollowStopsAtEnd attaches a viewer to a LIVE session and
// prints what arrived: it stops at the replay end and exits 0 instead of
// following forever.
func TestLogsWithoutFollowStopsAtEnd(t *testing.T) {
	st := newSessionTest(t)
	st.selectWorkspace(t, "skel")
	hub, id := sessionCommandHub(t, testRunID)
	attachTunnelServer(t, st, hub)
	st.runner.podsJSON = sessionPodJSON("default", "bashy-run-"+strings.ToLower(testRunID), "abcdef1234567890", "worker-a", "41234")
	if _, err := hub.Append([]byte("line1\nline2\n")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	done := make(chan struct{})
	var code int
	var stdout, stderr string
	go func() {
		defer close(done)
		code, stdout, stderr = st.exec(t, "logs", id)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("logs without -f followed a live session instead of stopping at the end")
	}
	if code != 0 {
		t.Errorf("logs exited %d, want 0 (stderr %q)", code, stderr)
	}
	if stdout != "line1\nline2\n" {
		t.Errorf("logs printed %q, want the replay to the current end", stdout)
	}
	st.noLocal(t)
}

// TestAttachLostExitCodeDistinct marks the executor lost, then attaches: the
// verb replays the surviving bytes and exits with the dedicated lost code —
// distinct from a clean exit and from eviction.
func TestAttachLostExitCodeDistinct(t *testing.T) {
	if sessionLostExitCode == 0 || sessionEvictedExitCode == 0 || sessionLostExitCode == sessionEvictedExitCode {
		t.Fatalf("lost=%d evicted=%d are not distinct non-zero codes", sessionLostExitCode, sessionEvictedExitCode)
	}
	st := newSessionTest(t)
	st.selectWorkspace(t, "skel")
	hub, id := sessionCommandHub(t, testRunID)
	attachTunnelServer(t, st, hub)
	st.runner.podsJSON = sessionPodJSON("default", "bashy-run-"+strings.ToLower(testRunID), "abcdef1234567890", "worker-a", "41234")
	if _, err := hub.Append([]byte("partial")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := hub.MarkLost(time.Now()); err != nil {
		t.Fatalf("MarkLost: %v", err)
	}

	code, stdout, stderr := st.exec(t, "attach", id)
	if code != sessionLostExitCode {
		t.Errorf("attach to a lost session exited %d, want %d (stderr %q)", code, sessionLostExitCode, stderr)
	}
	if stdout != "partial" {
		t.Errorf("attach replayed %q, want the surviving bytes", stdout)
	}
	if !strings.Contains(stderr, "lost") {
		t.Errorf("attach stderr %q does not say the session is lost", stderr)
	}
	st.noLocal(t)
}

// TestAttachEvictedExitCodeDistinct drives the verb with a stub stream that
// reports a takeover: the verb exits with the dedicated evicted code and
// tells the user to attach again.
func TestAttachEvictedExitCodeDistinct(t *testing.T) {
	st := newSessionTest(t)
	st.selectWorkspace(t, "skel")
	st.runner.podsJSON = sessionPodJSON("default", "bashy-run-"+strings.ToLower(testRunID), "abcdef1234567890", "worker-a", "41234")
	st.senv.tunnelBase = func(dagrun.Context) (string, error) { return "http://tunnel.invalid", nil }
	st.senv.dialStream = func(context.Context, string, string, int64, bool) (attachStream, error) {
		return &scriptStream{
			reads:   []scriptRead{{err: dagrun.ErrEvicted}},
			waitErr: dagrun.ErrEvicted,
		}, nil
	}

	code, _, stderr := st.exec(t, "attach", testRunID)
	if code != sessionEvictedExitCode {
		t.Errorf("attach after takeover exited %d, want %d (stderr %q)", code, sessionEvictedExitCode, stderr)
	}
	if !strings.Contains(stderr, "took over") && !strings.Contains(stderr, "evict") {
		t.Errorf("attach stderr %q does not name the takeover", stderr)
	}
	st.noLocal(t)
}

// TestAttachPumpReconnectsFromLastOffset drops the transport mid-stream: the
// pump redials once from the last byte and delivers every byte exactly once.
func TestAttachPumpReconnectsFromLastOffset(t *testing.T) {
	s := &scriptStream{
		reads: []scriptRead{
			{data: "one"},
			{err: errors.New("attach: connection lost")},
			{data: "two"},
			{err: io.EOF},
		},
	}
	var out bytes.Buffer
	end, err := pumpAttachStream(context.Background(), s, &out)
	if err != nil {
		t.Fatalf("pump: %v", err)
	}
	if out.String() != "onetwo" {
		t.Errorf("pump delivered %q, want %q without duplication or loss", out.String(), "onetwo")
	}
	if end.kind != "exit" || end.code != 0 {
		t.Errorf("pump end = %+v, want a clean exit 0", end)
	}
	if s.reconnects != 1 {
		t.Errorf("reconnects = %d, want exactly one redial", s.reconnects)
	}
}

// TestAttachViewerSendsViewerFlag pins the --viewer wiring: the tunnel
// endpoint carries viewer=1, so the server never lets the read-only client
// take the session over.
func TestAttachViewerSendsViewerFlag(t *testing.T) {
	st := newSessionTest(t)
	st.selectWorkspace(t, "skel")
	hub, id := sessionCommandHub(t, testRunID)
	_, dialed := attachTunnelServer(t, st, hub)
	st.runner.podsJSON = sessionPodJSON("default", "bashy-run-"+strings.ToLower(testRunID), "abcdef1234567890", "worker-a", "41234")
	if _, err := hub.Append([]byte("x")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := hub.RecordExit(0, time.Now()); err != nil {
		t.Fatalf("RecordExit: %v", err)
	}

	code, stdout, _ := st.exec(t, "attach", "--viewer", id)
	if code != 0 {
		t.Fatalf("viewer attach exited %d, want 0", code)
	}
	if stdout != "x" {
		t.Errorf("viewer replayed %q, want %q", stdout, "x")
	}
	if len(*dialed) != 1 || !(*dialed)[0].viewer {
		t.Errorf("viewer dials = %v, want exactly one viewer dial", *dialed)
	}
	st.noLocal(t)
}

// TestLogsFollowExitsWithSessionCode follows a finished session to its
// recorded exit and reports that code.
func TestLogsFollowExitsWithSessionCode(t *testing.T) {
	st := newSessionTest(t)
	st.selectWorkspace(t, "skel")
	hub, id := sessionCommandHub(t, testRunID)
	attachTunnelServer(t, st, hub)
	st.runner.podsJSON = sessionPodJSON("default", "bashy-run-"+strings.ToLower(testRunID), "abcdef1234567890", "worker-a", "41234")
	if _, err := hub.Append([]byte("out\n")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := hub.RecordExit(2, time.Now()); err != nil {
		t.Fatalf("RecordExit: %v", err)
	}

	code, stdout, stderr := st.exec(t, "logs", "-f", id)
	if code != 2 {
		t.Errorf("logs -f exited %d, want the session's exit code 2 (stderr %q)", code, stderr)
	}
	if stdout != "out\n" {
		t.Errorf("logs -f printed %q, want %q", stdout, "out\n")
	}
	st.noLocal(t)
}

// TestLogsBatchRunPointsToRunRecord pins the no-kubectl-logs rule: a batch
// run (a Job with no session server) gets the explicit RunRecord message and
// a non-zero exit, never a `kubectl logs` fallback.
func TestLogsBatchRunPointsToRunRecord(t *testing.T) {
	st := newSessionTest(t)
	st.selectWorkspace(t, "skel")
	st.runner.listJSON = runsJSON(jobJSON(testWorkspaceID, testRunID, "make", "Complete"))
	dialed := false
	st.senv.dialStream = func(context.Context, string, string, int64, bool) (attachStream, error) {
		dialed = true
		return nil, errors.New("must not dial for a batch run")
	}

	code, stdout, stderr := st.exec(t, "logs", testRunID)
	if code == 0 {
		t.Errorf("logs of a batch run exited 0 (stdout %q)", stdout)
	}
	if !strings.Contains(stderr, "logs for batch runs arrive with the RunRecord path") {
		t.Errorf("logs stderr %q misses the RunRecord message", stderr)
	}
	if dialed {
		t.Error("logs of a batch run dialled the tunnel")
	}
	if st.runner.podLookups() != 1 {
		t.Errorf("pod lookups = %d, want the one label lookup before the batch check", st.runner.podLookups())
	}
	st.noLocal(t)
}

// TestLogsSnapshotSendsPairedAuth proves the header seam: the snapshot dial
// carries the paired client's headers, and the server sees them. The scheme
// itself stays injected — production defaults to none.
func TestLogsSnapshotSendsPairedAuth(t *testing.T) {
	st := newSessionTest(t)
	st.selectWorkspace(t, "skel")
	hub, id := sessionCommandHub(t, testRunID)
	if _, err := hub.Append([]byte("auth-ok\n")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	wrapped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-DKS-Test-Auth") != "test-token" {
			http.Error(w, "missing paired auth", http.StatusUnauthorized)
			return
		}
		attach.Handler(hub).ServeHTTP(w, r)
	}))
	t.Cleanup(wrapped.Close)
	st.senv.tunnelBase = func(dagrun.Context) (string, error) { return wrapped.URL, nil }
	st.senv.authHeaders = func(string) (http.Header, error) {
		h := http.Header{}
		h.Set("X-DKS-Test-Auth", "test-token")
		return h, nil
	}
	st.runner.podsJSON = sessionPodJSON("default", "bashy-run-"+strings.ToLower(testRunID), "abcdef1234567890", "worker-a", "41234")

	code, stdout, stderr := st.exec(t, "logs", id)
	if code != 0 {
		t.Fatalf("logs exited %d, want 0 (stderr %q)", code, stderr)
	}
	if stdout != "auth-ok\n" {
		t.Errorf("logs printed %q, want the replay", stdout)
	}
	st.noLocal(t)
}

// TestAttachAppNameMatchesOutpostTransientFormat pins the yoke reimplementation
// against outpost's TransientAppName (outpost/internal/agent/vknode/transient.go):
// "vk-" + namespace + "-" + name + "-" + uid[:8], "noid" when uid is empty.
// A single-port session pod is addressed by this bare name (outpost tacks
// "-<port>" only onto non-first ports of multi-port pods).
func TestAttachAppNameMatchesOutpostTransientFormat(t *testing.T) {
	for _, tc := range []struct{ ns, name, uid, want string }{
		{"default", "bashy-run-abc", "1234567890abcdef", "vk-default-bashy-run-abc-12345678"},
		{"default", "bashy-run-abc", "12345678", "vk-default-bashy-run-abc-12345678"},
		{"prod", "pod", "ab", "vk-prod-pod-ab"},
		{"default", "pod", "", "vk-default-pod-noid"},
	} {
		if got := transientAppName(tc.ns, tc.name, tc.uid); got != tc.want {
			t.Errorf("transientAppName(%q,%q,%q) = %q, want %q", tc.ns, tc.name, tc.uid, got, tc.want)
		}
	}
}

// TestSessionVerbsFailClosed pins the closed doors: no selection, a weave
// run ref, a bad id, and session-server usage errors all exit non-zero
// having started nothing and run nothing locally.
func TestSessionVerbsFailClosed(t *testing.T) {
	st := newSessionTest(t)
	for _, args := range [][]string{
		{"attach", testRunID},
		{"logs", testRunID},
		{"logs", "-f", testRunID},
	} {
		if code, _, stderr := st.exec(t, args...); code == 0 || !strings.Contains(stderr, "no workspace selected") {
			t.Errorf("%v: code=%d stderr=%q, want the no-context refusal", args, code, stderr)
		}
	}
	st.selectWorkspace(t, "skel")
	for _, args := range [][]string{
		{"attach", "run:568"},
		{"logs", "run:568"},
		{"attach", "not a label,,,"},
		{"logs"},
		{"attach"},
	} {
		if code, _, stderr := st.exec(t, args...); code == 0 {
			t.Errorf("%v exited 0, want a usage failure", args)
		} else if args[0] == "attach" && len(args) > 1 && strings.HasPrefix(args[len(args)-1], "run:") && !strings.Contains(stderr, "weave run") {
			t.Errorf("%v stderr %q does not name the weave run", args, stderr)
		}
	}
	if len(st.runner.calls) != 0 {
		t.Errorf("refused verbs reached the plane: %v", st.runner.calls)
	}
	st.noLocal(t)

	if code, _, _ := st.exec(t, "session-server", "--session", "x"); code == 0 {
		t.Error("session-server without --home/--port/argv exited 0")
	}
}

// TestSessionServerIsDispatchedAsUserland keeps the hidden verb routable:
// the executor runs `bashy dks session-server`, which must pass the lean
// dispatch door even though help hides it.
func TestSessionServerIsDispatchedAsUserland(t *testing.T) {
	if !IsUserland([]string{"session-server", "--session", "x"}) {
		t.Error("IsUserland does not claim session-server")
	}
	found := false
	for _, c := range NewUserlandCmds() {
		if c.Name() == "session-server" {
			found = true
			if !c.Hidden {
				t.Error("session-server is not hidden")
			}
		}
	}
	if !found {
		t.Error("NewUserlandCmds registers no session-server command")
	}
}

// sessionRenderInput builds a Generate input for one run kind over the same
// values pkg/dagrun's golden tests use, so the dks side pins the executor
// contract without reaching into dagrun internals.
func sessionRenderInput(kind dagrun.RunKind) dagrun.GenerateInput {
	in := dagrun.GenerateInput{
		Command: dagrun.CommandRef{
			Name:      "bashy",
			Digest:    "sha256:4444444444444444444444444444444444444444444444444444444444444444",
			Artifacts: map[string]string{"linux/amd64": "sha256:1111111111111111111111111111111111111111111111111111111111111111"},
		},
		Argv:        []string{"bashy", "-i"},
		WorkspaceID: "ws-01jb6zexample0000000000",
		RunID:       "01JB7CEXAMPLE0000000000000",
		Cwd:         "~/sub",
		Env:         map[string]string{"LANG": "C.UTF-8", "TZ": "UTC"},
		Venue:       "cluster",
		Match:       map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64", "outpost.dhnt.io/host": "worker-a"},
		Run: dagrun.RunSpec{
			Kind:      kind,
			Resources: dagrun.Resources{CPU: 1, Memory: "512Mi"},
			Egress:    dagrun.Egress{Mode: dagrun.EgressInternet},
		},
	}
	if kind == dagrun.RunLongRunning {
		in.Command.Name = "agent"
		in.Argv = []string{"agent", "--task", `say "hi"`}
		in.Run = dagrun.RunSpec{Kind: kind, Timeout: 90 * time.Minute}
	}
	return in
}

// sessionPodObjects renders kind and returns its Pod object.
func sessionPodObjects(t *testing.T, kind dagrun.RunKind) dagrun.Object {
	t.Helper()
	m, err := dagrun.Generate(sessionRenderInput(kind))
	if err != nil {
		t.Fatalf("Generate(%s): %v", kind, err)
	}
	objs, err := dagrun.Render(m, dagrun.ExecutorClass{OS: "linux", Image: "example.invalid/bashy-executor:test"})
	if err != nil {
		t.Fatalf("Render(%s): %v", kind, err)
	}
	for _, o := range objs {
		if o["kind"] == "Pod" {
			return o
		}
	}
	t.Fatalf("no Pod rendered for %s: %v", kind, objs)
	return nil
}

// TestRenderInteractivePodRunsSessionServerWithHostPort pins the executor
// contract: interactive and long-running Pods run their body under the hidden
// session server on a deterministic host-published port, with the port in the
// dhnt.io/attach-port annotation the verbs read back. One-shot Jobs are
// untouched.
func TestRenderInteractivePodRunsSessionServerWithHostPort(t *testing.T) {
	for _, kind := range []dagrun.RunKind{dagrun.RunInteractive, dagrun.RunLongRunning} {
		pod := sessionPodObjects(t, kind)
		in := sessionRenderInput(kind)

		md, _ := pod["metadata"].(map[string]any)
		spec, _ := pod["spec"].(map[string]any)
		var ctr map[string]any
		for _, c := range spec["containers"].([]any) {
			ctr = c.(map[string]any)
		}
		cmd, _ := ctr["command"].([]any)
		strs := make([]string, len(cmd))
		for i, c := range cmd {
			strs[i], _ = c.(string)
		}
		// The body is what the Pod ran before, now wrapped: session-server
		// flags, then "--", then the original `bashy -c <body>`.
		var body string
		m, err := dagrun.Generate(in)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := dagrun.ParseManifest(m)
		if err != nil {
			t.Fatal(err)
		}
		body = strings.TrimSuffix(parsed.Task.Body, "\n")
		port := 0
		for i, c := range strs {
			if c == "--port" && i+1 < len(strs) {
				fmt.Sscanf(strs[i+1], "%d", &port)
			}
		}
		if port < 40000 || port > 49999 {
			t.Fatalf("%s: attach port %d is outside 40000-49999", kind, port)
		}
		want := []string{"bashy", "dks", "session-server",
			"--session", in.RunID, "--home", dagrun.HomeMountPath,
			"--port", fmt.Sprint(port), "--", "bashy", "-c", body}
		if strings.Join(strs, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%s command = %q, want %q", kind, strs, want)
		}
		ports, _ := ctr["ports"].([]any)
		if len(ports) != 1 {
			t.Fatalf("%s ports = %v, want the one published attach port", kind, ports)
		}
		entry, _ := ports[0].(map[string]any)
		if entry["containerPort"] != port || entry["hostPort"] != port {
			t.Errorf("%s port entry = %v, want containerPort=hostPort=%d", kind, entry, port)
		}
		anns, _ := md["annotations"].(map[string]any)
		if anns[dagrun.AnnotAttachPort] != fmt.Sprint(port) {
			t.Errorf("%s annotation %q = %v, want %d", kind, dagrun.AnnotAttachPort, anns[dagrun.AnnotAttachPort], port)
		}
		if ctr["stdin"] != nil && kind == dagrun.RunLongRunning {
			t.Errorf("long-running container carries stdin flags: %v", ctr["stdin"])
		}
	}

	// The port is a pure function of the session id: re-rendering agrees.
	again := sessionPodObjects(t, dagrun.RunInteractive)
	annAgain, _ := again["metadata"].(map[string]any)["annotations"].(map[string]any)
	annOnce, _ := sessionPodObjects(t, dagrun.RunInteractive)["metadata"].(map[string]any)["annotations"].(map[string]any)
	if annAgain[dagrun.AnnotAttachPort] != annOnce[dagrun.AnnotAttachPort] {
		t.Error("attach port is not deterministic across renders")
	}

	// One-shot still renders the unwrapped Job: no session server, no ports,
	// no attach annotation.
	oneShot, err := dagrun.Generate(dagrun.GenerateInput{
		Command:     dagrun.CommandRef{Name: "echo", Digest: testExeSHA, Artifacts: map[string]string{"linux/amd64": testAmdSHA}},
		Argv:        []string{"echo", "hi"},
		WorkspaceID: "ws-01jb6zexample0000000000",
		RunID:       "01JB7CEXAMPLE0000000000000",
		Venue:       "cluster",
	})
	if err != nil {
		t.Fatalf("Generate(one-shot): %v", err)
	}
	oneObjs, err := dagrun.Render(oneShot, dagrun.ExecutorClass{OS: "linux", Image: "example.invalid/bashy-executor:test"})
	if err != nil {
		t.Fatalf("Render(one-shot): %v", err)
	}
	var job dagrun.Object
	for _, o := range oneObjs {
		if o["kind"] == "Job" {
			job = o
		}
	}
	if job == nil {
		t.Fatalf("one-shot objects = %v, want the Job", oneObjs)
	}
	jobSpec, _ := job["spec"].(map[string]any)
	tmpl, _ := jobSpec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	ctrs, _ := podSpec["containers"].([]any)
	oneCtr, _ := ctrs[0].(map[string]any)
	oneCmd, _ := oneCtr["command"].([]any)
	var oneStrs []string
	for _, c := range oneCmd {
		s, _ := c.(string)
		oneStrs = append(oneStrs, s)
	}
	if strings.Join(oneStrs, "\x00") != strings.Join([]string{"bashy", "-c", "echo hi"}, "\x00") {
		t.Errorf("one-shot command = %q, want the unwrapped body", oneStrs)
	}
	if _, ok := oneCtr["ports"]; ok {
		t.Errorf("one-shot container publishes ports: %v", oneCtr["ports"])
	}
	oneMD, _ := job["metadata"].(map[string]any)
	oneAnns, _ := oneMD["annotations"].(map[string]any)
	if _, ok := oneAnns[dagrun.AnnotAttachPort]; ok {
		t.Errorf("one-shot carries the attach annotation: %v", oneAnns[dagrun.AnnotAttachPort])
	}
}

// TestAttachUsesPairedCredentialsAndDerivedTunnelBase is the production
// wiring: no tunnel env var, the base derived from the plane context's
// kubeconfig server, and the paired Outpost's bearer plus the app-scoped
// elevation cookie the edge mints for it. The httptest tunnel refuses any
// request without the bearer and any upgrade without the cookie.
func TestAttachUsesPairedCredentialsAndDerivedTunnelBase(t *testing.T) {
	st := newSessionTest(t)
	st.senv.tunnelBase = defaultTunnelBase
	st.senv.authHeaders = pairedAuthHeaders
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv(tunnelBaseEnv, "")
	t.Setenv("OUTPOST_SESSION_JWT", "")

	st.selectWorkspace(t, "skel@k8s:plane-a")
	hub, id := sessionCommandHub(t, testRunID)
	if _, err := hub.Append([]byte("paired-ok")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := hub.RecordExit(0, time.Now()); err != nil {
		t.Fatalf("RecordExit: %v", err)
	}
	podName := "bashy-run-" + strings.ToLower(testRunID)
	appPath := "/matrix/h/worker-a/app/vk-default-" + podName + "-abcdef12"
	const bearer, cookie = "test-paired-bearer-0123456789", "test-elev-cookie"
	var primed, upgrades int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, appPath+"/") {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+bearer {
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		if !websocket.IsWebSocketUpgrade(r) {
			primed++
			http.SetCookie(w, &http.Cookie{Name: "matrix_elev", Value: cookie, Path: appPath, HttpOnly: true})
			http.Error(w, "no index", http.StatusNotFound)
			return
		}
		if c, err := r.Cookie("matrix_elev"); err != nil || c.Value != cookie {
			http.Error(w, "elevation required", http.StatusForbidden)
			return
		}
		upgrades++
		attach.Handler(hub).ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	kube := filepath.Join(home, "kubeconfig")
	writeTestFile(t, kube, 0o600, fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: other
clusters:
- name: other
  cluster:
    server: https://other.invalid:6443
- name: plane-a-cluster
  cluster:
    server: %s/k8s/clusters/plane-a
contexts:
- name: other
  context:
    cluster: other
- name: plane-a
  context:
    cluster: plane-a-cluster
`, srv.URL))
	t.Setenv("KUBECONFIG", kube)
	writeTestFile(t, filepath.Join(home, "xdg", "matrix", "agent.json"), 0o600,
		fmt.Sprintf(`{"server_addr":"127.0.0.1","server_port":443,"protocol":"wss","access_token":%q}`, bearer))
	st.runner.podsJSON = sessionPodJSON("default", podName, "abcdef1234567890", "worker-a", "41234")

	code, stdout, stderr := st.exec(t, "attach", id)
	if code != 0 {
		t.Fatalf("attach exited %d, want 0 (stderr %q)", code, stderr)
	}
	if stdout != "paired-ok" {
		t.Errorf("attach replayed %q, want %q", stdout, "paired-ok")
	}
	if primed == 0 || upgrades != 1 {
		t.Errorf("primed=%d upgrades=%d, want the cookie minted then one upgrade", primed, upgrades)
	}
	if strings.Contains(stderr, bearer) || strings.Contains(stderr, cookie) {
		t.Errorf("stderr leaked a credential: %q", stderr)
	}
	st.noLocal(t)
}

func writeTestFile(t *testing.T, path string, mode os.FileMode, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// TestPairedAuthHeadersGuards pins the credential doors: the bearer never
// goes to a host other than the paired server, a refusal at the edge is an
// actionable error that names no credential, a config other users can read
// is ignored, and $BASHY_DKS_TUNNEL_BASE overrides the derived base.
func TestPairedAuthHeadersGuards(t *testing.T) {
	newSessionTest(t)
	home := os.Getenv("HOME")
	const bearer = "test-paired-bearer-0123456789"
	cfg := filepath.Join(home, "xdg", "matrix", "agent.json")
	writeTestFile(t, cfg, 0o600, fmt.Sprintf(`{"server_addr":"127.0.0.1:443","access_token":%q}`, bearer))
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, "not shared", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	wsEndpoint := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/matrix/h/worker-a/app/vk-x/attach?client=c"

	_, err := pairedAuthHeaders(wsEndpoint)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "worker-a") || strings.Contains(err.Error(), bearer) {
		t.Errorf("edge refusal error = %v, want an actionable 403 naming the host and no credential", err)
	}

	foreign := strings.Replace(wsEndpoint, "127.0.0.1", "localhost", 1)
	before := hits
	if h, err := pairedAuthHeaders(foreign); err != nil || h != nil || hits != before {
		t.Errorf("foreign tunnel got headers=%v err=%v hits=%d, want no credentials and no request", h, err, hits-before)
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(cfg, 0o644); err != nil {
			t.Fatal(err)
		}
		if h, err := pairedAuthHeaders(wsEndpoint); err != nil || h != nil {
			t.Errorf("readable config used: headers=%v err=%v", h, err)
		}
	}

	t.Setenv(tunnelBaseEnv, "https://override.invalid/")
	if got, err := defaultTunnelBase(dagrun.Context{Plane: "k8s:missing"}); err != nil || got != "https://override.invalid" {
		t.Errorf("override base = %q, %v", got, err)
	}
	t.Setenv(tunnelBaseEnv, "")
	if _, err := defaultTunnelBase(dagrun.Context{Plane: "k8s:missing"}); err == nil || !strings.Contains(err.Error(), tunnelBaseEnv) {
		t.Errorf("missing kubeconfig context error = %v, want fail-closed naming $%s", err, tunnelBaseEnv)
	}
}
