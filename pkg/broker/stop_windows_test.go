//go:build windows

package broker

import (
	"os"
	"os/exec"
	"testing"
)

// TestStopDoorProcessKillsNotSignals is the reported bug, pinned: `bashy llm
// down` answered "not supported by windows" because the unix path sent
// SIGTERM. On Windows Kill is the only verb there is, so the door helper must
// use it and must stop the door.
func TestStopDoorProcessKillsNotSignals(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process")
	}
	cmd, exited := spawnStopHelper(t)
	if err := stopDoorProcess(cmd.Process); err != nil {
		t.Fatalf("stopDoorProcess: %v (Signal is not supported by windows)", err)
	}
	waitExit(t, "door", cmd.Process.Pid, exited)
}

// TestStopDoorProcessGracefulCtrlBreak is the Sprint 455 F9 fix, pinned: a
// door started the way the real one is (detachSysProcAttr —
// CREATE_NEW_PROCESS_GROUP, breakaway, detached, no console) and that
// handles os.Interrupt the way RunDoor does must exit 0 when stopped,
// because bashy supervisord's on-failure restart policy restarts a unit on
// any nonzero exit. Before this fix, stopDoorProcess always called Kill,
// which TerminateProcess always reports as exit code 1 regardless of why
// the door was asked to stop.
func TestStopDoorProcessGracefulCtrlBreak(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestEngineStopHelperProcess$", "-test.v=false")
	cmd.Env = append(os.Environ(), engineStopHelperEnv+"=1", gracefulStopHelperEnv+"=1")
	cmd.SysProcAttr = detachSysProcAttr()
	if err := startDetached(cmd); err != nil {
		t.Skipf("cannot spawn a child process here: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	if err := stopDoorProcess(cmd.Process); err != nil {
		t.Fatalf("stopDoorProcess: %v", err)
	}
	state, err := cmd.Process.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if code := state.ExitCode(); code != 0 {
		t.Fatalf("graceful stop exit code = %d, want 0 (a hard Kill reports 1 and would make bashy supervisord restart the unit)", code)
	}
}

// TestStopEngineProcessClosesJobObject covers the engine half: the runners the
// engine spawned die with it only if the kill-on-close job object is closed,
// because Kill on Windows terminates one process.
func TestStopEngineProcessClosesJobObject(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process")
	}
	cmd, exited := spawnStopHelper(t)
	if err := putEngineInJob(cmd.Process.Pid); err != nil {
		t.Fatalf("putEngineInJob: %v", err)
	}
	if err := stopEngineProcess(cmd.Process); err != nil {
		t.Fatalf("stopEngineProcess: %v", err)
	}
	waitExit(t, "engine", cmd.Process.Pid, exited)
	if engineJobHandle != 0 {
		t.Fatal("stopEngineProcess left the engine job object open, so the engine's runners would survive the door")
	}
	if err := stopEngineProcess(cmd.Process); err != nil {
		t.Fatalf("stopEngineProcess without a job: %v", err)
	}
}
