package binmgr

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

// RunSpec describes how to launch a managed binary. The caller supplies the args
// (e.g. binding the tool to a loopback port) and an optional readiness probe; the
// supervisor handles ensure → launch → wait-ready → stop. Shared by `bashy loom`
// (foreground) and the outpost wrap-harness builtin (background daemon).
type RunSpec struct {
	Tool       Tool          // the binary to Ensure + run
	Args       []string      // command-line args
	Env        []string      // extra env, appended to the parent environment
	Dir        string        // working directory ("" inherits)
	Stdout     io.Writer     // default os.Stdout
	Stderr     io.Writer     // default os.Stderr
	HealthURL  string        // optional: GET until it answers (< 500) to consider ready
	HealthWait time.Duration // max wait for readiness (default 30s)
}

// Process is a launched managed binary.
type Process struct {
	Path string
	Cmd  *exec.Cmd
}

// Start ensures the binary is present (download/verify/cache), launches it, and —
// if HealthURL is set — blocks until it answers or HealthWait elapses (killing it
// on timeout). Returns immediately once ready (or once launched, if no probe).
func Start(ctx context.Context, spec RunSpec) (*Process, error) {
	path, err := Ensure(ctx, spec.Tool)
	if err != nil {
		return nil, err
	}
	return Launch(ctx, path, spec)
}

// Launch runs an already-resolved binary path (the half of Start after Ensure).
// Useful when the caller resolved the path itself, or in tests.
func Launch(ctx context.Context, path string, spec RunSpec) (*Process, error) {
	cmd := exec.Command(path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = append(os.Environ(), spec.Env...)
	cmd.Stdout = orWriter(spec.Stdout, os.Stdout)
	cmd.Stderr = orWriter(spec.Stderr, os.Stderr)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("binmgr: start %s: %w", path, err)
	}
	p := &Process{Path: path, Cmd: cmd}
	if spec.HealthURL != "" {
		wait := spec.HealthWait
		if wait <= 0 {
			wait = 30 * time.Second
		}
		if err := waitHealth(ctx, spec.HealthURL, wait); err != nil {
			_ = p.Stop(5 * time.Second)
			return nil, err
		}
	}
	return p, nil
}

// Pid returns the running process id, or 0 if not started.
func (p *Process) Pid() int {
	if p.Cmd != nil && p.Cmd.Process != nil {
		return p.Cmd.Process.Pid
	}
	return 0
}

// Wait blocks until the process exits.
func (p *Process) Wait() error { return p.Cmd.Wait() }

// Stop asks the process to terminate (SIGTERM on unix, Kill on Windows), then
// hard-kills it if it hasn't exited within timeout. Idempotent.
func (p *Process) Stop(timeout time.Duration) error {
	if p.Cmd == nil || p.Cmd.Process == nil {
		return nil
	}
	_ = terminate(p.Cmd.Process)
	done := make(chan error, 1)
	go func() { done <- p.Cmd.Wait() }()
	select {
	case <-time.After(timeout):
		_ = p.Cmd.Process.Kill()
		<-done
		return nil
	case <-done:
		return nil
	}
}

func terminate(proc *os.Process) error {
	if runtime.GOOS == "windows" {
		return proc.Kill() // no SIGTERM semantics on Windows
	}
	return proc.Signal(syscall.SIGTERM)
}

// waitHealth polls url until it returns a non-5xx response or the deadline.
func waitHealth(ctx context.Context, url string, max time.Duration) error {
	deadline := time.Now().Add(max)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("binmgr: %s not healthy after %s", url, max)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func orWriter(w, def io.Writer) io.Writer {
	if w != nil {
		return w
	}
	return def
}
