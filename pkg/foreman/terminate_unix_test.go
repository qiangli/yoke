//go:build darwin || linux

package foreman

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/room"
)

// The fixture is THIS test binary re-executed: it records itself as the serve
// process of an isolated store exactly as ServeControl does, then holds a
// control socket that accepts and never answers — the Sprint 457 shape. No real
// session, socket, or home directory is involved.
const hungServeEnv = "FOREMAN_TEST_HUNG_SERVE"

func TestMain(m *testing.M) {
	if spec := os.Getenv(hungServeEnv); spec != "" {
		root, rest, _ := strings.Cut(spec, "|")
		id, mode, _ := strings.Cut(rest, "|")
		if mode == "ignore-term" {
			signal.Ignore(syscall.SIGTERM) // forces Terminate into its SIGKILL path
		}
		runHungServeFixture(root, id)
		return
	}
	os.Exit(m.Run())
}

func runHungServeFixture(root, id string) {
	store := NewStore(root, id)
	st, err := store.LoadState()
	if err != nil {
		os.Exit(3)
	}
	if err := store.SaveState(withServeProcess(st)); err != nil {
		os.Exit(4)
	}
	ln, err := net.Listen("unix", store.CtlSockPath())
	if err != nil {
		os.Exit(5)
	}
	var held []net.Conn
	for {
		c, err := ln.Accept()
		if err != nil {
			os.Exit(6)
		}
		held = append(held, c) // accepted, never read, never answered
	}
}

func startHungServe(t *testing.T) (Store, *exec.Cmd) {
	t.Helper()
	return startHungServeMode(t, "")
}

func startHungServeMode(t *testing.T, mode string) (Store, *exec.Cmd) {
	t.Helper()
	root, err := os.MkdirTemp("", "fmh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	store := NewStore(root, "sprint-1-manager")
	if err := store.SaveState(State{ID: store.ID, Agent: "fixture", Status: StatusWorking}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), hungServeEnv+"="+root+"|"+store.ID+"|"+mode)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, _ := store.LoadState()
		if _, err := os.Stat(store.CtlSockPath()); err == nil && st.ServePID == cmd.Process.Pid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hung serve fixture never came up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Reap promptly so liveness reflects exit, not a zombie awaiting Wait.
	terminateReaped = func(pid int) bool {
		select {
		case <-exited:
			return true
		default:
			return !room.PidAlive(pid)
		}
	}
	t.Cleanup(func() { terminateReaped = func(pid int) bool { return !room.PidAlive(pid) } })
	return store, cmd
}

func TestTerminateStopsAProvenOwnedHungServe(t *testing.T) {
	store, cmd := startHungServe(t)

	// The reproducer: the ordinary stop goes over the same socket and dies on
	// the ack read deadline, so the lifecycle could never stop a hung session.
	_, err := SendCommand(store.Root, store.ID, Command{Verb: CommandStop})
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("stop over a hung socket: got %v, want an i/o timeout", err)
	}
	if !room.PidAlive(cmd.Process.Pid) {
		t.Fatal("fixture died on its own; the reproducer proves nothing")
	}

	if err := Terminate(store.Root, store.ID, "control socket unresponsive"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if !terminateReaped(cmd.Process.Pid) {
		t.Fatal("proven-owned hung serve process is still running")
	}
	st, err := store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Stopped || !strings.Contains(st.StopReason, "control socket unresponsive") {
		t.Fatalf("state not recorded as stopped: %+v", st)
	}
	if _, err := os.Stat(store.CtlSockPath()); !os.IsNotExist(err) {
		t.Fatalf("stale control socket left behind: %v", err)
	}
}

func TestTerminateRefusesAPidItCannotProve(t *testing.T) {
	store, cmd := startHungServe(t)
	st, err := store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	// Same live pid, different process identity: a recycled pid must never be
	// signalled on the strength of a pid number alone.
	st.ServeStart = "not-this-process"
	if err := store.SaveState(st); err != nil {
		t.Fatal(err)
	}
	err = Terminate(store.Root, store.ID, "control socket unresponsive")
	if !errors.Is(err, ErrServeNotProven) {
		t.Fatalf("Terminate on an unproven pid: got %v, want ErrServeNotProven", err)
	}
	time.Sleep(100 * time.Millisecond)
	if !room.PidAlive(cmd.Process.Pid) {
		t.Fatal("an unproven process was signalled")
	}
	if st, _ := store.LoadState(); st.Stopped {
		t.Fatal("a refused termination recorded the session as stopped")
	}
	if _, err := os.Stat(filepath.Join(store.CtlSockPath())); err != nil {
		t.Fatalf("a refused termination removed the socket: %v", err)
	}
}

// The pid is re-proven before the delayed SIGKILL: a serve process that exited
// during the grace period may have had its pid recycled, and the new owner of
// that pid must not be killed. The seam makes the recycle deterministic.
func TestTerminateReprovesBeforeSIGKILL(t *testing.T) {
	store, cmd := startHungServeMode(t, "ignore-term")
	oldGrace, oldStart := terminateGrace, terminateProcessStart
	t.Cleanup(func() { terminateGrace, terminateProcessStart = oldGrace, oldStart })
	terminateGrace = 200 * time.Millisecond
	calls := 0
	terminateProcessStart = func(pid int) (string, bool) {
		calls++
		if calls > 1 {
			return "recycled", true // after SIGTERM the pid names another process
		}
		return ProcessStart(pid)
	}
	err := Terminate(store.Root, store.ID, "control socket unresponsive")
	if !errors.Is(err, ErrServeNotProven) {
		t.Fatalf("Terminate after a recycle during grace: got %v, want ErrServeNotProven", err)
	}
	time.Sleep(100 * time.Millisecond)
	if !room.PidAlive(cmd.Process.Pid) {
		t.Fatal("SIGKILL reached a pid that no longer proved to be the serve process")
	}
	if st, _ := store.LoadState(); st.Stopped {
		t.Fatal("an unproven termination recorded the session as stopped")
	}
	if _, err := os.Stat(store.CtlSockPath()); err != nil {
		t.Fatalf("an unproven termination removed the socket: %v", err)
	}
}

// The session is re-proven before it is recorded stopped: if another serve
// process took the session over while Terminate waited, its state and socket
// belong to that process and must be preserved.
func TestTerminatePreservesASessionReplacedWhileWaiting(t *testing.T) {
	store, _ := startHungServe(t)
	reaped := terminateReaped
	replaced := false
	terminateReaped = func(pid int) bool {
		if !reaped(pid) {
			return false
		}
		if !replaced {
			replaced = true
			st, err := store.LoadState()
			if err != nil {
				t.Error(err)
			}
			st.ServePID, st.ServeStart = os.Getpid(), "replacement"
			if err := store.SaveState(st); err != nil {
				t.Error(err)
			}
		}
		return true
	}
	err := Terminate(store.Root, store.ID, "control socket unresponsive")
	if !errors.Is(err, ErrServeNotProven) {
		t.Fatalf("Terminate over a replaced session: got %v, want ErrServeNotProven", err)
	}
	st, _ := store.LoadState()
	if st.Stopped || st.ServeStart != "replacement" {
		t.Fatalf("the replacement session's state was overwritten: %+v", st)
	}
	if _, err := os.Stat(store.CtlSockPath()); err != nil {
		t.Fatalf("the replacement session's socket was removed: %v", err)
	}
}
