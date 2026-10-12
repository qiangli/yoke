package feed

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/toolcmd"
)

const (
	channelsFile      = "channels.json"
	channelsStateFile = "channels-state.json"
)

// Channel is one delivery channel: a registered tool command (TOOL:NAME,
// for example an OpenClaw send) that receives a batched digest of unread
// posts. The cadence preferences live here, locally; the server holds none.
type Channel struct {
	Name      string   `json:"name"`
	Cmd       string   `json:"cmd"`
	Args      string   `json:"args,omitempty"`
	Interval  string   `json:"interval,omitempty"` // Go duration; empty = the default
	MaxItems  int      `json:"max_items,omitempty"`
	Quiet     string   `json:"quiet,omitempty"` // "HH:MM-HH:MM" or "off"; empty = the default
	Immediate []string `json:"immediate,omitempty"`
	Off       bool     `json:"off,omitempty"`
}

// quietOff is a window with equal, nonzero bounds: PlanDelivery treats it as
// "never quiet" (an all-zero window means the default instead).
const quietOff = 60

// Prefs resolves the channel's stored preferences against the defaults.
func (c Channel) Prefs() (DeliveryPrefs, error) {
	p := DefaultDeliveryPrefs()
	if c.Interval != "" {
		d, err := time.ParseDuration(c.Interval)
		if err != nil || d <= 0 {
			return p, fmt.Errorf("feed: channel %q: bad interval %q", c.Name, c.Interval)
		}
		p.Interval = d
	}
	if c.MaxItems < 0 {
		return p, fmt.Errorf("feed: channel %q: max must not be negative", c.Name)
	}
	if c.MaxItems > 0 {
		p.MaxItems = c.MaxItems
	}
	if c.Quiet != "" {
		s, e, err := parseQuiet(c.Quiet)
		if err != nil {
			return p, fmt.Errorf("feed: channel %q: %w", c.Name, err)
		}
		p.QuietStart, p.QuietEnd = s, e
	}
	p.Immediate = c.Immediate
	p.Off = c.Off
	return p, nil
}

// parseQuiet reads "HH:MM-HH:MM" (the window may wrap midnight) or "off"
// into minutes after local midnight.
func parseQuiet(s string) (start, end int, err error) {
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, "off") {
		return quietOff, quietOff, nil
	}
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("bad quiet window %q: want HH:MM-HH:MM or off", s)
	}
	if start, err = parseClock(a); err != nil {
		return 0, 0, fmt.Errorf("bad quiet window %q: %w", s, err)
	}
	if end, err = parseClock(b); err != nil {
		return 0, 0, fmt.Errorf("bad quiet window %q: %w", s, err)
	}
	if start == end {
		return 0, 0, fmt.Errorf("bad quiet window %q: start equals end; use \"off\" for no quiet hours", s)
	}
	return start, end, nil
}

func parseClock(s string) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 || len(m) != 2 {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	return hh*60 + mm, nil
}

func channelsPath(dir string) string      { return filepath.Join(dir, channelsFile) }
func channelsStatePath(dir string) string { return filepath.Join(dir, channelsStateFile) }

func validateChannel(c Channel) error {
	if c.Name == "" {
		return errors.New("feed: channel name is required")
	}
	if strings.ContainsAny(c.Name, `/\`) || c.Name == "." || c.Name == ".." {
		return fmt.Errorf("feed: invalid channel name %q", c.Name)
	}
	if tool, name, ok := strings.Cut(c.Cmd, ":"); !ok || tool == "" || name == "" {
		return fmt.Errorf("feed: channel %q: --cmd wants TOOL:NAME, got %q", c.Name, c.Cmd)
	}
	if _, err := toolcmd.SplitArgs(c.Args); err != nil {
		return fmt.Errorf("feed: channel %q: --args: %w", c.Name, err)
	}
	_, err := c.Prefs()
	return err
}

func validateChannels(cs []Channel) error {
	seen := make(map[string]struct{}, len(cs))
	for _, c := range cs {
		if err := validateChannel(c); err != nil {
			return err
		}
		if _, dup := seen[c.Name]; dup {
			return fmt.Errorf("feed: duplicate channel name %q", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	return nil
}

// LoadChannels reads dir/channels.json. A missing file means none.
func LoadChannels(dir string) ([]Channel, error) {
	data, err := os.ReadFile(channelsPath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("feed: load channels: %w", err)
	}
	var cs []Channel
	if err := json.Unmarshal(data, &cs); err != nil {
		return nil, fmt.Errorf("feed: load channels: %w", err)
	}
	if err := validateChannels(cs); err != nil {
		return nil, err
	}
	return cs, nil
}

// SaveChannels validates cs and writes dir/channels.json atomically, 0600.
func SaveChannels(dir string, cs []Channel) error {
	if err := validateChannels(cs); err != nil {
		return err
	}
	if cs == nil {
		cs = []Channel{}
	}
	return writeJSON0600(dir, channelsPath(dir), cs)
}

// writeJSON0600 writes v to path atomically (temp file + rename) with 0600
// permissions, creating dir 0700 when missing.
func writeJSON0600(dir, path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("feed: write %s: %w", filepath.Base(path), err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("feed: write %s: %w", filepath.Base(path), err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("feed: write %s: %w", filepath.Base(path), err)
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("feed: write %s: %w", filepath.Base(path), err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("feed: write %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("feed: write %s: %w", filepath.Base(path), err)
	}
	return nil
}

// ---- channel ----

func newChannelCmd(env Env) *cobra.Command {
	ch := &cobra.Command{
		Use:   "channel",
		Short: "manage delivery channels (channels.json)",
		Long: `A channel is a registered tool command (bashy tool cmd) that "feed deliver"
sends a batched digest of unread posts to, on a cadence kept locally: batch
interval, item cap, quiet hours and authors that break through them.`,
	}
	var (
		cmdRef, args, interval, quiet string
		max                           int
		immediate                     []string
		off                           bool
	)
	add := &cobra.Command{
		Use:   "add NAME --cmd TOOL:NAME [--args '<extra args>']",
		Short: "add a delivery channel over a registered tool command",
		Example: `  bashy feed channel add phone --cmd openclaw:send --args '--channel telegram --target me' \
      --interval 4h --max 10 --quiet 22:00-07:00 --immediate alice`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, a []string) error {
			cfg := Channel{
				Name: a[0], Cmd: cmdRef, Args: args, Interval: interval,
				MaxItems: max, Quiet: quiet, Immediate: immediate, Off: off,
			}
			if err := validateChannel(cfg); err != nil {
				return err
			}
			if _, _, err := toolcmd.LookupCommand(cfg.Cmd); err != nil {
				return fmt.Errorf("feed: channel %q: %w", cfg.Name, err)
			}
			dir := env.dir()
			cs, err := LoadChannels(dir)
			if err != nil {
				return err
			}
			for _, e := range cs {
				if e.Name == cfg.Name {
					return fmt.Errorf("feed: channel %q already exists; remove it first", cfg.Name)
				}
			}
			if err := SaveChannels(dir, append(cs, cfg)); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "added channel %s (%s)\n", cfg.Name, cfg.Cmd)
			return nil
		},
	}
	f := add.Flags()
	f.StringVar(&cmdRef, "cmd", "", "registered tool command TOOL:NAME that sends the message (required)")
	f.StringVar(&args, "args", "", "extra arguments placed before --message")
	f.StringVar(&interval, "interval", "", "minimum time between batches (default 4h)")
	f.IntVar(&max, "max", 0, "items per batch before the rest collapse into a link (default 10)")
	f.StringVar(&quiet, "quiet", "", `quiet hours "HH:MM-HH:MM" local time, or "off" (default 22:00-07:00)`)
	f.StringArrayVar(&immediate, "immediate", nil, "author whose posts break quiet hours and the interval (repeatable)")
	f.BoolVar(&off, "off", false, "keep the channel but deliver nothing")
	_ = add.MarkFlagRequired("cmd")

	list := &cobra.Command{
		Use:   "list",
		Short: "list delivery channels",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cs, err := LoadChannels(env.dir())
			if err != nil {
				return err
			}
			for _, e := range cs {
				p, err := e.Prefs()
				if err != nil {
					return err
				}
				line := fmt.Sprintf("%s\t%s\tinterval=%s\tmax=%d\tquiet=%s", e.Name, e.Cmd, p.Interval, p.MaxItems, quietString(p))
				if len(e.Immediate) > 0 {
					line += "\timmediate=" + strings.Join(e.Immediate, ",")
				}
				if e.Args != "" {
					line += "\targs=" + e.Args
				}
				if e.Off {
					line += "\toff"
				}
				fmt.Fprintln(c.OutOrStdout(), oneLine(line))
			}
			return nil
		},
	}

	rm := &cobra.Command{
		Use:   "rm NAME",
		Short: "remove a delivery channel and its delivery state",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, a []string) error {
			dir := env.dir()
			cs, err := LoadChannels(dir)
			if err != nil {
				return err
			}
			var keep []Channel
			for _, e := range cs {
				if e.Name != a[0] {
					keep = append(keep, e)
				}
			}
			if len(keep) == len(cs) {
				return fmt.Errorf("feed: no channel named %q", a[0])
			}
			if err := SaveChannels(dir, keep); err != nil {
				return err
			}
			st, err := loadChannelState(dir)
			if err != nil {
				return err
			}
			if _, ok := st[a[0]]; ok {
				delete(st, a[0])
				if err := saveChannelState(dir, st); err != nil {
					return err
				}
			}
			fmt.Fprintf(c.OutOrStdout(), "removed channel %s\n", a[0])
			return nil
		},
	}
	ch.AddCommand(add, list, rm)
	return ch
}

func quietString(p DeliveryPrefs) string {
	if p.QuietStart == p.QuietEnd {
		return "off"
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d", p.QuietStart/60, p.QuietStart%60, p.QuietEnd/60, p.QuietEnd%60)
}
