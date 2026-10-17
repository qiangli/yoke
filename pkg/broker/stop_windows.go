//go:build windows

package broker

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// doorStopGrace bounds how long stopDoorProcess waits for the door to exit
// on its own after a graceful CTRL_BREAK_EVENT before falling back to Kill.
const doorStopGrace = 5 * time.Second

// stillActive is GetExitCodeProcess's "has not exited yet" pseudo exit code
// (STATUS_PENDING); x/sys/windows does not export it.
const stillActive = 259

// stopDoorProcess stops the door by pid, as `llm down` does.
//
// Windows has no signals: os.Process.Signal answers "not supported by
// windows" for everything but Kill, which is why `bashy llm down` failed
// there (Sprint 379). But Kill means TerminateProcess, which the Go runtime
// calls with a hardcoded exit code of 1 — a plain Kill always looks like a
// crash to bashy supervisord's on-failure restart policy, even for a
// deliberate stop, and raced the unit's disable into one spurious restart
// (Sprint 455 F9).
//
// The door is started with CREATE_NEW_PROCESS_GROUP (detachSysProcAttr),
// which makes its own pid double as the id of its own, single-member
// console process group, so GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT, pid)
// can target it specifically and reliably — the one case where a Windows
// process can be reached individually through the console mechanism (see
// sh/interp/sendsignal_windows.go for why every other case is unreliable
// and avoided there). Go's runtime maps CTRL_BREAK_EVENT to os.Interrupt
// (runtime.ctrlHandler), which RunDoor already listens for via
// signal.NotifyContext, so a door that receives it runs its ordinary
// graceful shutdown and exits 0. Kill stays as the fallback for a door that
// does not exit within doorStopGrace — wedged, or a binary built before
// this fix that never installed the handler.
func stopDoorProcess(p *os.Process) error {
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(p.Pid)); err == nil && waitDoorExit(p.Pid, doorStopGrace) {
		return nil
	}
	return p.Kill()
}

// waitDoorExit polls pid's own exit code on a handle separate from
// *os.Process (rather than p.Wait, which would race the fallback p.Kill
// over the same handle's lifecycle) until it stops being STILL_ACTIVE or
// timeout passes.
func waitDoorExit(pid int, timeout time.Duration) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return true // already gone
	}
	defer windows.CloseHandle(h)
	deadline := time.Now().Add(timeout)
	for {
		var code uint32
		if err := windows.GetExitCodeProcess(h, &code); err != nil || code != stillActive {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stopEngineProcess stops the engine and everything it spawned. Kill on
// Windows terminates one process, not a tree, and the engine has children of
// its own (`bashy ollama serve` starts Ollama, which starts runners). The one
// mechanism the platform gives a parent for a whole subtree is a job object
// with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so the engine is started in one
// (engineJobHook) and closing it here stops the tree.
func stopEngineProcess(p *os.Process) error {
	if closed, err := closeEngineJob(); closed {
		return err
	}
	// No job: an engine started before this fix, or one whose job object
	// could not be created. Kill the engine itself and let its children be.
	return p.Kill()
}
