//go:build !windows

package chat

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// Killing a wedged agent means killing a TREE, not a process.
//
// The pipe runner used exec.CommandContext, whose cancellation kills exactly one
// pid: the agent CLI. But every agent CLI bashy drives spawns children — a shell
// shim, an MCP server, a language server — and those children INHERIT the write
// end of the stdout/stderr pipes. So the direct child dies, the grandchildren
// live, and two things go wrong at once: Wait blocks until the last pipe writer
// closes (WaitDelay bounds that, but only by abandoning the pipes), and the
// grandchildren keep running forever as orphans. The 2026-07-18 meeting artifact
// shows both — a turn stranded past its 20m budget, and descendants surviving a
// cancellation.
//
// The fix is to give the child its own process group at launch and signal the
// GROUP. agentpty already does the equivalent for the terminal path (see
// agentpty.Run's killTree); this is the pipe path's half, deliberately kept to
// process-group signalling so it stays pure-Go syscall with no `ps` snapshot.

// setProcessGroup puts a pipe-run child in a new session. Besides giving it a
// process group whose id equals its pid (so kill(-pid) reaches its ordinary
// descendants), this deliberately detaches it from the caller's controlling
// terminal. A headless child with inherited /dev/tty could otherwise run stty
// itself, corrupting the operator terminal even though its stdio is pipes.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// killProcessTree signals the child's whole process group.
//
// It sends SIGKILL rather than SIGTERM: this is only ever reached after the
// per-turn budget expired or the caller cancelled, both of which already mean
// "this agent has had its chance". A graceful window here would just be more
// wall-clock spent on a process that is by definition not responding.
//
// Falls back to killing the bare pid when the group is unavailable (the child
// raced us and exited, or was launched without its own group), so the caller
// never ends up with no kill at all. Preserve the group error even when the
// fallback succeeds, because a pid kill says nothing about descendants.
func killProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	groupErr := syscall.Kill(-pid, syscall.SIGKILL)
	if groupErr == nil {
		return nil
	}
	pidErr := cmd.Process.Kill()
	if pidErr == nil {
		return fmt.Errorf("kill process group %d: %w (fallback kill pid succeeded)", pid, groupErr)
	}
	return errors.Join(fmt.Errorf("kill process group %d: %w", pid, groupErr),
		fmt.Errorf("fallback kill pid %d: %w", pid, pidErr))
}

func processTreeGone(err error) bool { return errors.Is(err, syscall.ESRCH) }

func budgetOwnedGroupGone(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return true
	}
	if cmd.ProcessState == nil || cmd.SysProcAttr == nil || !(cmd.SysProcAttr.Setsid || cmd.SysProcAttr.Setpgid && cmd.SysProcAttr.Pgid == 0) {
		return false
	}
	return syscall.Kill(-cmd.Process.Pid, 0) == syscall.ESRCH
}
