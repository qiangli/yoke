package binmgr

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestMain lets the test binary impersonate a long-running managed process: with
// $BINMGR_TEST_SLEEP set it just sleeps then exits, so Launch/Stop can drive a
// real executable on every platform without shipping a fixture binary.
func TestMain(m *testing.M) {
	if os.Getenv("BINMGR_TEST_SLEEP") != "" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Launch a real process (this test binary), then Stop it gracefully.
func TestLaunchAndStop(t *testing.T) {
	p, err := Launch(context.Background(), os.Args[0], RunSpec{
		Env:    []string{"BINMGR_TEST_SLEEP=1"},
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if p.Pid() <= 0 {
		t.Fatalf("expected a pid, got %d", p.Pid())
	}
	start := time.Now()
	if err := p.Stop(5 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("Stop took too long (graceful term didn't work): %s", time.Since(start))
	}
}

// waitHealth returns once the endpoint answers non-5xx.
func TestWaitHealth_Ready(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable) // not ready yet
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := waitHealth(context.Background(), srv.URL, 5*time.Second); err != nil {
		t.Fatalf("waitHealth: %v", err)
	}
}

// waitHealth times out when the endpoint never becomes ready.
func TestWaitHealth_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	if err := waitHealth(context.Background(), srv.URL, 400*time.Millisecond); err == nil {
		t.Fatal("expected a timeout error")
	}
}

// TestLaunchConfigureRunsBeforeStart pins the Configure seam: it must see the
// assembled command (path, args, dir, env) and its mutations must reach the
// started process. `bashy loom` uses it to set SysProcAttr.Credential so Gitea
// starts under an unprivileged account when loom itself is root.
func TestLaunchConfigureRunsBeforeStart(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var seen *exec.Cmd
	p, err := Launch(context.Background(), os.Args[0], RunSpec{
		Env:    []string{"BINMGR_TEST_SLEEP=1"},
		Dir:    dir,
		Stdout: io.Discard,
		Stderr: io.Discard,
		Configure: func(cmd *exec.Cmd) {
			seen = cmd
			if cmd.Process != nil {
				t.Error("Configure ran after Start — process attributes would be too late")
			}
		},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer func() { _ = p.Stop(5 * time.Second) }()
	if seen == nil {
		t.Fatal("Configure was never called")
	}
	if seen.Path != p.Cmd.Path || seen.Dir != dir {
		t.Fatalf("Configure saw path %q dir %q, want %q / %q", seen.Path, seen.Dir, p.Cmd.Path, dir)
	}
	if len(seen.Env) == 0 {
		t.Fatal("Configure saw an unassembled env")
	}
}

// A nil Configure is the normal case and must not be called.
func TestLaunchWithoutConfigure(t *testing.T) {
	p, err := Launch(context.Background(), os.Args[0], RunSpec{
		Env:    []string{"BINMGR_TEST_SLEEP=1"},
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	_ = p.Stop(5 * time.Second)
}
