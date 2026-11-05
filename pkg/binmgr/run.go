package binmgr

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/qiangli/yoke/pkg/telemetry"
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
	// Configure, when set, is called with the assembled command just before it
	// is started. It is the one seam for per-OS process attributes binmgr must
	// not know about: `bashy loom` running as root sets
	// SysProcAttr.Credential here so Gitea starts under a dedicated
	// unprivileged account (external/loom/privdrop.go).
	Configure func(*exec.Cmd)
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
	if spec.Configure != nil {
		spec.Configure(cmd)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("binmgr: start %s: %w", path, err)
	}
	p := &Process{Path: path, Cmd: cmd}
	if spec.HealthURL != "" {
		wait := spec.HealthWait
		if wait <= 0 {
			wait = 30 * time.Second
		}
		if err := waitHealth(ctx, spec.Tool.Name, spec.HealthURL, wait); err != nil {
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
//
// Every poll passes through the TYPED status seam (telemetry.ServiceStatus /
// ServiceStatusQueryFailed), which records first observations and meaningful
// transitions and collapses identical repeats — a service that answers
// "healthy" every 200 ms for the whole wait window produces O(1) records, not
// one per poll. The seam also keeps a failed query (transport error) separate
// from a reported state, and reports the HTTP status as structured state: a
// 404 is "degraded", not "healthy", even though the readiness decision below
// (non-5xx = ready, unchanged from before) still accepts it.
func waitHealth(ctx context.Context, service, url string, max time.Duration) error {
	deadline := time.Now().Add(max)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		switch {
		case err != nil:
			telemetry.ServiceStatusQueryFailed(ctx, telemetry.StatusQueryFailure{
				Service: service, Target: url, Err: err.Error(), ObservedAt: time.Now().UTC(),
			})
		default:
			_ = resp.Body.Close()
			state, detail := probeState(resp.StatusCode)
			telemetry.ServiceStatus(ctx, telemetry.StatusObservation{
				Service: service, Target: url, State: state, Detail: detail, ObservedAt: time.Now().UTC(),
			})
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

// probeState maps one HTTP probe answer to the structured reported state. The
// exit of the probe command is NOT the service's state; this answer is.
func probeState(code int) (state, detail string) {
	switch {
	case code < 400:
		return telemetry.StatusHealthy, ""
	case code < 500:
		return telemetry.StatusDegraded, "HTTP " + strconv.Itoa(code)
	default:
		return telemetry.StatusError, "HTTP " + strconv.Itoa(code)
	}
}

func orWriter(w, def io.Writer) io.Writer {
	if w != nil {
		return w
	}
	return def
}
