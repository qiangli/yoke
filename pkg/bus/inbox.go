package bus

// `bashy inbox` is the receive-side front door for 1:1 bus notifications.
//
// It deliberately owns no store. Notifications remain room.EventNotify records
// in the bus timeline, and the read position remains the bus's per-subscriber
// drain cursor. This command only fixes the filter to the reader's own address
// and gives that common operation the small flag surface shared by the other
// communication commands.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/room"
)

// NewInboxCmd returns the top-level `inbox` command.
func NewInboxCmd() *cobra.Command {
	var as string
	var wait time.Duration
	var peek, jsonOut bool
	var id string
	var limit int

	cmd := &cobra.Command{
		Use:   "inbox",
		Short: "read 1:1 notifications waiting for you",
		Long: `inbox reads notifications addressed to you from the existing bus.

  bashy inbox
  bashy inbox --wait 15m
  bashy inbox --peek
  bashy inbox --limit 10 --json

Reading advances your existing per-subscriber bus cursor. --peek leaves it in
place, and --wait is bounded: a timeout is an empty successful read.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if wait < 0 {
				return fmt.Errorf("inbox: --wait must not be negative")
			}
			if limit < 0 {
				return fmt.Errorf("inbox: --limit must not be negative")
			}
			who, err := BoardIdentity(as)
			if err != nil {
				return err
			}
			if strings.TrimSpace(id) != "" {
				if wait != 0 || limit != 0 {
					return fmt.Errorf("inbox: --id cannot be combined with --wait or --limit")
				}
				return openInboxItem(cmd, who, id, jsonOut)
			}
			return runInbox(cmd, inboxOptions{
				reader: who, wait: wait, peek: peek, limit: limit, jsonOut: jsonOut,
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&as, "as", "", "read as this identity (default: resolved from your principal)")
	f.DurationVar(&wait, "wait", 0, "wait up to this duration for a new notification")
	f.BoolVar(&peek, "peek", false, "read without advancing your cursor")
	f.IntVarP(&limit, "limit", "n", 0, "print at most this many notifications (0 = no cap)")
	f.StringVar(&id, "id", "", "open one durable notification by bus:<sequence> without advancing a cursor")
	f.BoolVar(&jsonOut, "json", false, "emit one "+SchemaVersion+" JSON object per line (NDJSON)")
	return cmd
}

type inboxOptions struct {
	reader  string
	wait    time.Duration
	peek    bool
	limit   int
	jsonOut bool
}

// InboxSnapshot is one non-destructive read across the bus timeline and its
// materialized pending view. Items are deduplicated only when provenance proves
// they are the same timeline event (same sequence and fields), never because
// two senders happened to repeat text.
type InboxSnapshot struct {
	Items       []Pending
	reader      string
	through     int64
	pendingHigh int64
}

// Commit acknowledges both bus views after Items were delivered successfully.
func (s InboxSnapshot) Commit() error {
	if s.pendingHigh > 0 {
		if err := MarkRead(s.reader, s.pendingHigh); err != nil {
			return err
		}
	}
	if s.through > 0 {
		return MarkNotificationsRead(s.reader, s.through)
	}
	return nil
}

// SnapshotInbox resolves without a sidecar, then reconciles the durable
// timeline with the pending materialization. EnsureSubscription opens current
// topic/room routing; the direct timeline scan preserves addressed backlog that
// predates that subscription.
func SnapshotInbox(reader string) (InboxSnapshot, error) {
	reader = strings.TrimSpace(reader)
	snap := InboxSnapshot{reader: reader}
	if IsRoleName(reader) {
		if _, err := EnsureRoleInbox(reader); err != nil {
			return snap, err
		}
	} else {
		if _, err := EnsureSubscription(reader); err != nil {
			return snap, err
		}
	}
	// ONE timeline parse per snapshot, shared by the resolve and the direct
	// scan below. Each used to parse it independently.
	events, err := watchTimeline(0)
	if err != nil {
		return snap, err
	}
	if sub, err := LoadSubscription(reader); err == nil && sub.Subscriber != "" {
		if _, err := resolveForEvents(sub, events); err != nil {
			return snap, err
		}
	}
	allPending, err := ReadPending(reader)
	if err != nil {
		return snap, err
	}
	direct, through, err := unreadNotificationsFrom(reader, events)
	if err != nil {
		return snap, err
	}
	snap.through = through
	// Addressed backlog can predate an automatically-created subscription. Put
	// those records into the existing pending materialization so admission can
	// acknowledge an arbitrary priority-selected subset without inventing a new
	// store or advancing the timeline cursor over omitted records.
	for _, event := range direct {
		candidate := Pending{SchemaVersion: SchemaVersion, Seq: event.Seq, TS: event.TS, Principal: event.Principal, Topic: event.Topic, To: event.To, Room: event.Room, Body: event.Body, Delivery: DeliveryQueued}
		duplicate := false
		for _, p := range allPending {
			if sameNotification(p, candidate) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			if err := AppendPending(reader, candidate); err != nil {
				return snap, err
			}
			allPending = append(allPending, candidate)
		}
	}
	for _, p := range allPending {
		if !p.Unread() {
			continue
		}
		if p.Seq > snap.pendingHigh {
			snap.pendingHigh = p.Seq
		}
		snap.Items = append(snap.Items, p)
	}
	sort.SliceStable(snap.Items, func(i, j int) bool { return snap.Items[i].Seq < snap.Items[j].Seq })
	return snap, nil
}

// CommitItem acknowledges one represented record and advances the direct
// timeline cursor only after no earlier materialized record remains unread.
func (s InboxSnapshot) CommitItem(seq int64) error {
	if err := MarkPendingRead(s.reader, seq); err != nil {
		return err
	}
	items, err := UnreadPending(s.reader)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Seq <= s.through {
			return nil
		}
	}
	if s.through > 0 {
		return MarkNotificationsRead(s.reader, s.through)
	}
	return nil
}

func sameNotification(a, b Pending) bool {
	return a.Seq == b.Seq && a.TS == b.TS && a.Principal == b.Principal &&
		a.Topic == b.Topic && a.To == b.To && a.Room == b.Room && a.Body == b.Body
}

// UnreadNotifications returns the reader's addressed bus events and the exact
// timeline high-water mark represented by the scan. It is the read-through API
// used by Bashy's unified inbox; the bus remains the owner of both timeline and
// cursor and no second delivery store is introduced.
func UnreadNotifications(reader string) ([]room.Event, int64, error) {
	events, err := watchTimeline(0)
	if err != nil {
		return nil, 0, err
	}
	return unreadNotificationsFrom(reader, events)
}

// unreadNotificationsFrom is UnreadNotifications over an already-parsed
// timeline (see resolveForEvents for why the parse is shared).
func unreadNotificationsFrom(reader string, events []room.Event) ([]room.Event, int64, error) {
	from, err := readCursor(reader)
	if err != nil {
		return nil, 0, err
	}
	filter := eventFilter{to: reader}
	matched := make([]room.Event, 0)
	high := from
	for _, e := range events {
		if e.Seq <= from {
			continue
		}
		if e.Seq > high {
			high = e.Seq
		}
		if filter.match(e) {
			matched = append(matched, e)
		}
	}
	return matched, high, nil
}

// MarkNotificationsRead acknowledges a previously rendered bus snapshot.
func MarkNotificationsRead(reader string, through int64) error {
	return writeCursor(reader, through)
}

func openInboxItem(cmd *cobra.Command, reader, id string, jsonOut bool) error {
	raw := strings.TrimSpace(id)
	raw = strings.TrimPrefix(raw, "bus:")
	seq, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seq <= 0 {
		return fmt.Errorf("inbox: invalid --id %q (want bus:<sequence>)", id)
	}
	items, err := ReadPending(reader)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Seq != seq {
			continue
		}
		e := room.Event{Seq: item.Seq, TS: item.TS, Type: room.EventNotify, Principal: item.Principal, Topic: item.Topic, To: item.To, Room: item.Room, Body: item.Body, Priority: item.Delivery}
		return writeEvent(cmd, e, jsonOut)
	}
	return fmt.Errorf("inbox: notification bus:%d is not in %s's durable inbox", seq, reader)
}

func runInbox(cmd *cobra.Command, opt inboxOptions) error {
	from, err := readCursor(opt.reader)
	if err != nil {
		return err
	}
	filter := eventFilter{to: opt.reader}
	if opt.wait > 0 {
		// This is the bus watch bounded-drain primitive, including its stat-before-
		// parse loop and empty-success timeout contract. inbox adds no poller.
		if err := waitForDrain(cmd.Context(), filter, from, opt.wait); err != nil {
			return err
		}
	}

	events, err := watchTimeline(0)
	if err != nil {
		return err
	}
	matched := make([]room.Event, 0)
	high := from
	for _, e := range events {
		if e.Seq <= from {
			continue
		}
		if e.Seq > high {
			high = e.Seq
		}
		if filter.match(e) {
			matched = append(matched, e)
			if opt.limit > 0 && len(matched) == opt.limit {
				// Stop at the last item shown. Advancing beyond it would silently
				// consume a notification omitted only because of --limit.
				high = e.Seq
				break
			}
		}
	}

	for _, e := range matched {
		if err := writeEvent(cmd, e, opt.jsonOut); err != nil {
			return err
		}
	}
	if !opt.peek && high > from {
		if err := writeCursor(opt.reader, high); err != nil {
			return err
		}
	}
	if len(matched) == 0 && !opt.jsonOut {
		fmt.Fprintln(cmd.ErrOrStderr(), "nothing new")
	}
	return nil
}
