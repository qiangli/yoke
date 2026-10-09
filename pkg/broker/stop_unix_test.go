//go:build !windows

package broker

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// engineStopHelperDirEnv is the directory the child reports readiness and the
// caught signal in (engineStopHelperEnv selects the child half itself).
const engineStopHelperDirEnv = "BASHY_BROKER_STOP_HELPER_DIR"

// TestStopDoorProcessIsGracefulOnUnix pins the Unix contract `llm down` has
// always had: a SIGTERM the door catches and handles, not a Kill. The child
// records the signal it saw, so the test observes the delivered verb and not
// merely the exit.
func TestStopDoorProcessIsGracefulOnUnix(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process")
	}
	cmd, dir, exited := spawnSignalChild(t, nil)
	if err := stopDoorProcess(cmd.Process); err != nil {
		t.Fatalf("stopDoorProcess: %v", err)
	}
	waitExit(t, "door", cmd.Process.Pid, exited)
	if err := sawSIGTERM(dir); err != nil {
		t.Fatalf("stopDoorProcess: %v", err)
	}
}

// TestStopEngineProcessSignalsProcessGroup checks the engine path reaches the
// engine's own process group, which is how the engine's runners die with it
// on Unix.
func TestStopEngineProcessSignalsProcessGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process")
	}
	// A group leader, exactly as engineSysProcAttr starts the engine.
	cmd, dir, exited := spawnSignalChild(t, &syscall.SysProcAttr{Setpgid: true})
	if err := stopEngineProcess(cmd.Process); err != nil {
		t.Fatalf("stopEngineProcess: %v", err)
	}
	waitExit(t, "engine", cmd.Process.Pid, exited)
	if err := sawSIGTERM(dir); err != nil {
		t.Fatalf("stopEngineProcess: %v", err)
	}
}

// spawnSignalChild re-execs the test binary as a child that catches SIGTERM
// and records it. It returns the child and the directory the child reports
// in, and returns only once the child's handler is live, so a stop call right
// after cannot race the child's set-up.
func spawnSignalChild(t *testing.T, attr *syscall.SysProcAttr) (*exec.Cmd, string, <-chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestEngineStopHelperUnixProcess$", "-test.v=false")
	cmd.Env = append(os.Environ(),
		engineStopHelperEnv+"=unix", engineStopHelperDirEnv+"="+dir)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = attr
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a child process here: %v", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	if err := waitFor(filepath.Join(dir, "ready")); err != nil {
		t.Fatalf("the signal helper never installed its handler: %v", err)
	}
	return cmd, dir, exited
}

// sawSIGTERM reports whether the child caught SIGTERM: a Kill leaves no
// record at all, which is exactly what it must be told apart from.
func sawSIGTERM(dir string) error {
	if err := waitFor(filepath.Join(dir, "term")); err != nil {
		return errors.New("the child was killed rather than signalled (no SIGTERM was caught)")
	}
	return nil
}

// waitFor blocks until path exists.
func waitFor(path string) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("timed out waiting for " + path)
}

// TestEngineStopHelperUnixProcess is not a test. It is the child half of the
// two tests above: it catches SIGTERM, records that it did, and exits 0.
func TestEngineStopHelperUnixProcess(t *testing.T) {
	if os.Getenv(engineStopHelperEnv) != "unix" {
		t.Skip("child-process helper for the unix stop tests")
	}
	dir := os.Getenv(engineStopHelperDirEnv)
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM)
	// The handler is live: tell the parent, then wait to be stopped.
	if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	<-c
	if err := os.WriteFile(filepath.Join(dir, "term"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}
