package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/fleet"
)

// WatchSampler is the single-sample source the watcher uses; overridable so
// tests (and the dry-run fixture) inject fake readings instead of touching
// real hardware. One short sample per interval, never a tree walk.
var WatchSampler = func(ctx context.Context) (*System, error) {
	return Collect(ctx, Options{Interval: 0})
}

// Notifier delivers one alarm event to a destination outside the console.
// Tests inject one; nothing here sends real mail in a test.
type Notifier func(ev Event) error

// MBNotifier delivers through the same board path `bashy mb send` uses.
func MBNotifier(recipient string) Notifier {
	return func(ev Event) error {
		from, err := bus.BoardIdentity("resource-watch")
		if err != nil {
			from = "resource-watch"
		}
		body := FormatEventLine(ev)
		if ev.Detail != "" {
			body += " (" + ev.Detail + ")"
		}
		_, err = bus.Send(bus.SendRequest{
			From: from, To: recipient, Topic: "resource-watch", Body: body,
		})
		return err
	}
}

// FormatEventLine renders the one-line-per-transition console form:
// resource, level, value, threshold, host, time.
func FormatEventLine(ev Event) string {
	unit := "%"
	switch ev.Unit {
	case "bytes":
		unit = "B"
	case "state":
		unit = ""
	}
	val := watchTrimFloat(ev.Value)
	th := watchTrimFloat(ev.Threshold)
	at := ev.At.Format(time.RFC3339)
	switch ev.Type {
	case "clear":
		return fmt.Sprintf("CLEAR host=%s resource=%s metric=%s value=%s%s at=%s",
			ev.Host, ev.Resource, ev.Metric, val, unit, at)
	case "renotify":
		return fmt.Sprintf("ALARM host=%s resource=%s metric=%s level=%s value=%s%s threshold=%s%s at=%s (still-alarming re-notify)",
			ev.Host, ev.Resource, ev.Metric, ev.Level, val, unit, th, unit, at)
	default:
		return fmt.Sprintf("ALARM host=%s resource=%s metric=%s level=%s value=%s%s threshold=%s%s at=%s",
			ev.Host, ev.Resource, ev.Metric, ev.Level, val, unit, th, unit, at)
	}
}

func watchTrimFloat(v float64) string {
	s := fmt.Sprintf("%.1f", v)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

// watchStatePath is where --once runs keep their memory across invocations.
func watchStatePath(dir, host string) string {
	if dir == "" {
		dir = ResourcesStateDir()
	}
	return dir + "/watch-" + WatchNamePrefix(host) + ".json"
}

func defaultWatchRecipient() string {
	if v := strings.TrimSpace(os.Getenv("BASHY_RESOURCE_WATCH_TO")); v != "" {
		return v
	}
	return "operator"
}

// NewWatchCommand builds `bashy resource watch`: sample every watched
// resource on this host, alarm on threshold transitions, optionally deliver
// to the inbox.
func NewWatchCommand() *cobra.Command {
	var (
		once       bool
		interval   time.Duration
		jsonOut    bool
		notify     bool
		recipient  string
		stateDir   string
		noRegister bool
	)
	c := &cobra.Command{
		Use:   "watch",
		Short: "Sample watched resources and alarm on threshold crossings",
		Long: "Sample this host's registered resources and alarm on threshold crossings.\n\n" +
			"On first run the host's cpu, memory (including swap), each mounted disk,\n" +
			"network interfaces and GPUs are registered automatically (unless already\n" +
			"present); thresholds come from the defaults, overridden per host and per\n" +
			"resource with `resource set NAME --watch k=v`. Alarms print one line per\n" +
			"transition on stderr (plus NDJSON on stdout with --json) and can also be\n" +
			"delivered to the inbox with --notify. State persists between --once runs,\n" +
			"so hysteresis, sustained windows and re-notify clocks survive the\n" +
			"scheduler cadence.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if interval <= 0 && !once {
				return fmt.Errorf("resource watch: --interval must be positive (or pass --once)")
			}
			rcpt := recipient
			if rcpt == "" {
				rcpt = defaultWatchRecipient()
			}
			return RunWatch(cmd.Context(), WatchOptions{
				Once:            once,
				Interval:        interval,
				JSON:            jsonOut,
				StateDir:        stateDir,
				Register:        !noRegister,
				NotifyTo:        rcpt,
				NotifyRequested: notify,
			}, cmd.ErrOrStderr(), cmd.OutOrStdout())
		},
	}
	c.Flags().BoolVar(&once, "once", false, "take one sample, emit any transitions, save state and exit")
	c.Flags().DurationVar(&interval, "interval", time.Minute, "loop interval in continuous mode")
	c.Flags().BoolVar(&jsonOut, "json", false, "emit events as NDJSON on stdout")
	c.Flags().BoolVar(&notify, "notify", false, "also deliver alarms to the bashy inbox")
	c.Flags().StringVar(&recipient, "recipient", "", "inbox recipient for --notify (default: the operator seat, or $BASHY_RESOURCE_WATCH_TO, or \"operator\")")
	c.Flags().StringVar(&stateDir, "state-dir", "", "directory for the watcher state file (default: the bashy resources state dir)")
	c.Flags().BoolVar(&noRegister, "no-register", false, "skip auto-registering host resources")
	return c
}

// WatchOptions is one invocation of the watcher: run mode, console shape
// and inbox delivery. Notify, when set, replaces the built-in delivery
// (tests inject it; nothing sends real mail in a test).
type WatchOptions struct {
	Once            bool
	Interval        time.Duration
	JSON            bool
	Register        bool
	StateDir        string
	NotifyTo        string
	NotifyRequested bool
	Notify          Notifier
}

const maxWatchStateBytes = 2 << 20

// RunWatch is the watcher's engine, shared by the cobra front door and the
// `resources watch` tool: resolve configs from the registry, sample, emit
// transition events, persist state, optionally notify.
func RunWatch(ctx context.Context, opts WatchOptions, errW, outW io.Writer) error {
	sys0, err := WatchSampler(ctx)
	if err != nil {
		return fmt.Errorf("resource watch: %v", err)
	}
	cat := fleet.New()
	if opts.Register {
		if added, err := AutoRegisterHost(cat, sys0); err != nil {
			fmt.Fprintf(errW, "resource watch: %v\n", err)
		} else if len(added) > 0 {
			fmt.Fprintf(errW, "resource watch: registered %s\n", strings.Join(added, " "))
		}
	}
	base, byName := ResolveWatchConfigs(cat, sys0.Host)
	notify := opts.Notify
	if notify == nil && (opts.NotifyRequested || base.NotifyTo != "") {
		rcpt := opts.NotifyTo
		if base.NotifyTo != "" {
			rcpt = base.NotifyTo
		}
		notify = MBNotifier(rcpt)
	}

	emit := func(evs []Event) {
		for _, ev := range evs {
			fmt.Fprintln(errW, FormatEventLine(ev))
			if opts.JSON {
				if b, err := json.Marshal(ev); err == nil {
					fmt.Fprintln(outW, string(b))
				}
			}
			if notify != nil {
				if err := notify(ev); err != nil {
					fmt.Fprintf(errW, "resource watch: notify: %v\n", err)
				}
			}
		}
	}

	path := watchStatePath(opts.StateDir, sys0.Host)
	w := NewWatcherWithConfigs(base, byName)
	var state WatchStateFile
	if err := readObservationJSON(path, maxWatchStateBytes, &state); err == nil {
		w.ImportState(state)
	}

	runOnce := func() error {
		sys, err := WatchSampler(ctx)
		if err != nil {
			return err
		}
		evs := w.Observe(time.Now(), sys)
		out := w.ExportState()
		if err := writeObservationJSON(path, maxWatchStateBytes, &out); err != nil {
			fmt.Fprintf(errW, "resource watch: state: %v\n", err)
		}
		emit(evs)
		return nil
	}

	if opts.Once {
		return runOnce()
	}
	tick := time.NewTicker(opts.Interval)
	defer tick.Stop()
	if err := runOnce(); err != nil {
		fmt.Fprintf(errW, "resource watch: %v\n", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := runOnce(); err != nil {
				fmt.Fprintf(errW, "resource watch: %v\n", err)
			}
		}
	}
}
