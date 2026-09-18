package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/room"
)

// defaultPoll is how often follow mode re-reads the timeline. The log is a local
// append-only file, so polling is both cheap and reliable — a watcher that was
// not running when an event landed still sees it on its next read, which a
// push-only subscription could not offer.
const defaultPoll = time.Second

const defaultWaitInterval = 100 * time.Millisecond

var (
	watchTimeline     = room.Timeline
	watchTimelineStat = statTimeline
)

func newWatchCmd() *cobra.Command {
	var topic, to, roomID, as string
	var drain, jsonOut, all bool
	var interval, wait time.Duration
	var since int64

	cmd := &cobra.Command{
		Use: "watch [flags]",
		// NOT aliased to "subscribe"/"sub". Cobra resolves an alias ahead of a
		// sibling's real name, so those aliases silently shadowed the `subscribe`
		// command — typing `bus subscribe` ran `watch` and rejected its flags.
		// The two are also genuinely different operations: watch reads the stream
		// yourself, subscribe registers a standing interest for the sidecar to hold
		// on your behalf. Sharing a name would blur exactly the distinction the
		// sidecar exists to make.
		Aliases: []string{"follow", "tail"},
		Short:   "watch the bus for notifications (follow, or drain what you missed)",
		Long: `watch reads notifications off the host room's timeline.

Two modes, and the difference matters:

  --drain    print everything published since YOU last drained, then exit.
             This is the mode an orchestrator wants: it is not "what is on the
             bus", it is "what have I not seen", so nothing is missed just
             because nobody was watching at the time.

  (default)  follow: print matching notifications as they arrive, until
             interrupted.

Filters narrow what counts as yours: --topic, --to, --room. With no filter,
watch shows every notification (--all makes that explicit, and is required in
drain mode so a cursor is never advanced over messages you did not mean to
claim).

Drain position is per-reader identity, named by --as (default: your principal),
so two agents draining the same topic each get their own copy.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			filter := eventFilter{topic: topic, to: to, room: roomID}

			if !filter.any() && !all {
				return fmt.Errorf("watch: no filter given — pass --topic/--to/--room, or --all to watch everything")
			}
			if wait > 0 && all {
				return fmt.Errorf("watch: --wait cannot be combined with --all")
			}

			if drain {
				return runDrain(cmd, filter, drainOptions{
					as: resolveSubscriber(as), since: since, jsonOut: jsonOut, wait: wait,
				})
			}
			if wait > 0 {
				return fmt.Errorf("watch: --wait requires --drain")
			}
			return runFollow(cmd, filter, followOptions{
				since: since, jsonOut: jsonOut, poll: interval,
			})
		},
	}

	f := cmd.Flags()
	f.StringVar(&topic, "topic", "", "only notifications on this topic")
	f.StringVar(&to, "to", "", "only notifications addressed to this session or role")
	f.StringVar(&roomID, "room", "", "only notifications in this room")
	f.BoolVar(&all, "all", false, "watch every notification (required when no filter is given)")
	f.BoolVar(&drain, "drain", false, "print what you have not seen since your last drain, then exit")
	f.StringVar(&as, "as", "", "reader identity for the drain cursor (default: your principal)")
	f.Int64Var(&since, "since", 0, "start after this sequence number (overrides the saved cursor)")
	f.Int64Var(&since, "from", 0, "alias for --since; start after this sequence number")
	f.DurationVar(&interval, "interval", defaultPoll, "how often follow mode re-reads the timeline")
	f.DurationVar(&wait, "wait", 0, "with --drain, wait up to this duration for a new relevant notification")
	f.BoolVar(&jsonOut, "json", false, "emit one "+SchemaVersion+" JSON object per line (NDJSON)")
	return cmd
}

// eventFilter decides which notifications belong to this subscriber.
type eventFilter struct{ topic, to, room string }

func (f eventFilter) any() bool { return f.topic != "" || f.to != "" || f.room != "" }

// match reports whether an event is one this subscriber asked for.
//
// Only notify events are ever matched. The timeline also carries join/leave/
// steer/status entries — session bookkeeping that is not addressed to anyone and
// would be noise on a bus subscription.
func (f eventFilter) match(e room.Event) bool {
	if e.Type != room.EventNotify {
		return false
	}
	if f.topic != "" && e.Topic != f.topic {
		return false
	}
	if f.to != "" && e.To != f.to {
		return false
	}
	if f.room != "" && e.Room != f.room {
		return false
	}
	return true
}

type drainOptions struct {
	as      string
	since   int64
	jsonOut bool
	wait    time.Duration
}

// runDrain prints what this subscriber has not seen, then advances its cursor.
//
// The cursor is advanced only AFTER the events have been written out. A drain
// that updated the cursor first and then failed to print would silently consume
// the messages it was supposed to deliver — the reader would never learn they
// existed, and there is no way to ask for them again.
func runDrain(cmd *cobra.Command, f eventFilter, opt drainOptions) error {
	from := opt.since
	if from == 0 {
		c, err := readCursor(opt.as)
		if err != nil {
			return err
		}
		from = c
	}

	if opt.wait < 0 {
		return fmt.Errorf("watch: --wait must not be negative")
	}
	if opt.wait > 0 {
		if err := waitForDrain(cmd.Context(), f, from, opt.wait); err != nil {
			return err
		}
	}

	return drainOnce(cmd, f, opt, from)
}

func drainOnce(cmd *cobra.Command, f eventFilter, opt drainOptions, from int64) error {
	events, err := watchTimeline(0)
	if err != nil {
		return err
	}

	var matched []room.Event
	var high int64
	for _, e := range events {
		if e.Seq > high {
			high = e.Seq
		}
		if e.Seq > from && f.match(e) {
			matched = append(matched, e)
		}
	}

	for _, e := range matched {
		if err := writeEvent(cmd, e, opt.jsonOut); err != nil {
			return err
		}
	}

	// Advance past everything READ, not merely everything matched: a cursor that
	// only moved over matching events would re-scan the whole log every drain, and
	// would rewind the moment a subscriber narrowed its filter.
	if high > from {
		if err := writeCursor(opt.as, high); err != nil {
			return err
		}
	}
	if len(matched) == 0 && !opt.jsonOut {
		fmt.Fprintln(cmd.ErrOrStderr(), "nothing new")
	}
	return nil
}

// waitForDrain blocks until this drain has something relevant to read or the
// bound expires. A timeout is an empty successful read: per-turn pollers must
// not treat a quiet bus as a failed turn boundary.
func waitForDrain(ctx context.Context, f eventFilter, from int64, bound time.Duration) error {
	if bound <= 0 {
		return nil
	}
	timer := time.NewTimer(bound)
	defer timer.Stop()
	ticker := time.NewTicker(defaultWaitInterval)
	defer ticker.Stop()
	var last timelineStat
	var haveLast bool
	for {
		st, changed, err := timelineChanged(last, haveLast)
		if err != nil {
			return err
		}
		if changed {
			events, err := watchTimeline(0)
			if err != nil {
				return err
			}
			for _, e := range events {
				if e.Seq > from && f.match(e) {
					return nil
				}
			}
			last = st
			haveLast = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case <-ticker.C:
		}
	}
}

type timelineStat struct {
	size  int64
	mtime time.Time
}

func timelineChanged(last timelineStat, haveLast bool) (timelineStat, bool, error) {
	st, err := watchTimelineStat()
	if err != nil {
		return st, false, err
	}
	return st, !haveLast || st.size != last.size || !st.mtime.Equal(last.mtime), nil
}

func statTimeline() (timelineStat, error) {
	path := filepath.Join(room.Dir(), "timeline.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return timelineStat{}, nil
		}
		return timelineStat{}, err
	}
	return timelineStat{size: info.Size(), mtime: info.ModTime()}, nil
}

type followOptions struct {
	since   int64
	jsonOut bool
	poll    time.Duration
}

// runFollow streams matching notifications until interrupted.
//
// It starts from the CURRENT end of the log rather than replaying history,
// because "follow" means "tell me what happens from now on" — a subscriber that
// wanted the backlog would have asked to drain it. --since overrides that.
func runFollow(cmd *cobra.Command, f eventFilter, opt followOptions) error {
	last := opt.since
	if last == 0 {
		events, err := watchTimeline(0)
		if err != nil {
			return err
		}
		for _, e := range events {
			if e.Seq > last {
				last = e.Seq
			}
		}
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()

	if opt.poll <= 0 {
		opt.poll = defaultPoll
	}
	t := time.NewTicker(opt.poll)
	defer t.Stop()

	for {
		events, err := watchTimeline(0)
		if err != nil {
			return err
		}
		for _, e := range events {
			if e.Seq <= last {
				continue
			}
			last = e.Seq
			if f.match(e) {
				if werr := writeEvent(cmd, e, opt.jsonOut); werr != nil {
					return werr
				}
			}
		}
		select {
		case <-ctx.Done():
			// Interrupted is the NORMAL way to end a follow, not a failure.
			return nil
		case <-t.C:
		}
	}
}

// WatchEvent is one notification as `watch --json` emits it (NDJSON, one per
// line — a stream, so a reader can act on each as it arrives rather than waiting
// for a closing bracket that follow mode would never write).
type WatchEvent struct {
	SchemaVersion string `json:"schema_version"`
	Seq           int64  `json:"seq"`
	TS            string `json:"ts"`
	Principal     string `json:"principal,omitempty"`
	Topic         string `json:"topic,omitempty"`
	To            string `json:"to,omitempty"`
	Room          string `json:"room,omitempty"`
	Body          string `json:"body,omitempty"`
}

func writeEvent(cmd *cobra.Command, e room.Event, jsonOut bool) error {
	w := cmd.OutOrStdout()
	if jsonOut {
		b, err := json.Marshal(WatchEvent{
			SchemaVersion: SchemaVersion,
			Seq:           e.Seq, TS: e.TS,
			Principal: e.Principal, Topic: e.Topic, To: e.To, Room: e.Room,
			Body: e.Body,
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(b))
		return err
	}

	line := fmt.Sprintf("%s  <%s>", e.TS, e.Principal)
	var addr []string
	if e.Topic != "" {
		addr = append(addr, "#"+e.Topic)
	}
	if e.To != "" {
		addr = append(addr, "→"+e.To)
	}
	if e.Room != "" {
		addr = append(addr, "@"+e.Room)
	}
	if len(addr) > 0 {
		line += "  " + strings.Join(addr, " ")
	}
	if e.Body != "" {
		line += "  " + e.Body
	}
	_, err := fmt.Fprintln(w, line)
	return err
}
