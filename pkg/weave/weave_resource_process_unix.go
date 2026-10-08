//go:build !windows

package weave

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

func weavePrepareOwnedChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
func weaveOwnedChildGroup(pid int) (int, error) {
	group, err := syscall.Getpgid(pid)
	if err != nil {
		return 0, err
	}
	if group != pid {
		return 0, errors.New("child did not enter its isolated process group")
	}
	return group, nil
}

// A group probe covers the child session weave created. Descendants that
// deliberately leave that session are outside this portable proof; workloads
// allowed to do that need a cgroup/job-object lifetime backend before release.
func weaveRecordedGroupStopped(it *weaveItem) error {
	if it.ChildPID <= 0 || it.ChildStartID == "" || it.ChildGroup != it.ChildPID {
		return errors.New("child birth identity and isolated group were not recorded at launch; retain reservation and inspect the owned process tree")
	}
	if err := syscall.Kill(-it.ChildGroup, 0); err != syscall.ESRCH {
		if err == nil || err == syscall.EPERM {
			return errors.New("recorded child process group may still contain processes")
		}
		return fmt.Errorf("inspect recorded child process group: %w", err)
	}
	return nil
}

// weaveOwnProvisionGroup names this wrapper's process group only when the
// wrapper leads it; a shared launcher group would contain unrelated peers.
func weaveOwnProvisionGroup() int {
	if pid := os.Getpid(); syscall.Getpgrp() == pid {
		return pid
	}
	return 0
}
func weaveProvisionGroupStopped(group int) error {
	if group <= 0 {
		return errors.New("provisioning group was not recorded at admission")
	}
	if err := syscall.Kill(-group, 0); err != syscall.ESRCH {
		if err == nil || err == syscall.EPERM {
			return errors.New("recorded provisioning group may still contain the wrapper or its provisioning subprocesses")
		}
		return fmt.Errorf("inspect recorded provisioning group: %w", err)
	}
	return nil
}

// weaveStopVerifiedChildGroup stops a recorded isolated child group whose
// wrapper is gone. Each signal is preceded by a fresh check that the group
// leader still carries its recorded birth identity: the kernel never reuses a
// PID while its group exists, so a verified live leader means the group is the
// one weave created. A leader that cannot be verified (exited, reused, or no
// observer) is never signalled; surviving descendants retain the reservation.
func weaveStopVerifiedChildGroup(ctx context.Context, it *weaveItem, grace time.Duration) error {
	if it.ChildPID <= 0 || it.ChildStartID == "" || it.ChildGroup != it.ChildPID {
		return errors.New("child birth identity and isolated group were not recorded at launch; refusing to signal")
	}
	gone := func() bool { return syscall.Kill(-it.ChildGroup, 0) == syscall.ESRCH }
	if gone() {
		return nil
	}
	lookup := weaveResourceHooks(ctx).LookupIdentity
	verify := func() error {
		if lookup == nil {
			return errors.New("native process identity lookup unavailable; refusing to signal")
		}
		lookupCtx, cancel := context.WithTimeout(ctx, time.Second)
		id, err := lookup(lookupCtx, it.ChildPID)
		cancel()
		if err != nil || id == "" {
			return fmt.Errorf("child group leader %d birth identity unverifiable (%v); refusing to signal its group", it.ChildPID, err)
		}
		if id != it.ChildStartID {
			return fmt.Errorf("child PID %d was reused; refusing to signal", it.ChildPID)
		}
		return nil
	}
	wait := func(d time.Duration) bool {
		deadline := time.Now().Add(d)
		for !gone() {
			if ctx.Err() != nil || time.Now().After(deadline) {
				return false
			}
			time.Sleep(50 * time.Millisecond)
		}
		return true
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if err := verify(); err != nil {
			if gone() {
				return nil
			}
			return err
		}
		if err := syscall.Kill(-it.ChildGroup, sig); err != nil && err != syscall.ESRCH {
			return fmt.Errorf("signal child group %d: %w", it.ChildGroup, err)
		}
		if wait(grace) {
			return nil
		}
	}
	return errors.New("recorded child process group may still contain processes after SIGKILL")
}
func weaveConfigureOwnedCancellation(cmd *exec.Cmd) {
	// Cancellation targets this child's process group, never the wrapper's peers.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
func weaveAbortOwnedChild(cmd *exec.Cmd) {
	if cmd.Cancel != nil {
		_ = cmd.Cancel()
	} else if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
func weaveOwnedChildTerminated(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return true
	} // start failed
	if cmd.ProcessState == nil {
		return false
	}
	// Wait alone is insufficient: descendants may outlive the direct child.
	return syscall.Kill(-cmd.Process.Pid, 0) == syscall.ESRCH
}

// Recheck the stored birth identity before every signal. Unknown/reused PIDs
// never grant authority; only the explicit owner's pause reaches this helper.
func weaveStopVerifiedWrapper(ctx context.Context, pid int, startID string, checkpoint func() bool) error {
	it := &weaveItem{WrapperPid: pid, WrapperStartID: startID}
	if err := weaveVerifiedWrapper(ctx, it); err != nil {
		return err
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	timer := time.NewTicker(50 * time.Millisecond)
	defer timer.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			if checkpoint != nil && checkpoint() {
				return nil
			}
			if !pidAlive(pid) {
				return nil
			}
		case <-deadline.C:
			// The wrapper may still be preserving progress. Do not kill it and falsely
			// claim its isolated child session has terminated; report pending pause.
			return errors.New("pause requested; waiting for wrapper to preserve progress and exit")
		}
	}
}

func weaveWatchPlainTermination(cancel context.CancelFunc) func() {
	signals := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case <-signals:
			cancel()
		case <-done:
		}
	}()
	return func() { signal.Stop(signals); close(done) }
}
