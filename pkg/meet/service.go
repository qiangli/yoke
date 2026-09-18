package meet

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

	"github.com/spf13/cobra"
)

// `meet serve` as a supervised daemon, so outpost can keep the web room alive.
//
// outpost supervises local services through one convention — `bashy <Command...>
// {start|status|stop}` (conf.BashyService) — and it polls status on a timer,
// restarting anything that reads "stopped". The obvious spelling for this side
// is `bashy meet service start`; `bashy meet open` convenes a
// deliberation session and enters the REPL, which is a foreground human verb and
// nothing a supervisor should ever call. Renaming it to free the word would
// break every documented workflow.
//
// `sdlc` hit the identical collision and answered it with a subcommand
// (`bashy sdlc service start`), and outpost issue #6 already declares this
// service as Command: ["meet","service"]. So the daemon lifecycle lives one
// level down so service lifecycle and room lifecycle remain distinct.
//
// There is no shared pidfile helper in the repo to reuse: pkg/sdlc and
// pkg/schedule each carry their own private copy of this (writePid/readPid +
// build-tagged processAlive/signalStop). This is the third, deliberately shaped
// like the other two — same pidfile format (a bare pid), same "running"/
// "stopped" status token outpost's bashyServiceRunning greps for — so the three
// stay diff-able until somebody factors them into one package.
//
// What this one adds over the other two: stop ESCALATES. SIGTERM asks the server
// to shut down (runMeetServe drains its WebSocket clients on ctx.Done), and a
// process that has not gone after the grace period gets SIGKILL. A supervisor
// that cannot guarantee the port is free cannot restart the service.

// serviceSchema is the envelope version of `meet service status --json`.
const serviceSchema = "bashy-meet-service-v1"

const (
	serviceStateRunning      = "running"
	serviceStateStopped      = "stopped"
	serviceStateNotRunning   = "not_running"
	serviceStateStaleRemoved = "stale_pidfile_removed"
	serviceStateUnidentified = "unidentified_listener"
)

// stopGrace is how long stop waits after SIGTERM before SIGKILL.
const stopGrace = 5 * time.Second

// ServiceOptions configures the daemon lifecycle verbs.
type ServiceOptions struct {
	// Port and Bind are handed to `meet serve` verbatim; they default to the
	// same values that command defaults to, so `meet service start` and
	// `meet serve` land on the same address.
	Port int
	Bind string
	// Grace is the SIGTERM→SIGKILL window. Zero means stopGrace.
	Grace time.Duration
}

// ServiceStatus is the daemon's lifecycle state.
//
// Addr is the CONFIGURED address (this invocation's --bind/--port), not one read
// back from the running process: the pidfile holds a bare pid, on purpose, so
// that anything expecting a conventional pidfile can read it. Under outpost both
// sides come from the same registration, so they agree.
type ServiceStatus struct {
	SchemaVersion string `json:"schema_version"`
	Running       bool   `json:"running"`
	PID           int    `json:"pid,omitempty"`
	PidFile       string `json:"pid_file"`
	Addr          string `json:"addr,omitempty"`
	LogFile       string `json:"log_file,omitempty"`
	State         string `json:"state,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

// ErrServiceStopped is what `status` returns when the daemon is not running, so
// the command's EXIT CODE carries the answer too. A supervisor should not have to
// parse prose to learn whether the thing it supervises is up.
var ErrServiceStopped = errors.New("meet service: not running")

// ErrServiceUnidentified means the configured port is occupied, but no visible
// pidfile safely identifies a process that stop is authorized to signal.
var ErrServiceUnidentified = errors.New("meet service: listener could not be identified or stopped")

func (o ServiceOptions) port() int {
	if o.Port > 0 {
		return o.Port
	}
	return defaultServePort
}

func (o ServiceOptions) bind() string {
	if b := strings.TrimSpace(o.Bind); b != "" {
		return b
	}
	return "127.0.0.1"
}

func (o ServiceOptions) addr() string {
	return fmt.Sprintf("%s:%d", o.bind(), o.port())
}

func (o ServiceOptions) grace() time.Duration {
	if o.Grace > 0 {
		return o.Grace
	}
	return stopGrace
}

// servicePidPath keeps the pidfile at the ROOT of the session store, beside the
// per-meeting directories rather than inside one: the server outlives any single
// room, and $BASHY_MEET_DIR already relocates the whole store (which is what lets
// a test drive this without touching a developer's own daemon).
func servicePidPath() (string, error) {
	base, err := baseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "service.pid"), nil
}

func serviceLogPath() (string, error) {
	base, err := baseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "service.log"), nil
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

type serviceProbe int

const (
	probeClear serviceProbe = iota
	probeMeet
	probeOccupied
)

// probeServicePort uses the port as the cross-store liveness identity. A TCP
// listener is evidence that stop cannot claim success; /healthz tells us whether
// that listener is bashy-meet without granting permission to kill an unknown
// process.
func probeServicePort(opt ServiceOptions) serviceProbe {
	host := opt.bind()
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::", "[::]":
		host = "::1"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(opt.port()))
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

	client := &http.Client{
		Timeout: 500 * time.Millisecond,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}
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
		health.OK && health.Service == otelServiceName {
		return probeMeet
	}
	return probeOccupied
}

// serviceStatusOf reports whether the daemon is running. When the current store
// has no usable pidfile, the configured port supplies the missing evidence.
func serviceStatusOf(opt ServiceOptions) (ServiceStatus, error) {
	st := ServiceStatus{SchemaVersion: serviceSchema, Addr: opt.addr()}
	p, err := servicePidPath()
	if err != nil {
		return st, err
	}
	st.PidFile = p
	if lp, err := serviceLogPath(); err == nil {
		st.LogFile = lp
	}
	pid, err := readPid(p)
	if err == nil && pid > 0 && processAlive(pid) {
		st.Running, st.PID, st.State = true, pid, serviceStateRunning
		return st, nil
	}
	switch probeServicePort(opt) {
	case probeMeet:
		st.Running = true
		st.State = serviceStateRunning
		st.Detail = fmt.Sprintf("bashy-meet answered on port %d; pidfile is not visible", opt.port())
		return st, nil
	case probeOccupied:
		st.State = serviceStateUnidentified
		st.Detail = fmt.Sprintf("port %d is in use by an unidentified listener", opt.port())
		return st, ErrServiceUnidentified
	default:
		st.State = serviceStateNotRunning
		if err == nil || !os.IsNotExist(err) {
			st.Detail = "pidfile is stale"
		}
		return st, nil
	}
}

// ServiceStatusOf is the compatibility form used by callers that only need the
// status envelope. Command paths use serviceStatusOf so an unidentified listener
// also affects the exit code.
func ServiceStatusOf(opt ServiceOptions) ServiceStatus {
	st, _ := serviceStatusOf(opt)
	return st
}

// StartService launches `meet serve` detached in the background.
//
// Idempotent by contract: outpost re-runs start on every supervision tick, so a
// second start against a live pid must be a silent no-op rather than a second
// server racing for the port.
func StartService(opt ServiceOptions) (ServiceStatus, error) {
	if st, err := serviceStatusOf(opt); err != nil {
		return st, err
	} else if st.Running {
		return st, nil
	}
	p, err := servicePidPath()
	if err != nil {
		return ServiceStatus{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return ServiceStatus{}, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return ServiceStatus{}, err
	}

	cmd := exec.Command(exe, "meet", "serve",
		"--port", strconv.Itoa(opt.port()), "--bind", opt.bind())
	// A daemon has nowhere to write: its parent is about to exit and its stdout
	// is a terminal that will close. Without this the server's startup banner and
	// every later error vanish, which is precisely what you need when the
	// supervisor reports it keeps dying.
	logPath, err := serviceLogPath()
	if err == nil {
		if lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			defer lf.Close()
			cmd.Stdout, cmd.Stderr = lf, lf
		}
	}
	applyBackgroundProcAttrs(cmd)
	if err := cmd.Start(); err != nil {
		return ServiceStatus{}, err
	}
	pid := cmd.Process.Pid
	// Release, never Wait: this process exits immediately and the daemon must
	// outlive it.
	_ = cmd.Process.Release()
	if err := writePid(p, pid); err != nil {
		return ServiceStatus{}, err
	}
	return ServiceStatus{
		SchemaVersion: serviceSchema, Running: true, PID: pid,
		PidFile: p, Addr: opt.addr(), LogFile: logPath,
	}, nil
}

// StopService asks the daemon to stop, then insists.
//
// A STALE pidfile is success, not failure. The state the caller asked for — not
// running — is the state that holds; erroring on it would make every stop after a
// crash look like a broken stop, and a supervisor would loop on it forever.
func StopService(opt ServiceOptions) (ServiceStatus, error) {
	st := ServiceStatus{SchemaVersion: serviceSchema, Addr: opt.addr()}
	p, err := servicePidPath()
	if err != nil {
		return st, err
	}
	st.PidFile = p
	pid, readErr := readPid(p)
	hadPidfile := readErr == nil || !os.IsNotExist(readErr)
	livePID := readErr == nil && pid > 0 && processAlive(pid)
	if livePID {
		_ = signalStop(pid)
		if !waitGone(pid, opt.grace()) {
			// The room's WebSocket loops are polling tails; a server wedged past
			// the grace period is one the supervisor cannot restart, because the
			// port is still held. Take the port back.
			_ = forceStop(pid)
			_ = waitGone(pid, time.Second)
		}
	}
	probe := probeServicePort(opt)
	if probe != probeClear {
		st.Running = probe == probeMeet
		st.PID = pid
		st.State = serviceStateUnidentified
		if livePID {
			st.Detail = fmt.Sprintf("pid=%d or another listener is still using port %d", pid, opt.port())
		} else if probe == probeMeet {
			st.Detail = fmt.Sprintf("bashy-meet is running on port %d, but no visible pidfile identifies its pid", opt.port())
		} else {
			st.Detail = fmt.Sprintf("port %d is in use by an unidentified listener", opt.port())
		}
		return st, ErrServiceUnidentified
	}
	// Remove last: a pidfile dropped before the signal would strand the process
	// with nothing left pointing at it.
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return st, err
	}
	switch {
	case livePID:
		st.State = serviceStateStopped
	case hadPidfile:
		st.State = serviceStateStaleRemoved
		st.Detail = "stale pidfile removed"
	default:
		st.State = serviceStateNotRunning
	}
	return st, nil
}

// waitGone polls until pid is dead or the deadline passes. Polling rather than
// waiting on the child: `stop` is usually run by a DIFFERENT process from the one
// that started the daemon, so there is no child to reap.
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

func newServiceCmd() *cobra.Command {
	var opt ServiceOptions
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "service",
		Short: "manage `meet serve` as a supervised background daemon (start|status|stop)",
		Long: strings.TrimSpace(`
Run the meet web server as a background daemon under a supervisor.

This is the lifecycle wrapper outpost drives: register it with a BashyService
whose Command is ["meet","service"] and it will call start / status / stop.
It is NOT ` + "`meet open`" + `, which convenes a deliberation session.`),
	}
	pf := cmd.PersistentFlags()
	pf.IntVar(&opt.Port, "port", defaultServePort, "listen port")
	pf.StringVar(&opt.Bind, "bind", "127.0.0.1", "bind address")
	pf.BoolVar(&asJSON, "json", false, "print JSON")

	cmd.AddCommand(
		&cobra.Command{
			Use:   "start",
			Short: "start the meet web server in the background (no-op if already running)",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				st, err := StartService(opt)
				if err != nil {
					return err
				}
				printServiceStatus(cmd.OutOrStdout(), st, asJSON, "start")
				return nil
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "report daemon status (exit 0 running, non-zero stopped)",
			Args:  cobra.NoArgs,
			// The non-zero exit for "stopped" is the answer, not a malfunction:
			// usage text and an "Error:" banner would be noise in a supervisor's
			// log every poll interval.
			SilenceUsage:  true,
			SilenceErrors: true,
			RunE: func(cmd *cobra.Command, _ []string) error {
				st, err := serviceStatusOf(opt)
				printServiceStatus(cmd.OutOrStdout(), st, asJSON, "")
				if err != nil {
					return err
				}
				if !st.Running {
					return ErrServiceStopped
				}
				return nil
			},
		},
		&cobra.Command{
			Use:           "stop",
			Short:         "stop the meet web server (SIGTERM, then SIGKILL)",
			Args:          cobra.NoArgs,
			SilenceUsage:  true,
			SilenceErrors: true,
			RunE: func(cmd *cobra.Command, _ []string) error {
				st, err := StopService(opt)
				printServiceStatus(cmd.OutOrStdout(), st, asJSON, "stop")
				if err != nil {
					return err
				}
				return nil
			},
		},
	)
	return cmd
}

// printServiceStatus distinguishes an observed stop from an already-idle
// service. In particular, absence of a pidfile never prints "stopped".
func printServiceStatus(w io.Writer, st ServiceStatus, asJSON bool, action string) {
	if asJSON {
		b, _ := json.Marshal(st)
		fmt.Fprintln(w, string(b))
		return
	}
	prefix := ""
	if action != "" {
		prefix = "meet service " + action + ": "
	}
	if st.State == serviceStateUnidentified {
		fmt.Fprintf(w, "%sFAILED: %s\n", prefix, st.Detail)
		return
	}
	if st.Running {
		if st.PID > 0 {
			fmt.Fprintf(w, "%srunning (pid=%d) http://%s/\n", prefix, st.PID, st.Addr)
		} else {
			fmt.Fprintf(w, "%srunning on port %s (pid unknown)\n", prefix, st.Addr)
		}
		return
	}
	switch st.State {
	case serviceStateStopped:
		fmt.Fprintf(w, "%sstopped\n", prefix)
	case serviceStateStaleRemoved:
		fmt.Fprintf(w, "%sstale pidfile removed\n", prefix)
	default:
		if st.Detail != "" {
			fmt.Fprintf(w, "%snot running (%s)\n", prefix, st.Detail)
		} else {
			fmt.Fprintf(w, "%snot running\n", prefix)
		}
	}
}
