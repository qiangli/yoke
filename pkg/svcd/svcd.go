// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package svcd is the daemon lifecycle behind a bashy service's
// `start` / `status` / `stop` verbs.
//
// It exists because there were about to be four copies of it. pkg/meet,
// pkg/sdlc and pkg/schedule each carry a private implementation — same pidfile
// format, same status tokens, three sets of build-tagged process helpers — and
// pkg/meet's own comment names the condition for stopping: "so the three stay
// diff-able until somebody factors them into one package." A fourth copy for
// the web console is that moment.
//
// So this is the factoring TARGET, adopted by the console first. The other
// three still carry their own copies and should migrate onto this package; the
// semantics here are pkg/meet's, deliberately, because that copy is the one
// that had already learned the most (a port probe that only trusts an explicit
// refusal, an identity check before signalling, an escalating stop).
//
// # The contract this must satisfy
//
// outpost supervises a bashy service by running `bashy <argv...> start`, then
// polling `bashy <argv...> status` every 30 seconds and restarting anything
// whose output contains "stopped" or "not running". On shutdown it runs `stop`.
// Three properties follow, and each is load-bearing:
//
//   - start is IDEMPOTENT. A second start against a live daemon is a silent
//     no-op, never a second process racing for the port.
//   - stop must ACTUALLY FREE THE PORT, or the supervisor's next restart fails
//     forever. Hence the escalation.
//   - a stale pidfile is SUCCESS for stop. The state the caller asked for holds;
//     erroring would make every stop after a crash look broken and loop the
//     supervisor.
package svcd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DefaultGrace is the SIGTERM→SIGKILL window.
const DefaultGrace = 5 * time.Second

// State tokens. outpost greps status output for "stopped" and "not running",
// so these strings are WIRE FORMAT with a supervisor on the other end — do not
// reword them without changing the supervisor.
const (
	StateRunning      = "running"
	StateStopped      = "stopped"
	StateNotRunning   = "not_running"
	StateStaleRemoved = "stale_pidfile_removed"
	StateUnidentified = "unidentified_listener"
)

// Spec describes one supervised daemon. Everything that differs between
// services lives here; everything that does not lives in this package.
type Spec struct {
	// Name appears in errors and messages ("apps", "meet").
	Name string
	// Schema is the envelope version of `status --json`.
	Schema string
	// Dir is the directory holding service.pid and service.log. It is the
	// service's own store root, so relocating the store (a test, a sandboxed
	// run) relocates the daemon with it and cannot touch a developer's own.
	Dir func() (string, error)
	// Argv is what follows the bashy binary to run the server in the
	// foreground, e.g. ["apps", "serve"]. Port and bind flags are appended.
	Argv []string
	// Health is the `service` value this daemon's GET /healthz reports. It is
	// what distinguishes "our daemon holds the port" from "something else does"
	// — and therefore what separates a port we may signal from one we may not.
	Health string
	// DefaultPort is used when Options.Port is zero.
	DefaultPort int
}

// Options are the per-invocation knobs.
type Options struct {
	Port  int
	Bind  string
	Grace time.Duration
}

func (s Spec) port(o Options) int {
	if o.Port > 0 {
		return o.Port
	}
	return s.DefaultPort
}

func bind(o Options) string {
	if b := strings.TrimSpace(o.Bind); b != "" {
		return b
	}
	return "127.0.0.1"
}

func (s Spec) addr(o Options) string { return fmt.Sprintf("%s:%d", bind(o), s.port(o)) }

func grace(o Options) time.Duration {
	if o.Grace > 0 {
		return o.Grace
	}
	return DefaultGrace
}

// Status is the daemon's lifecycle state.
//
// Addr is the CONFIGURED address (this invocation's --bind/--port), not one
// read back from the running process: the pidfile holds a bare pid, on purpose,
// so anything expecting a conventional pidfile can read it. Under outpost both
// sides come from the same registration, so they agree.
type Status struct {
	SchemaVersion string `json:"schema_version"`
	Running       bool   `json:"running"`
	PID           int    `json:"pid,omitempty"`
	PidFile       string `json:"pid_file"`
	Addr          string `json:"addr,omitempty"`
	LogFile       string `json:"log_file,omitempty"`
	State         string `json:"state,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

// ErrStopped is what `status` returns when the daemon is not running, so the
// command's EXIT CODE carries the answer too. A supervisor should not have to
// parse prose to learn whether the thing it supervises is up.
var ErrStopped = errors.New("service: not running")

// ErrUnidentified means the configured port is occupied, but no visible pidfile
// safely identifies a process that stop is authorized to signal.
var ErrUnidentified = errors.New("service: listener could not be identified or stopped")

func (s Spec) pidPath() (string, error) { return s.path("service.pid") }
func (s Spec) logPath() (string, error) { return s.path("service.log") }

func (s Spec) path(name string) (string, error) {
	base, err := s.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, name), nil
}

func writePid(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644)
}

func readPid(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

type probe int

const (
	probeClear probe = iota
	probeMine
	probeOccupied
)

// probePort uses the port as the cross-store liveness identity. A TCP listener
// is evidence that stop cannot claim success; /healthz tells us whether that
// listener is ours without granting permission to kill an unknown process.
func (s Spec) probePort(o Options) probe {
	host := bind(o)
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::", "[::]":
		host = "::1"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(s.port(o)))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		// Only an explicit refusal proves the address was reached and had no
		// listener. A timeout, DNS failure, or unreachable address is absence of
		// evidence and therefore cannot authorize a success claim.
		if errors.Is(err, syscall.ECONNREFUSED) {
			return probeClear
		}
		return probeOccupied
	}
	_ = conn.Close()

	client := &http.Client{Timeout: 500 * time.Millisecond, Transport: &http.Transport{Proxy: nil}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		return probeOccupied
	}
	resp, err := client.Do(req)
	if err != nil {
		return probeOccupied
	}
	defer resp.Body.Close()
	var health struct {
		OK      bool   `json:"ok"`
		Service string `json:"service"`
	}
	if resp.StatusCode == http.StatusOK &&
		json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&health) == nil &&
		health.OK && health.Service == s.Health {
		return probeMine
	}
	return probeOccupied
}

// StatusOf reports whether the daemon is running. When the current store has no
// usable pidfile, the configured port supplies the missing evidence.
func (s Spec) StatusOf(o Options) (Status, error) {
	st := Status{SchemaVersion: s.Schema, Addr: s.addr(o)}
	p, err := s.pidPath()
	if err != nil {
		return st, err
	}
	st.PidFile = p
	if lp, err := s.logPath(); err == nil {
		st.LogFile = lp
	}
	pid, err := readPid(p)
	if err == nil && pid > 0 && processAlive(pid) {
		st.Running, st.PID, st.State = true, pid, StateRunning
		return st, nil
	}
	switch s.probePort(o) {
	case probeMine:
		st.Running = true
		st.State = StateRunning
		st.Detail = fmt.Sprintf("%s answered on port %d; pidfile is not visible", s.Health, s.port(o))
		return st, nil
	case probeOccupied:
		st.State = StateUnidentified
		st.Detail = fmt.Sprintf("port %d is in use by an unidentified listener", s.port(o))
		return st, ErrUnidentified
	default:
		st.State = StateNotRunning
		if err == nil || !os.IsNotExist(err) {
			st.Detail = "pidfile is stale"
		}
		return st, nil
	}
}

// Start launches the server detached in the background.
//
// Idempotent by contract: outpost re-runs start on every supervision tick, so a
// second start against a live pid must be a silent no-op rather than a second
// server racing for the port.
func (s Spec) Start(o Options) (Status, error) {
	if st, err := s.StatusOf(o); err != nil {
		return st, err
	} else if st.Running {
		return st, nil
	}
	p, err := s.pidPath()
	if err != nil {
		return Status{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return Status{}, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return Status{}, err
	}

	args := append([]string{}, s.Argv...)
	args = append(args, "--port", strconv.Itoa(s.port(o)), "--bind", bind(o))
	cmd := exec.Command(exe, args...)
	// A daemon has nowhere to write: its parent is about to exit and its stdout
	// is a terminal that will close. Without this the server's startup banner
	// and every later error vanish, which is precisely what you need when the
	// supervisor reports it keeps dying.
	logPath, err := s.logPath()
	if err == nil {
		if lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			defer lf.Close()
			cmd.Stdout, cmd.Stderr = lf, lf
		}
	}
	applyBackgroundProcAttrs(cmd)
	if err := cmd.Start(); err != nil {
		return Status{}, err
	}
	pid := cmd.Process.Pid
	// Release, never Wait: this process exits immediately and the daemon must
	// outlive it.
	_ = cmd.Process.Release()
	if err := writePid(p, pid); err != nil {
		return Status{}, err
	}
	return Status{
		SchemaVersion: s.Schema, Running: true, PID: pid,
		PidFile: p, Addr: s.addr(o), LogFile: logPath,
	}, nil
}

// Stop asks the daemon to stop, then insists.
//
// A STALE pidfile is success, not failure. The state the caller asked for — not
// running — is the state that holds; erroring on it would make every stop after
// a crash look like a broken stop, and a supervisor would loop on it forever.
func (s Spec) Stop(o Options) (Status, error) {
	st := Status{SchemaVersion: s.Schema, Addr: s.addr(o)}
	p, err := s.pidPath()
	if err != nil {
		return st, err
	}
	st.PidFile = p
	pid, readErr := readPid(p)
	hadPidfile := readErr == nil || !os.IsNotExist(readErr)
	livePID := readErr == nil && pid > 0 && processAlive(pid)
	if livePID {
		_ = signalStop(pid)
		if !waitGone(pid, grace(o)) {
			// A server wedged past the grace period is one the supervisor cannot
			// restart, because the port is still held. Take the port back.
			_ = forceStop(pid)
			_ = waitGone(pid, time.Second)
		}
	}
	pr := s.probePort(o)
	if pr != probeClear {
		st.Running = pr == probeMine
		st.PID = pid
		st.State = StateUnidentified
		switch {
		case livePID:
			st.Detail = fmt.Sprintf("pid=%d or another listener is still using port %d", pid, s.port(o))
		case pr == probeMine:
			st.Detail = fmt.Sprintf("%s is running on port %d, but no visible pidfile identifies its pid", s.Health, s.port(o))
		default:
			st.Detail = fmt.Sprintf("port %d is in use by an unidentified listener", s.port(o))
		}
		return st, ErrUnidentified
	}
	// Remove last: a pidfile dropped before the signal would strand the process
	// with nothing left pointing at it.
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return st, err
	}
	switch {
	case livePID:
		st.State = StateStopped
	case hadPidfile:
		st.State = StateStaleRemoved
		st.Detail = "stale pidfile removed"
	default:
		st.State = StateNotRunning
	}
	return st, nil
}

// waitGone polls until pid is dead or the deadline passes. Polling rather than
// waiting on the child: `stop` is usually run by a DIFFERENT process from the
// one that started the daemon, so there is no child to reap.
func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if !processAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}
