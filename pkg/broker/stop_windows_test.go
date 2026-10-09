//go:build windows

package broker

import (
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
