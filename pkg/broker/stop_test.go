package broker

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// engineStopHelperEnv marks the child half of these tests: a process that
// does nothing but stay alive until something stops it.
const engineStopHelperEnv = "BASHY_BROKER_STOP_HELPER"

// TestEngineStopHelperProcess is not a test. It is the child the stop tests
// below spawn; with no signal handlers installed, the platform's default
// disposition is exactly what is under test.
func TestEngineStopHelperProcess(t *testing.T) {
	if os.Getenv(engineStopHelperEnv) == "" {
		t.Skip("child-process helper for the door/engine stop tests")
	}
	select {}
}

// spawnStopHelper starts that child. The returned channel closes once the
// child is reaped, so a test can prove it stopped rather than trusting a nil
// error.
func spawnStopHelper(t *testing.T) (cmd *exec.Cmd, exited <-chan struct{}) {
	t.Helper()
	cmd = exec.Command(os.Args[0], "-test.run=^TestEngineStopHelperProcess$", "-test.v=false")
	cmd.Env = append(os.Environ(), engineStopHelperEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a child process here: %v", err)
	}
	ch := make(chan struct{})
	go func() { _ = cmd.Wait(); close(ch) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-ch
	})
	return cmd, ch
}

// waitExit reports whether the child exited before the timeout.
func waitExit(t *testing.T, tag string, pid int, exited <-chan struct{}) bool {
	t.Helper()
	select {
	case <-exited:
		return true
	case <-time.After(15 * time.Second):
		t.Fatalf("the %s process (pid %d) is still running after the stop helper returned", tag, pid)
		return false
	}
}

// TestStopDoorProcessStopsLiveChild is the `bashy llm down` path: a live door
// pid must be stopped by stopDoorProcess with no error. Before the Windows
// fix this failed exactly as reported — Signal answers "not supported by
// windows" and the door keeps running.
func TestStopDoorProcessStopsLiveChild(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process")
	}
	cmd, exited := spawnStopHelper(t)
	if err := stopDoorProcess(cmd.Process); err != nil {
		t.Fatalf("stopDoorProcess: %v", err)
	}
	waitExit(t, "door", cmd.Process.Pid, exited)
}

// TestStopEngineProcessStopsChild is the engine half of a door shutdown: the
// engine child must be stopped by stopEngineProcess, on every port, without
// waiting for the 10s timeout in ExecEngine.Stop to Kill it.
func TestStopEngineProcessStopsChild(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process")
	}
	cmd, exited := spawnStopHelper(t)
	if err := stopEngineProcess(cmd.Process); err != nil {
		t.Fatalf("stopEngineProcess: %v", err)
	}
	waitExit(t, "engine", cmd.Process.Pid, exited)
}
