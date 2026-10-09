//go:build !windows

package broker

import (
	"os"
	"syscall"
)

// stopDoorProcess stops the door by pid, as `llm down` does. Unix keeps the
// graceful path it has always had: SIGTERM, and the door shuts the engine
// down in order. Kill stays with the operator (or a wedged-door escalation).
func stopDoorProcess(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}

// stopEngineProcess stops the engine and its children. The engine runs in
// its own process group (engineSysProcAttr), so the negative pid reaches the
// whole tree and the door does not have to know its children.
func stopEngineProcess(p *os.Process) error {
	if err := syscall.Kill(-p.Pid, syscall.SIGTERM); err != nil {
		// Not a leader (or already gone): fall back to the engine itself.
		return p.Signal(syscall.SIGTERM)
	}
	return nil
}
