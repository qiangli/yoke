package sdlc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// The durable trigger daemon — the always-on SDLC loop, packaged so the outpost
// bashy-service supervisor can keep it alive across reboots (like loom). The
// supervisor drives `bashy sdlc service {start,status,stop}`; `run` is the
// foreground loop `start` launches. status prints "running"/"stopped" — the exact
// contract outpost's bashyServiceRunning parses.

// ServiceOptions configures the loop.
type ServiceOptions struct {
	ConfigPath string
	RunsDir    string
	Interval   time.Duration
	Cwd        string
	Sandbox    string
	Timeout    time.Duration
}

// ServiceStatus is the loop's lifecycle state.
type ServiceStatus struct {
	SchemaVersion string `json:"schema_version"`
	Running       bool   `json:"running"`
	PID           int    `json:"pid,omitempty"`
	PidFile       string `json:"pid_file"`
}

func servicePidPath(runsDir string) string {
	rd := runsDirForOption(runsDir) // e.g. .bashy/generated/sdlc/runs
	return filepath.Join(filepath.Dir(rd), "service.pid")
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

// ServiceStatusOf reports whether the loop is running (pidfile + live process).
func ServiceStatusOf(opt ServiceOptions) ServiceStatus {
	pidPath := servicePidPath(opt.RunsDir)
	st := ServiceStatus{SchemaVersion: schemaVersion, PidFile: pidPath}
	pid, err := readPid(pidPath)
	if err != nil || pid <= 0 {
		return st
	}
	if processAlive(pid) {
		st.Running, st.PID = true, pid
	}
	return st
}

// StartService launches the loop in the background (idempotent: a no-op when it
// is already running). It re-execs `bashy sdlc service run` detached.
func StartService(opt ServiceOptions) (ServiceStatus, error) {
	if st := ServiceStatusOf(opt); st.Running {
		return st, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return ServiceStatus{}, err
	}
	args := []string{"sdlc", "service", "run", "--runs-dir", runsDirForOption(opt.RunsDir)}
	if opt.ConfigPath != "" {
		args = append(args, "--config", opt.ConfigPath)
	}
	if opt.Interval > 0 {
		args = append(args, "--interval", opt.Interval.String())
	}
	if opt.Cwd != "" {
		args = append(args, "--cwd", opt.Cwd)
	}
	if opt.Sandbox != "" {
		args = append(args, "--sandbox", opt.Sandbox)
	}
	cmd := exec.Command(exe, args...)
	applyBackgroundProcAttrs(cmd)
	if err := cmd.Start(); err != nil {
		return ServiceStatus{}, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	pidPath := servicePidPath(opt.RunsDir)
	if err := writePid(pidPath, pid); err != nil {
		return ServiceStatus{}, err
	}
	return ServiceStatus{SchemaVersion: schemaVersion, Running: true, PID: pid, PidFile: pidPath}, nil
}

// StopService signals the loop to stop and clears the pidfile.
func StopService(opt ServiceOptions) (ServiceStatus, error) {
	pidPath := servicePidPath(opt.RunsDir)
	if pid, err := readPid(pidPath); err == nil && pid > 0 && processAlive(pid) {
		_ = signalStop(pid)
	}
	_ = os.Remove(pidPath)
	return ServiceStatus{SchemaVersion: schemaVersion, Running: false, PidFile: pidPath}, nil
}

// TickFunc runs one loop iteration; injectable for tests.
type TickFunc func(ctx context.Context) (DelegateResult, error)

// Serve runs the loop in the foreground until the context is cancelled or a
// SIGTERM/SIGINT arrives. Each interval it runs one tick (auto-selecting and
// delegating an intake issue); an idle tick (empty queue) is a normal no-op.
func Serve(ctx context.Context, opt ServiceOptions, tick TickFunc) error {
	interval := opt.Interval
	if interval <= 0 {
		interval = 60 * time.Second
	}
	if tick == nil {
		tick = func(ctx context.Context) (DelegateResult, error) {
			return Delegate(ctx, DelegateOptions{
				ConfigPath: opt.ConfigPath,
				RunsDir:    opt.RunsDir,
				Cwd:        opt.Cwd,
				Sandbox:    opt.Sandbox,
				Timeout:    opt.Timeout,
			})
		}
	}

	pidPath := servicePidPath(opt.RunsDir)
	_ = writePid(pidPath, os.Getpid())
	defer func() {
		if pid, _ := readPid(pidPath); pid == os.Getpid() {
			_ = os.Remove(pidPath)
		}
	}()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		res, err := tick(ctx)
		switch {
		case err != nil:
			slog.Warn("sdlc service: tick error", "err", err)
		case res.Status == "idle":
			// empty intake queue — nothing to do this tick
		case res.RunID != "":
			slog.Info("sdlc service: tick delegated", "run", res.RunID, "issue", res.Issue.ID)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func newServiceCmd() *cobra.Command {
	var opt ServiceOptions
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "service",
		Short: "manage the always-on SDLC loop (start|status|stop|run)",
		Long: strings.TrimSpace(`
Run the SDLC intake→conductor loop as a supervised background service. The outpost
bashy-service supervisor drives start|status|stop; register it with a BashyService
whose Command is ["sdlc","service"].`),
	}
	pf := cmd.PersistentFlags()
	pf.StringVar(&opt.RunsDir, "runs-dir", ".bashy/generated/sdlc/runs", "local SDLC runs directory")
	pf.StringVar(&opt.ConfigPath, "config", ".bashy/sdlc.yaml", "SDLC config file")
	pf.DurationVar(&opt.Interval, "interval", 60*time.Second, "loop tick interval")
	pf.StringVar(&opt.Cwd, "cwd", "", "working directory for the conductor")
	pf.StringVar(&opt.Sandbox, "sandbox", "danger-full-access", "conductor sandbox")
	pf.BoolVar(&asJSON, "json", false, "print JSON")

	cmd.AddCommand(
		&cobra.Command{Use: "start", Short: "start the loop in the background", RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := StartService(opt)
			if err != nil {
				return err
			}
			printServiceStatus(cmd.OutOrStdout(), st, asJSON, "started")
			return nil
		}},
		&cobra.Command{Use: "status", Short: "report loop status", RunE: func(cmd *cobra.Command, _ []string) error {
			printServiceStatus(cmd.OutOrStdout(), ServiceStatusOf(opt), asJSON, "")
			return nil
		}},
		&cobra.Command{Use: "stop", Short: "stop the loop", RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := StopService(opt)
			if err != nil {
				return err
			}
			printServiceStatus(cmd.OutOrStdout(), st, asJSON, "stopped")
			return nil
		}},
		&cobra.Command{Use: "run", Short: "run the loop in the foreground (used by start)", Hidden: true, RunE: func(cmd *cobra.Command, _ []string) error {
			return Serve(cmd.Context(), opt, nil)
		}},
	)
	return cmd
}

// printServiceStatus prints a line containing "running" or "stopped" — the token
// outpost's bashyServiceRunning greps for.
func printServiceStatus(w io.Writer, st ServiceStatus, asJSON bool, action string) {
	if asJSON {
		b, _ := json.Marshal(st)
		fmt.Fprintln(w, string(b))
		return
	}
	state := "stopped"
	if st.Running {
		state = "running"
	}
	if action != "" {
		fmt.Fprintf(w, "sdlc service %s: %s (pid=%d)\n", action, state, st.PID)
	} else {
		fmt.Fprintf(w, "%s (pid=%d)\n", state, st.PID)
	}
}
