package sdlc

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// ServiceOptions retains the supported configuration for the foreground SDLC unit.
type ServiceOptions struct {
	ConfigPath string
	RunsDir    string
	Interval   time.Duration
	Cwd        string
	Sandbox    string
	Timeout    time.Duration
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

func newServeCmd() *cobra.Command {
	var opt ServiceOptions
	cmd := &cobra.Command{Use: "serve", Short: "run the SDLC intake-to-conductor service in the foreground", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return Serve(cmd.Context(), opt, nil) }}
	f := cmd.Flags()
	f.StringVar(&opt.RunsDir, "runs-dir", ".bashy/generated/sdlc/runs", "local SDLC runs directory")
	f.StringVar(&opt.ConfigPath, "config", ".bashy/sdlc.yaml", "SDLC config file")
	f.DurationVar(&opt.Interval, "interval", 60*time.Second, "loop tick interval")
	f.StringVar(&opt.Cwd, "cwd", "", "working directory for the conductor")
	f.StringVar(&opt.Sandbox, "sandbox", "danger-full-access", "conductor sandbox")
	f.DurationVar(&opt.Timeout, "timeout", 0, "conductor timeout")
	return cmd
}
