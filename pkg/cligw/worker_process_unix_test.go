//go:build unix

package cligw

import (
	"context"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestWorkerCancelKillsProcessGroup(t *testing.T) {
	installFakeCatalog(t, "fakegroup", WarmCold, "group")
	worker, err := NewWorker(context.Background(), "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	pidFile := t.TempDir() + "/child.pid"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := worker.Do(ctx, pidFile, nil)
		done <- err
	}()
	var childPID int
	waitFor(t, 3*time.Second, func() bool {
		body, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		childPID, err = strconv.Atoi(string(body))
		return err == nil && childPID > 0
	})
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled worker returned nil error")
	}
	waitFor(t, 3*time.Second, func() bool {
		return syscall.Kill(childPID, 0) == syscall.ESRCH
	})
}
