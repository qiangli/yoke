//go:build !windows

package agentpty

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// TestParentExitKillsPTYTree covers the abrupt supervisor-death path used by
// bounded Meet turns. Context cancellation and Run's defers cannot execute
// after SIGKILL, so the pipe-EOF watcher must remove both the PTY leader and a
// descendant which inherited its process group.
func TestParentExitKillsPTYTree(t *testing.T) {
	if os.Getenv("BASHY_AGENTPTY_PARENT_EXIT_HELPER") == "1" {
		dir := os.Getenv("BASHY_AGENTPTY_PARENT_EXIT_DIR")
		cmd := exec.Command("sh", "-c", "echo $$ > leader.pid; sleep 120 & echo $! > child.pid; wait")
		cmd.Dir = dir
		_, _, _ = Run(cmd, io.Discard, Options{Capture: true, KillOnParentExit: true})
		return
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on this host")
	}

	dir := t.TempDir()
	helper := exec.Command(os.Args[0], "-test.run=^TestParentExitKillsPTYTree$", "-test.v=false")
	helper.Env = append(os.Environ(),
		"BASHY_AGENTPTY_PARENT_EXIT_HELPER=1",
		"BASHY_AGENTPTY_PARENT_EXIT_DIR="+dir,
	)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	leader := readParentWatchPID(t, filepath.Join(dir, "leader.pid"), 5*time.Second)
	child := readParentWatchPID(t, filepath.Join(dir, "child.pid"), 5*time.Second)
	defer func() {
		_ = syscall.Kill(leader, syscall.SIGKILL)
		_ = syscall.Kill(child, syscall.SIGKILL)
	}()

	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := helper.Wait(); err == nil {
		t.Fatal("helper should be killed")
	}
	if !waitParentWatchGone(leader, 5*time.Second) {
		t.Errorf("PTY leader %d survived its supervisor dying", leader)
	}
	if !waitParentWatchGone(child, 5*time.Second) {
		t.Errorf("PTY child %d survived its supervisor dying", child)
	}
}

func TestKillOnParentExitNormalPTYRun(t *testing.T) {
	cmd := exec.Command("sh", "-c", "printf normal-pty")
	var out strings.Builder
	exit, reason, err := Run(cmd, &out, Options{Capture: true, KillOnParentExit: true})
	if err != nil {
		t.Fatal(err)
	}
	if exit != 0 || reason != "" {
		t.Fatalf("exit=%d reason=%q", exit, reason)
	}
	if !strings.Contains(out.String(), "normal-pty") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestKillOnParentExitPreservesPTYControl(t *testing.T) {
	if os.Getenv("BASHY_AGENTPTY_CONTROL_HELPER") == "1" {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			os.Exit(2)
		}
		fg, err := unix.IoctlGetInt(int(os.Stdin.Fd()), unix.TIOCGPGRP)
		if err != nil {
			os.Exit(3)
		}
		if fg != syscall.Getpgrp() {
			os.Exit(4)
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestKillOnParentExitPreservesPTYControl$", "-test.v=false")
	cmd.Env = append(os.Environ(), "BASHY_AGENTPTY_CONTROL_HELPER=1")
	exit, reason, err := Run(cmd, io.Discard, Options{Capture: true, KillOnParentExit: true})
	if err != nil {
		t.Fatal(err)
	}
	if exit != 0 || reason != "" {
		t.Fatalf("exit=%d reason=%q", exit, reason)
	}
}

func TestKillOnParentExitPTYSignalStatus(t *testing.T) {
	cmd := exec.Command("sh", "-c", "kill -TERM $$")
	exit, reason, err := Run(cmd, io.Discard, Options{Capture: true, KillOnParentExit: true})
	if err != nil {
		t.Fatal(err)
	}
	if exit != 128+int(syscall.SIGTERM) || reason != "" {
		t.Fatalf("exit=%d reason=%q, want signal-derived exit %d", exit, reason, 128+int(syscall.SIGTERM))
	}
}

func readParentWatchPID(t *testing.T, path string, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 1 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper did not publish pid at %s", path)
	return 0
}

func waitParentWatchGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) == syscall.ESRCH
}
