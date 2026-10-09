//go:build windows

package broker

import (
	"os"
)

// stopDoorProcess stops the door by pid, as `llm down` does.
//
// Windows has no signals: os.Process.Signal answers "not supported by
// windows" for everything but Kill, which is why `bashy llm down` failed
// there (Sprint 379). Kill is the one verb Windows offers for a pid the
// caller does not own, so that is what down uses here. The door's engine
// child is not orphaned by it: stopEngineProcess closes the job object the
// engine was started in, which takes the engine and its runners down too.
func stopDoorProcess(p *os.Process) error { return p.Kill() }

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
