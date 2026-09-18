// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// SSHTransport is the first remote Transport: it delivers a task's body to a
// worker host over an OpenSSH-compatible client, feeding the body to a remote
// shell on stdin exactly as the mesh executor does — but as a Transport, so it
// plugs into the pool beside localTransport instead of replacing the Executor.
// (meshExecutor and the --mesh path are retained unchanged until this
// migration is proven; the two are parallel by design.)
//
// Reach (where the machine is, who logs in, which port) comes from a
// pkg/fleet.Host consumed READ-ONLY at construction and held in unexported
// fields. It is deliberately never re-exported: the standing privacy rule is
// that no hostname, username, address, or port reaches a TaskResult, a
// RunRecord, or a committed artifact. Every error this transport constructs
// names only the worker's LOGICAL id, and undeliverability is classified
// through the contract sentinels (ErrWorkerUnreachable, context.Canceled) so
// RecordAttempt records an infra failure with a stable code and no detail —
// never a conformance verdict, and never a raw dial error. The client's own
// stderr (which may name the host) still streams to the task's stderr, which
// is operator-facing and not a committed artifact.
type SSHTransport struct {
	// Command is the client argv prefix — "ssh", "ssh -i key", or a test
	// fake. It must accept OpenSSH-style -l/-p flags when the host carries a
	// user or port. Empty means "ssh".
	Command string
	// Shell is the remote command the body is piped to ("bash -s" when
	// empty). "none" feeds the body directly to the remote command, for
	// endpoints that consume stdin themselves.
	Shell string

	// Reach, extracted from pkg/fleet.Host at construction. Unexported on
	// purpose — see the type comment.
	target string
	user   string
	port   int

	closed atomic.Bool
}

// sshWaitDelay bounds the graceful-termination window on cancellation: after
// SIGTERM the client gets this long to tear down the connection (which is what
// makes the remote side reap the process) before it is killed outright.
const sshWaitDelay = 5 * time.Second

// NewSSHTransport builds a transport that reaches one host. h is read once,
// here, and only its reach fields (Target(), SSHUser, SSHPort) are kept.
func NewSSHTransport(h fleet.Host) *SSHTransport {
	return &SSHTransport{
		target: h.Target(),
		user:   h.SSHUser,
		port:   h.SSHPort,
	}
}

// Exec runs one task on the remote worker. The body travels on stdin (never a
// command line); stdout/stderr stream back through io. Cancellation SIGTERMs
// the client so the connection closes and the remote session is reaped rather
// than orphaned, and reports context.Canceled — which RecordAttempt classifies
// as a distinct infra status (FailCanceled), not a conformance failure.
func (x *SSHTransport) Exec(ctx context.Context, w *Worker, t *Task, tio TaskIO) TaskResult {
	start := time.Now()
	res := TaskResult{Name: t.Name, Host: t.Host}
	worker := ""
	if w != nil {
		worker = w.ID
	}
	fail := func(err error) TaskResult {
		res.Status, res.ExitCode, res.Err = StatusFailed, 1, err
		res.Duration = time.Since(start)
		return res
	}
	if x.closed.Load() {
		return fail(fmt.Errorf("worker %q: ssh transport is closed: %w", worker, ErrWorkerUnreachable))
	}
	if x.target == "" {
		return fail(fmt.Errorf("worker %q: no dial target configured: %w", worker, ErrWorkerUnreachable))
	}

	name, args, shellMode := x.commandArgs()
	body := t.Body
	if shellMode {
		// The fleet env tag travels inside the body, because ssh does not
		// forward client env. Worker id and venue are LOGICAL facts (see
		// localTransport) — this is the one thing that may cross the wire.
		body = "export DAG_FLEET_WORKER=" + shellQuote(worker) +
			" DAG_FLEET_VENUE=" + shellQuote(VenueUserland) + "\n" + t.Body
	}

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(body)
	cmd.Env = append(append([]string{}, tio.Env...),
		"DAG_FLEET_WORKER="+worker,
		"DAG_FLEET_VENUE="+VenueUserland,
	)
	cmd.Stdout = tio.Stdout
	cmd.Stderr = tio.Stderr
	// Graceful cancel: SIGTERM lets the client close the channel, which is
	// what makes sshd HUP/reap the remote session instead of orphaning it. On
	// platforms where the signal is unsupported (windows), Signal errors and
	// WaitDelay kills the client after the grace window.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = sshWaitDelay

	err := cmd.Run()
	res.Duration = time.Since(start)
	if ctx.Err() != nil {
		// Cancelled (or deadline-hit — the engine reclassifies its own
		// per-target deadline as exit 124 "timeout" above this layer). The
		// body's exit, if any, is void: no verdict was earned.
		res.Status, res.ExitCode, res.Err = StatusFailed, 1, ctx.Err()
		return res
	}
	res.ExitCode, res.Err = exitCodeFromExecErr(err)
	switch {
	case res.Err != nil:
		// The client never produced an exit code (not found, not startable):
		// the worker was never reached, so no verdict exists.
		res.Err = fmt.Errorf("worker %q: ssh client failed to run (%v): %w", worker, res.Err, ErrWorkerUnreachable)
	case res.ExitCode == 255:
		// OpenSSH reserves 255 for client/connection failure; a remote body
		// exiting 255 is indistinguishable, and the contract resolves that
		// ambiguity toward infra: a verdict must be positively earned.
		res.Err = fmt.Errorf("worker %q: ssh client exited 255 (connection or client failure): %w", worker, ErrWorkerUnreachable)
	}
	if res.ExitCode == 0 && res.Err == nil {
		res.Status = StatusDone
	} else {
		res.Status = StatusFailed
	}
	return res
}

// Close marks the transport closed. Idempotent by contract: a transport shared
// by several workers is closed once per worker. There is no persistent
// connection to tear down (each Exec dials its own), so closing only refuses
// further Execs.
func (x *SSHTransport) Close() error {
	x.closed.Store(true)
	return nil
}

// commandArgs builds the client argv: the Command prefix, -l/-p when the host
// carries a user/port, the dial target, then the remote shell argv (omitted
// for "none"). shellMode reports whether a remote shell will interpret the
// body (and so whether a shell preamble may be prepended).
func (x *SSHTransport) commandArgs() (name string, args []string, shellMode bool) {
	parts := strings.Fields(x.Command)
	if len(parts) == 0 {
		parts = []string{"ssh"}
	}
	args = append(args, parts[1:]...)
	if x.user != "" {
		args = append(args, "-l", x.user)
	}
	if x.port > 0 {
		args = append(args, "-p", strconv.Itoa(x.port))
	}
	args = append(args, x.target)
	shell := strings.TrimSpace(x.Shell)
	if shell == "" {
		shell = "bash -s"
	}
	if strings.EqualFold(shell, "none") {
		return parts[0], args, false
	}
	return parts[0], append(args, strings.Fields(shell)...), true
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
