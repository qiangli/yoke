// Package mirror is a continuous one-way directory mirror — node B keeps a live
// replica of a directory on node A — built entirely from PERMISSIVE parts: it
// reuses Syncthing's *architecture* (a recursive filesystem watcher + a periodic
// full-scan backstop + delta transfer) without any Syncthing code. The watcher is
// the third-party rjeczalik/notify (MIT, native-recursive: FSEvents on macOS,
// ReadDirectoryChangesW on Windows, inotify-per-subdir on Linux); the transfer is
// a binmgr-managed rclone (MIT, `rclone sync` = delta + mirror semantics); the
// orchestration here (debounce, backstop, lifecycle) is our own.
//
// Topology over the mesh: the replica node runs `bashy rclone serve webdav
// <replica>` exposed as a mesh service; the source node dials it and runs
// `bashy mirror --source <dir> --dest <that-webdav-target>`. mirror is
// transport-agnostic — Dest is any rclone target (a local path, a connection
// string, a configured remote). See dhnt/docs/external-binary-builtins.md.
package mirror

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/rjeczalik/notify"
	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/external/rclone"
)

// Options configures Run.
type Options struct {
	// Source is the local directory to mirror.
	Source string
	// Dest is the rclone target the replica lives at (a local path, an rclone
	// connection string like ":webdav,url='http://127.0.0.1:8080':", or a
	// configured remote). `rclone sync Source Dest` makes Dest match Source.
	Dest string
	// Debounce coalesces a burst of filesystem events into one sync (default 2s).
	Debounce time.Duration
	// Interval is the periodic full-sync backstop that runs regardless of events
	// (catches anything the watcher missed; default 10m). 0 disables it.
	Interval time.Duration
	// ExtraArgs are passed through to `rclone sync` (e.g. --filter, --transfers).
	ExtraArgs []string
	// Logger (default slog.Default()).
	Logger *slog.Logger
	// Sync overrides the transfer for tests; nil = `rclone sync Source Dest`.
	Sync func(ctx context.Context) error
}

func (o *Options) defaults() {
	if o.Debounce <= 0 {
		o.Debounce = 2 * time.Second
	}
	if o.Interval == 0 {
		o.Interval = 10 * time.Minute
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Run starts the continuous mirror, blocking until ctx is cancelled. It does an
// initial full sync, then watches Source recursively and re-syncs on change
// (debounced), with a periodic full sync as the backstop.
func Run(ctx context.Context, o Options) error {
	o.defaults()
	if o.Source == "" || o.Dest == "" {
		return fmt.Errorf("mirror: source and dest are required")
	}
	if fi, err := os.Stat(o.Source); err != nil || !fi.IsDir() {
		return fmt.Errorf("mirror: source %q is not a directory", o.Source)
	}

	syncFn := o.Sync
	if syncFn == nil {
		bin, err := rclone.Path(ctx, "")
		if err != nil {
			return err
		}
		syncFn = func(ctx context.Context) error { return runRcloneSync(ctx, bin, o) }
	}

	// Recursive watch (the "/..." suffix is rjeczalik/notify's recursive form).
	events := make(chan notify.EventInfo, 256)
	if err := notify.Watch(o.Source+"/...", events, notify.All); err != nil {
		return fmt.Errorf("mirror: watch %s: %w", o.Source, err)
	}
	defer notify.Stop(events)

	do := func(reason string) {
		o.Logger.Info("mirror: sync", "reason", reason, "src", o.Source, "dst", o.Dest)
		if err := syncFn(ctx); err != nil && ctx.Err() == nil {
			o.Logger.Warn("mirror: sync failed", "err", err)
		}
	}
	do("initial")

	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	var backstopC <-chan time.Time
	if o.Interval > 0 {
		t := time.NewTicker(o.Interval)
		defer t.Stop()
		backstopC = t.C
	}
	pending := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-events:
			pending = true
			debounce.Reset(o.Debounce) // each event pushes the sync out — coalesces bursts
		case <-debounce.C:
			if pending {
				pending = false
				do("change")
			}
		case <-backstopC:
			do("periodic")
		}
	}
}

func runRcloneSync(ctx context.Context, bin string, o Options) error {
	args := append([]string{"sync", o.Source, o.Dest}, o.ExtraArgs...)
	c := exec.CommandContext(ctx, bin, args...)
	c.Stdout, c.Stderr = io.Discard, io.Discard
	return c.Run()
}

// NewMirrorCmd builds the `mirror` command (bashy front-door).
func NewMirrorCmd() *cobra.Command {
	var (
		o            Options
		debounceSecs int
		intervalSecs int
	)
	cmd := &cobra.Command{
		Use:   "mirror --source <dir> --dest <rclone-target>",
		Short: "Continuously mirror a directory to a destination (recursive watch + rclone sync)",
		Long: `mirror keeps a destination in sync with a source directory: an initial sync,
then a recursive filesystem watcher re-syncs on change (debounced), with a
periodic full sync as the backstop. Permissive parts only: rjeczalik/notify (MIT)
+ rclone (MIT). For node-to-node over the mesh, run 'bashy rclone serve webdav
<replica>' on the replica (expose it over the mesh), then point --dest at it.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.Debounce = time.Duration(debounceSecs) * time.Second
			o.Interval = time.Duration(intervalSecs) * time.Second
			return Run(cmd.Context(), o)
		},
	}
	cmd.Flags().StringVar(&o.Source, "source", "", "local directory to mirror (required)")
	cmd.Flags().StringVar(&o.Dest, "dest", "", "rclone target the replica lives at (required)")
	cmd.Flags().IntVar(&debounceSecs, "debounce", 2, "seconds to coalesce a burst of changes before syncing")
	cmd.Flags().IntVar(&intervalSecs, "interval", 600, "periodic full-sync backstop in seconds (0 disables)")
	_ = cmd.MarkFlagRequired("source")
	_ = cmd.MarkFlagRequired("dest")
	return cmd
}
