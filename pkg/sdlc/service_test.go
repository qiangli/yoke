package sdlc

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestServiceStatusLifecycle(t *testing.T) {
	dir := t.TempDir()
	opt := ServiceOptions{RunsDir: dir + "/runs"}

	// Nothing running yet.
	if st := ServiceStatusOf(opt); st.Running {
		t.Fatal("should not be running before start")
	}

	// A pidfile pointing at THIS live process reads as running.
	if err := writePid(servicePidPath(opt.RunsDir), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if st := ServiceStatusOf(opt); !st.Running || st.PID != os.Getpid() {
		t.Fatalf("expected running with our pid, got %+v", st)
	}

	// A pidfile pointing at a dead pid reads as stopped.
	if err := writePid(servicePidPath(opt.RunsDir), 2147480000); err != nil {
		t.Fatal(err)
	}
	if st := ServiceStatusOf(opt); st.Running {
		t.Fatal("dead pid should read as stopped")
	}

	// StopService clears the pidfile.
	if _, err := StopService(opt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(servicePidPath(opt.RunsDir)); !os.IsNotExist(err) {
		t.Fatal("StopService should remove the pidfile")
	}
}

func TestStartServiceIdempotent(t *testing.T) {
	dir := t.TempDir()
	opt := ServiceOptions{RunsDir: dir + "/runs"}
	// Pre-write a pidfile for THIS process so StartService sees "already running"
	// and returns without spawning a child.
	if err := writePid(servicePidPath(opt.RunsDir), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	st, err := StartService(opt)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running || st.PID != os.Getpid() {
		t.Fatalf("idempotent start should report the existing pid, got %+v", st)
	}
}

func TestServeLoopRunsTicksUntilCancelled(t *testing.T) {
	dir := t.TempDir()
	opt := ServiceOptions{RunsDir: dir + "/runs", Interval: 5 * time.Millisecond}

	var ticks int32
	ctx, cancel := context.WithCancel(context.Background())
	tick := func(ctx context.Context) (DelegateResult, error) {
		if atomic.AddInt32(&ticks, 1) >= 3 {
			cancel() // stop after a few iterations
		}
		return DelegateResult{Status: "idle"}, nil
	}

	done := make(chan error, 1)
	go func() { done <- Serve(ctx, opt, tick) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not stop after context cancel")
	}
	if atomic.LoadInt32(&ticks) < 3 {
		t.Fatalf("expected >=3 ticks, got %d", ticks)
	}
	// Serve should have cleaned up its pidfile on exit.
	if _, err := os.Stat(servicePidPath(opt.RunsDir)); !os.IsNotExist(err) {
		t.Fatal("Serve should remove its pidfile on exit")
	}
}

// The status command must print a token outpost's supervisor greps for.
func TestServiceStatusCommandContract(t *testing.T) {
	dir := t.TempDir()
	cmd := NewSDLCCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"service", "status", "--runs-dir", dir + "/runs"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "stopped") {
		t.Fatalf("status of a not-started service must contain 'stopped', got: %q", out.String())
	}
}
