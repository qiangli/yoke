//go:build !windows

package chat

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The defect these encode: exec.CommandContext kills exactly one pid, so a
// wedged agent's grandchildren survived the per-turn deadline still holding the
// stdout pipe. The turn ran past its budget (the 2026-07-18 artifact shows a
// ycode turn that persisted exit 124 after 20 minutes) and the descendants
// orphaned.
//
// A real child tree is the only honest way to test this — a fake Runner never
// forks — so these use /bin/sh to build a two-level tree. They are Unix-only:
// process groups are the mechanism under test.

// alive reports whether a pid is still running. ESRCH means gone; EPERM means
// alive but not ours.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// waitGone waits for a pid to disappear, bounded by a deadline. A reap is
// inherently asynchronous — the kill is delivered, the process must then be
// scheduled and torn down — so this polls rather than sleeping a fixed guess,
// and fails at the deadline instead of hoping a sleep was long enough.
func waitGone(t *testing.T, pid int, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return !alive(pid)
}

// readPID waits for the helper to publish its grandchild's pid, bounded.
func readPID(t *testing.T, path string, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper never published a grandchild pid at %s", path)
	return 0
}

// hungTreeScript builds a shell command that forks a long-lived GRANDCHILD
// (which inherits our stdout pipe) and then hangs itself. Killing only the
// direct child leaves the grandchild holding the pipe — the exact shape that
// wedged a turn past its budget.
func hungTreeScript(pidFile string) string {
	return "sleep 120 & echo $! > " + pidFile + "; sleep 120"
}

//  3. A hung child must return within its budget, report a timeout, and take its
//     descendants with it.
func TestHungChildTimesOutAndKillsDescendants(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on this host")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")

	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, exit, err := execRunner{}.Run(ctx, "sh", []string{"-c", hungTreeScript(pidFile)}, dir)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a killed turn must report an error")
	}
	if exit != 124 {
		t.Errorf("exit = %d, want 124 (timeout)", exit)
	}
	// Bounded return. The pre-fix behaviour blocked on the grandchild's pipe
	// until WaitDelay (5s) expired; the group kill closes it immediately.
	if elapsed > 3*time.Second {
		t.Errorf("Run took %s — it waited on a grandchild's pipe instead of killing the tree", elapsed)
	}

	// The whole point: no orphan.
	grandchild := readPID(t, pidFile, 2*time.Second)
	if !waitGone(t, grandchild, 3*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL) // do not leak into the test host
		t.Errorf("grandchild %d survived the turn's deadline — it orphaned", grandchild)
	}
}

//  4. Caller cancellation is the same requirement by a different trigger: the
//     tree goes, and the call returns promptly.
func TestCancelKillsDescendants(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on this host")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var exit int
	var err error
	go func() {
		defer close(done)
		_, exit, err = execRunner{}.Run(ctx, "sh", []string{"-c", hungTreeScript(pidFile)}, dir)
	}()

	// Cancel only once the tree actually exists, so this tests teardown rather
	// than racing the launch.
	grandchild := readPID(t, pidFile, 5*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })
	start := time.Now()
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run hung after cancellation (10s guard exceeds 5s WaitDelay)")
	}
	if elapsed := time.Since(start); elapsed >= 3*time.Second {
		t.Errorf("cancellation took %s (want <3s, independently of 10s hang guard): %v", elapsed, err)
	}
	if err == nil {
		t.Error("a cancelled turn must report an error")
	}
	if exit != 124 {
		t.Errorf("exit = %d, want 124", exit)
	}
	if !waitGone(t, grandchild, 3*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Errorf("grandchild %d survived cancellation — it orphaned", grandchild)
	}
}

// Model a successful group signal whose membership snapshot missed a process
// forked concurrently. The first call really kills the leader, while the live
// descendant retains the pipe; the next call must reach that same group. This
// exercises real processes and pipe EOF without depending on a kernel race.
func TestCancelRetriesMissedDescendant(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", hungTreeScript(pidFile))
	cmd.Cancel = nil
	cmd.WaitDelay = 5 * time.Second
	setProcessGroup(cmd)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = killProcessTree(cmd) })
	grandchild := readPID(t, pidFile, 5*time.Second)
	pgid, err := syscall.Getpgid(grandchild)
	if err != nil || pgid != cmd.Process.Pid {
		t.Fatalf("descendant group = %d, %v; want %d", pgid, err, cmd.Process.Pid)
	}
	calls := 0
	kill := func() error {
		calls++
		if calls == 1 {
			return cmd.Process.Kill()
		}
		return killProcessTree(cmd)
	}
	start := time.Now()
	cancel()
	done := make(chan error, 1)
	go func() { done <- waitForProcessTree(ctx, cmd, kill) }()
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait hung after cancellation with a missed descendant")
	}
	if elapsed := time.Since(start); elapsed >= 3*time.Second {
		t.Errorf("cancellation took %s (want <3s): %v", elapsed, err)
	}
	if calls < 2 || !errors.Is(err, context.Canceled) {
		t.Errorf("kill calls=%d, error=%v; want retry and cancellation", calls, err)
	}
	if !waitGone(t, grandchild, 3*time.Second) {
		t.Errorf("grandchild %d survived a successful first signal", grandchild)
	}
}

func TestKillProcessTreeRetainsFallbackError(t *testing.T) {
	// Intentionally omit setProcessGroup so -pid is absent and the real
	// syscall takes the ESRCH -> direct-process fallback branch.
	cmd := exec.Command("/bin/sleep", "120")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	err := killProcessTree(cmd)
	if !errors.Is(err, syscall.ESRCH) || !strings.Contains(err.Error(), "fallback kill pid succeeded") {
		t.Fatalf("kill error = %v; want group ESRCH and successful pid fallback", err)
	}
}

func TestCancelDoesNotRetryAbsentProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Hold Wait open across retry ticks after the simulated ESRCH. A vanished
	// group's numeric id must never be signalled again, even while Wait drains.
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	cmd := exec.CommandContext(ctx, "/bin/cat")
	cmd.Stdin = reader
	cmd.Cancel = nil
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	calls := 0
	kill := func() error {
		calls++
		if calls == 1 {
			time.AfterFunc(100*time.Millisecond, func() { _ = writer.Close() })
		}
		return syscall.ESRCH
	}
	cancel()
	err = waitForProcessTree(ctx, cmd, kill)
	if calls != 1 {
		t.Errorf("kill called %d times after ESRCH; want exactly once", calls)
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, syscall.ESRCH) {
		t.Errorf("teardown lost cancellation or group errno: %v", err)
	}
}

// A wrapper may exit before its descendant closes the inherited output pipes.
// os/exec stops its context watcher once the direct child is reaped, although
// Cmd.Wait is still draining those pipes. Cancellation must still reach the
// process group during that interval.
func TestCancelKillsDescendantsAfterLeaderExit(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	leaderFile := filepath.Join(dir, "leader.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var exit int
	var err error
	go func() {
		defer close(done)
		_, exit, err = execRunner{}.Run(ctx, "/bin/sh", []string{"-c",
			"sleep 120 & echo $! > " + pidFile + "; echo $$ > " + leaderFile}, dir)
	}()
	grandchild := readPID(t, pidFile, 5*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })
	leader := readPID(t, leaderFile, 5*time.Second)
	if !waitGone(t, leader, 3*time.Second) {
		t.Fatal("wrapper has not exited and been reaped")
	}
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run hung after cancellation while draining descendant pipes")
	}
	if elapsed := time.Since(start); elapsed >= 3*time.Second {
		t.Errorf("cancellation took %s (want <3s, independently of 10s hang guard): %v", elapsed, err)
	}
	if exit != 124 || err == nil {
		t.Errorf("Run = exit %d, error %v; want cancellation exit 124", exit, err)
	}
	if !waitGone(t, grandchild, 3*time.Second) {
		t.Errorf("grandchild %d survived cancellation after leader exit", grandchild)
	}
}

// TestParentExitKillsDescendants proves the failure mode a context cannot
// cover: SIGKILL of the meet process before it gets to cancel its turn.
func TestParentExitKillsDescendants(t *testing.T) {
	if os.Getenv("BASHY_PARENT_EXIT_HELPER") == "1" {
		dir := os.Getenv("BASHY_PARENT_EXIT_DIR")
		pidFile := filepath.Join(dir, "grandchild.pid")
		_, _, _ = (execRunner{killOnParentExit: true}).Run(context.Background(), "sh", []string{"-c", hungTreeScript(pidFile)}, dir)
		return
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on this host")
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestParentExitKillsDescendants", "-test.v=false")
	cmd.Env = append(os.Environ(), "BASHY_PARENT_EXIT_HELPER=1", "BASHY_PARENT_EXIT_DIR="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	grandchild := readPID(t, filepath.Join(dir, "grandchild.pid"), 5*time.Second)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("helper should be killed")
	}
	if !waitGone(t, grandchild, 5*time.Second) {
		t.Errorf("grandchild %d survived its meet parent dying", grandchild)
	}
}

// The teardown must not change what a NORMAL turn returns. Putting the child in
// its own process group is invisible to a command that simply runs and exits.
func TestKillOnParentExitDoesNotChangeNormalRuns(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on this host")
	}
	out, exit, err := (execRunner{killOnParentExit: true}).Run(context.Background(), "sh",
		[]string{"-c", "echo hello from the agent"}, t.TempDir())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	if strings.TrimSpace(out) != "hello from the agent" {
		t.Errorf("out = %q", out)
	}
}

// A non-zero exit is still classified as one, not swallowed by the new cancel
// path.
func TestNonZeroExitIsPreserved(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on this host")
	}
	_, exit, err := execRunner{}.Run(context.Background(), "sh",
		[]string{"-c", "exit 3"}, t.TempDir())
	if err == nil {
		t.Fatal("a non-zero exit must report an error")
	}
	if exit != 3 {
		t.Errorf("exit = %d, want 3", exit)
	}
}

// setProcessGroup must actually take effect, or the group kill silently
// degrades to a single-pid kill and everything above passes for the wrong
// reason.
func TestSetProcessGroupPutsChildInItsOwnGroup(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on this host")
	}
	cmd := exec.Command("sh", "-c", "sleep 5")
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = killProcessTree(cmd); _, _ = cmd.Process.Wait() }()

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	if pgid != cmd.Process.Pid {
		t.Errorf("child pgid = %d, want its own pid %d", pgid, cmd.Process.Pid)
	}
	if pgid == syscall.Getpgrp() {
		t.Error("child shares the parent's process group; a group kill would signal the test runner")
	}
}
