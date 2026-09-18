package meet

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The daemon lifecycle outpost drives. Every test points $BASHY_MEET_DIR at a
// temp dir first, so the pidfile under test is never a developer's real one — and
// so nothing here can signal a real `meet serve`.
//
// One rule runs through all of it: NEVER put this process's own pid in a pidfile
// and then call StopService. It would work exactly as designed and kill the test
// binary. Live-process cases spawn a real child instead (see stopHelper).

const deadPID = 2147480000 // above any real pid, so it cannot be recycled onto us

func serviceTestDir(t *testing.T) {
	t.Helper()
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
}

func pidPath(t *testing.T) string {
	t.Helper()
	p, err := servicePidPath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestServiceStatusLifecycle(t *testing.T) {
	serviceTestDir(t)
	var opt ServiceOptions

	// Nothing started yet.
	clearOpt := ServiceOptions{Port: freeServicePort(t)}
	if st := ServiceStatusOf(clearOpt); st.Running {
		t.Fatalf("no pidfile must read as stopped, got %+v", st)
	}

	// A pidfile pointing at THIS live process reads as running.
	if err := writePid(pidPath(t), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	st := ServiceStatusOf(opt)
	if !st.Running || st.PID != os.Getpid() {
		t.Fatalf("expected running with our pid, got %+v", st)
	}
	// The default address must match `meet serve`'s own defaults, or start and
	// serve would disagree about where the room is.
	if st.Addr != "127.0.0.1:8637" {
		t.Errorf("default addr = %q, want 127.0.0.1:8637", st.Addr)
	}

	// A pidfile left behind by a crashed server reads as stopped: the file alone
	// proves nothing.
	if err := writePid(pidPath(t), deadPID); err != nil {
		t.Fatal(err)
	}
	if st := ServiceStatusOf(clearOpt); st.Running {
		t.Fatalf("a dead pid must read as stopped, got %+v", st)
	}
}

func TestServiceOptionsHonorPort(t *testing.T) {
	serviceTestDir(t)
	st := ServiceStatusOf(ServiceOptions{Port: 9099, Bind: "0.0.0.0"})
	if st.Addr != "0.0.0.0:9099" {
		t.Fatalf("addr = %q, want 0.0.0.0:9099", st.Addr)
	}
}

// Criterion: two starts in a row, the second a clean no-op. outpost re-runs start
// on every supervision tick, so a second server racing for the port would be a
// permanent restart loop.
func TestStartServiceIsIdempotent(t *testing.T) {
	serviceTestDir(t)
	var opt ServiceOptions

	// Stand in for a first, successful start: a pidfile naming a LIVE process
	// (ours). Actually spawning a server would bind port 8637 on the developer's
	// machine, which a hermetic test must never do.
	if err := writePid(pidPath(t), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(pidPath(t))
	if err != nil {
		t.Fatal(err)
	}

	st, err := StartService(opt)
	if err != nil {
		t.Fatalf("a second start must not fail: %v", err)
	}
	if !st.Running || st.PID != os.Getpid() {
		t.Fatalf("the second start must report the EXISTING daemon, got %+v", st)
	}
	after, err := os.ReadFile(pidPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the second start rewrote the pidfile (%s → %s): it spawned something", before, after)
	}
}

// Criterion: removing a stale pidfile is success. A stop after a crash must not
// look like a broken stop, or a supervisor loops on it forever.
func TestStopServiceStalePidfile(t *testing.T) {
	serviceTestDir(t)
	if err := writePid(pidPath(t), deadPID); err != nil {
		t.Fatal(err)
	}
	st, err := StopService(ServiceOptions{Port: freeServicePort(t)})
	if err != nil {
		t.Fatalf("a stale pidfile must stop cleanly: %v", err)
	}
	if st.Running {
		t.Errorf("stop must report stopped, got %+v", st)
	}
	if _, err := os.Stat(pidPath(t)); !os.IsNotExist(err) {
		t.Errorf("stop must remove the stale pidfile (stat err = %v)", err)
	}
}

// Nothing listening and no pidfile is success, but it is not evidence that this
// invocation stopped anything.
func TestStopServiceNoPidfile(t *testing.T) {
	serviceTestDir(t)
	st, err := StopService(ServiceOptions{Port: freeServicePort(t)})
	if err != nil {
		t.Fatalf("stop with no pidfile must succeed: %v", err)
	}
	if st.State != serviceStateNotRunning {
		t.Fatalf("state = %q, want %q", st.State, serviceStateNotRunning)
	}
}

// --- the real-process half ---------------------------------------------------

// stopHelper re-execs the test binary into TestServiceDaemonHelperProcess and
// returns it, with the pidfile already written. The child is the only honest way
// to test signalling: an in-process fake would prove nothing about SIGTERM.
//
// The caller checks the exit with waitExit rather than waitGone. Here — and ONLY
// here — the daemon is a child of the process stopping it, so once it dies it
// sits as a zombie until reaped and any liveness probe still sees it. In
// production stop runs from a different process entirely, so there is nothing to
// reap and no zombie to confuse.
func stopHelper(t *testing.T, ignoreTerm bool) *exec.Cmd {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no SIGTERM on windows; stop collapses to TerminateProcess there")
	}
	cmd := helperCmd(t, ignoreTerm)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	// Wait for the child to have installed its signal handling, so the SIGTERM
	// below races nothing.
	buf := make([]byte, len("ready\n"))
	if _, err := io.ReadFull(stdout, buf); err != nil || !strings.HasPrefix(string(buf), "ready") {
		t.Fatalf("helper never reported ready (%q, err %v)", buf, err)
	}
	if err := writePid(pidPath(t), cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	return cmd
}

// waitExit reaps the helper and reports which signal took it down, failing if it
// is still running when the deadline passes.
func waitExit(t *testing.T, cmd *exec.Cmd, d time.Duration) syscall.Signal {
	t.Helper()
	done := make(chan *os.ProcessState, 1)
	go func() {
		st, _ := cmd.Process.Wait()
		done <- st
	}()
	select {
	case st := <-done:
		ws, ok := st.Sys().(syscall.WaitStatus)
		if !ok || !ws.Signaled() {
			return 0
		}
		return ws.Signal()
	case <-time.After(d):
		t.Fatalf("pid %d was still running %v after stop", cmd.Process.Pid, d)
		return 0
	}
}

func TestStopServiceTerminatesLiveDaemon(t *testing.T) {
	serviceTestDir(t)
	cmd := stopHelper(t, false)
	port := freeServicePort(t)

	if _, err := StopService(ServiceOptions{Port: port, Grace: 5 * time.Second}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if sig := waitExit(t, cmd, 5*time.Second); sig != syscall.SIGTERM {
		t.Errorf("a well-behaved daemon must go on SIGTERM, got signal %v", sig)
	}
	if _, err := os.Stat(pidPath(t)); !os.IsNotExist(err) {
		t.Errorf("stop must remove the pidfile (stat err = %v)", err)
	}
}

// The escalation: a server that ignores SIGTERM still has to give the port back,
// or the supervisor's restart can never succeed.
func TestStopServiceEscalatesToKill(t *testing.T) {
	serviceTestDir(t)
	cmd := stopHelper(t, true)
	port := freeServicePort(t)

	start := time.Now()
	if _, err := StopService(ServiceOptions{Port: port, Grace: 300 * time.Millisecond}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	elapsed := time.Since(start)
	if sig := waitExit(t, cmd, 5*time.Second); sig != syscall.SIGKILL {
		t.Errorf("a daemon that ignores SIGTERM must be killed, got signal %v", sig)
	}
	if elapsed < 300*time.Millisecond {
		t.Errorf("stop killed after %v — it skipped the grace period and never asked politely", elapsed)
	}
}

// serviceHelperEnv selects the child half below: "term" dies on SIGTERM like a
// well-behaved server, "ignore" refuses it and has to be killed.
const serviceHelperEnv = "MEET_TEST_SERVICE_HELPER"
const serviceHelperPortEnv = "MEET_TEST_SERVICE_PORT"

func helperCmd(t *testing.T, ignoreTerm bool) *exec.Cmd {
	t.Helper()
	mode := "term"
	if ignoreTerm {
		mode = "ignore"
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestServiceDaemonHelperProcess", "-test.v=false")
	cmd.Env = append(os.Environ(), serviceHelperEnv+"="+mode)
	cmd.Stderr = os.Stderr
	return cmd
}

func freeServicePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// invisibleServingHelper starts a real health-serving process while store A is
// selected, writes its pidfile there, then switches the caller to store B. This
// is the reported failure mode: the pidfile is invisible but the port is live.
func invisibleServingHelper(t *testing.T) (*exec.Cmd, int) {
	t.Helper()
	serviceTestDir(t)
	port := freeServicePort(t)
	cmd := exec.Command(os.Args[0], "-test.run=TestServiceDaemonHelperProcess", "-test.v=false")
	cmd.Env = append(os.Environ(),
		serviceHelperEnv+"=serve",
		serviceHelperPortEnv+"="+strconv.Itoa(port))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	buf := make([]byte, len("ready\n"))
	if _, err := io.ReadFull(stdout, buf); err != nil || string(buf) != "ready\n" {
		t.Fatalf("serving helper never reported ready (%q, err %v)", buf, err)
	}
	if err := writePid(pidPath(t), cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	return cmd, port
}

// TestServiceDaemonHelperProcess is not a test. It is the child half of the stop
// tests, and does nothing unless re-exec'd with serviceHelperEnv set.
func TestServiceDaemonHelperProcess(t *testing.T) {
	mode := os.Getenv(serviceHelperEnv)
	if mode == "" {
		t.Skip("child-process helper for the StopService tests")
	}
	if mode == "ignore" {
		// Notified-but-unhandled: SIGTERM stops terminating this process, which is
		// exactly the wedged server the escalation exists for.
		signal.Notify(make(chan os.Signal, 1), syscall.SIGTERM)
	}
	if mode == "serve" {
		port, err := strconv.Atoi(os.Getenv(serviceHelperPortEnv))
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true,"service":"bashy-meet"}`)
		})
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = os.Stdout.WriteString("ready\n")
		if err := http.Serve(ln, mux); err != nil {
			t.Fatal(err)
		}
		return
	}
	_, _ = os.Stdout.WriteString("ready\n")
	// Sleep rather than block on a channel: a parked goroutine with no timer can
	// trip the runtime's deadlock detector, and the parent is going to signal this
	// process long before the nap is over.
	time.Sleep(60 * time.Second)
	os.Exit(9) // never reached; a 9 in the log means nobody ever signalled us
}

// --- the command surface -----------------------------------------------------

// Criterion: status exits 0 running, non-zero stopped. outpost reads both the
// exit code and the "running"/"stopped" token.
func TestServiceStatusCommandExitCode(t *testing.T) {
	serviceTestDir(t)
	port := freeServicePort(t)

	out, err := runMeet(t, "service", "status", "--port", strconv.Itoa(port))
	if !errors.Is(err, ErrServiceStopped) {
		t.Fatalf("status of a stopped daemon must fail; err = %v", err)
	}
	if out != "not running\n" {
		t.Fatalf("status must distinguish absence from a completed stop, got %q", out)
	}

	if err := writePid(pidPath(t), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	out, err = runMeet(t, "service", "status")
	if err != nil {
		t.Fatalf("status of a running daemon must exit 0; err = %v", err)
	}
	if !strings.Contains(out, "running") {
		t.Fatalf("status must print the token 'running', got %q", out)
	}
}

func TestServiceStatusCommandJSON(t *testing.T) {
	serviceTestDir(t)
	if err := writePid(pidPath(t), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	out, err := runMeet(t, "service", "status", "--json", "--port", "9100")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var st ServiceStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("status --json must emit one JSON object, got %q: %v", out, err)
	}
	if st.SchemaVersion != serviceSchema || !st.Running || st.PID != os.Getpid() {
		t.Fatalf("unexpected status %+v", st)
	}
	if st.Addr != "127.0.0.1:9100" {
		t.Errorf("--port must reach the status envelope, got %q", st.Addr)
	}
}

func TestServiceStopCommandStalePidfile(t *testing.T) {
	serviceTestDir(t)
	if err := writePid(pidPath(t), deadPID); err != nil {
		t.Fatal(err)
	}
	out, err := runMeet(t, "service", "stop", "--port", strconv.Itoa(freeServicePort(t)))
	if err != nil {
		t.Fatalf("stop with a stale pidfile must exit 0; err = %v", err)
	}
	if out != "meet service stop: stale pidfile removed\n" {
		t.Errorf("stop must identify the stale pidfile outcome, got %q", out)
	}
}

func TestServiceStopCommandNotRunning(t *testing.T) {
	serviceTestDir(t)
	port := freeServicePort(t)
	out, err := runMeet(t, "service", "stop", "--port", strconv.Itoa(port))
	if err != nil {
		t.Fatalf("stop with no daemon must exit 0; err = %v", err)
	}
	if out != "meet service stop: not running\n" {
		t.Fatalf("stop must not claim it stopped something, got %q", out)
	}
}

func TestServiceStopCommandStopsLiveDaemon(t *testing.T) {
	serviceTestDir(t)
	cmd := stopHelper(t, false)
	port := freeServicePort(t)
	out, err := runMeet(t, "service", "stop", "--port", strconv.Itoa(port))
	if err != nil {
		t.Fatalf("stop of a live daemon must exit 0; err = %v", err)
	}
	if out != "meet service stop: stopped\n" {
		t.Fatalf("stop of a live daemon output = %q", out)
	}
	if sig := waitExit(t, cmd, 5*time.Second); sig != syscall.SIGTERM {
		t.Errorf("a well-behaved daemon must go on SIGTERM, got signal %v", sig)
	}
}

func TestServiceStopFailsForPidfileInvisibleDaemon(t *testing.T) {
	cmd, port := invisibleServingHelper(t)
	out, err := runMeet(t, "service", "stop", "--port", strconv.Itoa(port))
	if !errors.Is(err, ErrServiceUnidentified) {
		t.Fatalf("stop must fail when the serving daemon has no visible pidfile; err = %v", err)
	}
	if !strings.Contains(out, "FAILED: bashy-meet is running on port "+strconv.Itoa(port)) ||
		strings.Contains(out, ": stopped") {
		t.Fatalf("stop must name the live port and must not claim stopped, got %q", out)
	}
	if !processAlive(cmd.Process.Pid) {
		t.Fatalf("stop without the daemon's pidfile killed pid %d", cmd.Process.Pid)
	}
}

func TestServiceStatusFindsPidfileInvisibleDaemon(t *testing.T) {
	_, port := invisibleServingHelper(t)
	out, err := runMeet(t, "service", "status", "--port", strconv.Itoa(port))
	if err != nil {
		t.Fatalf("status of the serving daemon must exit 0; err = %v", err)
	}
	if !strings.Contains(out, "running on port 127.0.0.1:"+strconv.Itoa(port)) ||
		strings.Contains(out, "stopped") {
		t.Fatalf("status must report port evidence, not stopped, got %q", out)
	}
}

// `meet service` must be a SUBCOMMAND, not a rename of anything. `meet open`
// convenes a deliberation session and is load-bearing; the daemon verbs sit one
// level down precisely so that name stays free.
func TestMeetOpenIsSeparateFromService(t *testing.T) {
	serviceTestDir(t)
	root := NewMeetCmd()

	open, _, err := root.Find([]string{"open"})
	if err != nil || open.Name() != "open" {
		t.Fatalf("`meet open` must exist: %v", err)
	}
	if !strings.Contains(open.Short, "open a meeting") {
		t.Fatalf("`meet open` must convene a meeting, got Short = %q", open.Short)
	}
	svc, _, err := root.Find([]string{"service", "start"})
	if err != nil || svc.Parent() == nil || svc.Parent().Name() != "service" {
		t.Fatalf("`meet service start` must be its own subcommand: %v", err)
	}
	if svc == open {
		t.Fatal("`meet service start` and `meet open` resolved to the SAME command")
	}

	// And it still runs. --dry-run resolves the session and exits without
	// launching a single agent, which is as far as a hermetic test can go.
	//
	// The depth marker is cleared first: markDepth() sets it in THIS process's
	// environment (every meet server and session does), so a test that ran earlier
	// in the binary leaves `start` refusing to convene from inside a meeting.
	t.Setenv(meetDepthEnv, "")
	out, err := runMeet(t, "open", "--topic", "ship the thing",
		"--participant", "codex", "--owner", "gemini", "--dry-run")
	if err != nil {
		t.Fatalf("`meet open --dry-run` must work: %v", err)
	}
	if !strings.Contains(out, "resolved session") ||
		!strings.Contains(out, "ship-the-thing") ||
		!strings.Contains(out, "(dry-run: no agents launched)") {
		t.Fatalf("`meet open --dry-run` output changed shape: %q", out)
	}
}

// runMeet drives the real cobra tree and returns everything it printed.
func runMeet(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewMeetCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}
