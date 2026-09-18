package bus

// The OBSERVER's view of every inbox on this host.
//
// `bashy inbox` is the first-person surface: it fixes the filter to the reader's
// own address and, unless --peek, advances that reader's cursor. This file is
// the third-person one — what a HUMAN sees when they want to inspect the mail of
// the whole fleet at once, which no first-person command can answer because
// there is no single reader whose inbox that is.
//
// TWO PROPERTIES ARE LOAD-BEARING, and both are the reason this is a separate
// file rather than a flag on the existing read path.
//
//  1. IT NEVER WRITES. Not a cursor, not a ReadAt stamp, not a subscription, not
//     a materialized Pending record. SnapshotInbox — the read path `bashy inbox`
//     uses — deliberately does all four: EnsureSubscription opens an inbox for a
//     name that had none, and addressed backlog predating that subscription is
//     APPENDED into the pending file so it can be acknowledged. That is correct
//     for the agent whose inbox it is and catastrophic for an observer, because
//     opening a web page would then mark another agent's mail as seen and eat it
//     at its next turn boundary. An inspection that mutates what it inspects is
//     not an inspection.
//
//  2. IT INTRODUCES NO STORE. Every fact here is derived from the two the bus
//     already owns — the append-only room timeline and the per-subscriber
//     pending materialization — plus the drain cursor. Nothing is persisted and
//     there is no second copy that could disagree with them. The Inboxes
//     snapshot below is an in-memory parse a caller may hold and revalidate by
//     stat; it is a read, not a store, and it can only ever be behind the log,
//     never in conflict with it.
//
// Read status is therefore REPORTED, never assumed: an item carries the
// materialized ReadAt when one exists, and otherwise says only that the
// subscriber's drain cursor has passed it. Those are different facts and a
// reader inspecting a stalled agent is entitled to which one they are looking
// at.

import (
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/room"
)

// Provenance of one inspected item.
const (
	// InboxSourcePending means a materialized Pending record exists — the
	// sidecar (or a previous read) put it in the subscriber's own buffer.
	InboxSourcePending = "pending"
	// InboxSourceTimeline means the record exists only as an addressed event on
	// the room timeline: published, durable, and not yet materialized for this
	// subscriber. It is what `bashy inbox` would fold in on its next read.
	InboxSourceTimeline = "timeline"
)

// How a name came to be listed. A name may carry several.
const (
	// InboxHolderCatalog: the fleet catalog names it (`bashy agent list`).
	InboxHolderCatalog = "catalog"
	// InboxHolderSubscribed: it holds a durable bus subscription.
	InboxHolderSubscribed = "subscription"
	// InboxHolderAddressed: the timeline carries mail addressed to it. This is
	// the rung that surfaces the ephemeral workers a catalog listing cannot —
	// an agent that was written to but never registered still has mail waiting,
	// and an inspection that hid it would hide exactly the backlog nobody owns.
	InboxHolderAddressed = "addressed"
	// InboxHolderViewer: the person doing the inspecting. Always listed, even
	// with an empty inbox, so the page has a definite first row.
	InboxHolderViewer = "viewer"
)

// Kinds of addressable name.
const (
	InboxKindPerson = "person"
	InboxKindAgent  = "agent"
	InboxKindRole   = "role"
	InboxKindOther  = "other"
)

// InboxItem is one notification as an observer sees it.
type InboxItem struct {
	Seq      int64  `json:"seq"`
	TS       string `json:"ts"`
	From     string `json:"from,omitempty"`
	Topic    string `json:"topic,omitempty"`
	To       string `json:"to,omitempty"`
	Room     string `json:"room,omitempty"`
	Body     string `json:"body,omitempty"`
	Delivery string `json:"delivery,omitempty"`
	// Demoted explains why an interrupt was downgraded. Carried through because
	// a governance decision nobody can observe is indistinguishable from a bug —
	// and this page is where somebody observes it.
	Demoted string `json:"demoted,omitempty"`
	// ReadAt is the materialized stamp, EMPTY when none was ever recorded. It is
	// not synthesized from the cursor: see Read.
	ReadAt string `json:"read_at,omitempty"`
	// Read is the honest union — a materialized stamp OR a drain cursor that has
	// passed this sequence. PastCursor says which, so "read at 09:12" and
	// "behind the cursor, never stamped" stay distinguishable.
	Read        bool   `json:"read"`
	PastCursor  bool   `json:"past_cursor"`
	Source      string `json:"source"`
	MatchReason string `json:"match_reason,omitempty"`
}

// InboxView is one name's whole inbox, oldest first.
type InboxView struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Cursor is this subscriber's drain position: the sequence through which its
	// own reads have consumed the timeline.
	Cursor int64       `json:"cursor"`
	Items  []InboxItem `json:"items"`
	Unread int         `json:"unread"`
}

// InboxHolder is one addressable name, summarized for a roster.
type InboxHolder struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Sources are the rungs that listed this name, sorted.
	Sources  []string `json:"sources"`
	Total    int      `json:"total"`
	Unread   int      `json:"unread"`
	LatestTS string   `json:"latest_ts,omitempty"`
	Cursor   int64    `json:"cursor"`
}

// InboxKind classifies a name without consulting any store.
//
// The order matters: a role topic (`steward.<ref>`) can also be a catalog name
// in principle, and the role reading is the one that changes how its backlog
// behaves (see IsRoleName), so it wins.
func InboxKind(name, viewer string) string {
	switch {
	case name == "":
		return InboxKindOther
	case viewer != "" && strings.EqualFold(name, viewer):
		return InboxKindPerson
	case IsRoleName(name):
		return InboxKindRole
	}
	if FleetNames != nil {
		for _, n := range FleetNames() {
			if strings.EqualFold(n, name) {
				return InboxKindAgent
			}
		}
	}
	return InboxKindOther
}

// addressedIndex buckets the timeline's addressed notifications by recipient.
//
// One pass, reused by every name in a roster listing. Building it per name would
// re-read and re-scan the whole timeline once per agent, which on a host with a
// live fleet is the difference between a page and a stall.
func addressedIndex(events []room.Event) map[string][]room.Event {
	out := map[string][]room.Event{}
	for _, e := range events {
		if e.Type != room.EventNotify {
			continue
		}
		to := strings.TrimSpace(e.To)
		if to == "" {
			continue
		}
		out[to] = append(out[to], e)
	}
	return out
}

// Inboxes is ONE parse of the timeline, reusable across every name.
//
// It exists because the timeline is the expensive part and the only part that
// is shared. Measured on a working host: 161k events / 18 MB, ~176 ms to read
// and index, against ~2 small file reads per name afterwards. A caller
// answering "how much is waiting everywhere" and a caller answering "show me
// this one inbox" were each paying that 176 ms separately, on a timer.
//
// A snapshot is IMMUTABLE and carries the stat it was read at, so a caller can
// ask Stale() and re-read only when the log actually moved — the same
// stat-before-parse rule the drain loop follows. That is exact rather than
// approximate: a quiet host revalidates for the price of one os.Stat, and a
// busy one never serves a number from before the message it is being asked
// about.
type Inboxes struct {
	index map[string][]room.Event
	stat  timelineStat
}

// ReadInboxes parses the timeline once and indexes it by recipient.
func ReadInboxes() (*Inboxes, error) {
	stat, err := statTimeline()
	if err != nil {
		return nil, err
	}
	events, err := room.Timeline(0)
	if err != nil {
		return nil, err
	}
	// The stat is taken BEFORE the read, so an append that lands mid-read makes
	// the snapshot look stale and is picked up next time. Taken after, the same
	// append would be silently swallowed: the stat would already describe a file
	// whose new tail this parse never saw.
	return &Inboxes{index: addressedIndex(events), stat: stat}, nil
}

// Stale reports whether the timeline has moved since this snapshot was read.
//
// An error stats as stale: refusing to answer is worse than one extra parse,
// and a snapshot that cannot be checked must not be reported as current.
func (s *Inboxes) Stale() bool {
	if s == nil {
		return true
	}
	stat, err := statTimeline()
	if err != nil {
		return true
	}
	return stat != s.stat
}

// Of returns one name's inbox from this snapshot.
func (s *Inboxes) Of(name, viewer string) (InboxView, error) {
	return inspectInbox(name, viewer, s.index)
}

// InspectInbox returns one name's inbox without touching a single byte of state.
func InspectInbox(name, viewer string) (InboxView, error) {
	snap, err := ReadInboxes()
	if err != nil {
		return InboxView{}, err
	}
	return snap.Of(name, viewer)
}

func inspectInbox(name, viewer string, index map[string][]room.Event) (InboxView, error) {
	name = strings.TrimSpace(name)
	view := InboxView{Name: name, Kind: InboxKind(name, viewer)}
	if name == "" {
		return view, nil
	}

	// readCursor and ReadPending both answer "missing" with a zero value rather
	// than an error, so a name that has never been written to inspects cleanly
	// as an empty inbox instead of failing the page.
	cursor, err := readCursor(name)
	if err != nil {
		return view, err
	}
	view.Cursor = cursor

	materialized, err := ReadPending(name)
	if err != nil {
		return view, err
	}

	items := make([]InboxItem, 0, len(materialized)+len(index[name]))
	for _, p := range materialized {
		items = append(items, InboxItem{
			Seq: p.Seq, TS: p.TS, From: p.Principal, Topic: p.Topic, To: p.To,
			Room: p.Room, Body: p.Body, Delivery: p.Delivery, Demoted: p.Demoted,
			ReadAt:     p.ReadAt,
			Read:       !p.Unread() || (p.Seq > 0 && p.Seq <= cursor),
			PastCursor: p.Seq > 0 && p.Seq <= cursor,
			Source:     InboxSourcePending,
		})
	}

	// A timeline event is folded in only when no materialized record already
	// REPRESENTS it. sameNotification is the same provenance test SnapshotInbox
	// uses, so the two views cannot disagree about what is a duplicate — and it
	// compares fields, never text alone: two agents saying the same sentence are
	// two messages.
	for _, e := range index[name] {
		candidate := Pending{
			SchemaVersion: SchemaVersion, Seq: e.Seq, TS: e.TS, Principal: e.Principal,
			Topic: e.Topic, To: e.To, Room: e.Room, Body: e.Body, Delivery: DeliveryQueued,
		}
		duplicate := false
		for _, p := range materialized {
			if sameNotification(p, candidate) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		delivery := strings.TrimSpace(e.Priority)
		if delivery == "" {
			delivery = DeliveryQueued
		}
		items = append(items, InboxItem{
			Seq: e.Seq, TS: e.TS, From: e.Principal, Topic: e.Topic, To: e.To,
			Room: e.Room, Body: e.Body, Delivery: delivery,
			Read:        e.Seq <= cursor,
			PastCursor:  e.Seq <= cursor,
			Source:      InboxSourceTimeline,
			MatchReason: e.MatchReason,
		})
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].Seq < items[j].Seq })
	for _, it := range items {
		if !it.Read {
			view.Unread++
		}
	}
	view.Items = items
	return view, nil
}

// InspectInboxes lists every name on this host that has an inbox, summarized.
func InspectInboxes(viewer string) ([]InboxHolder, error) {
	snap, err := ReadInboxes()
	if err != nil {
		return nil, err
	}
	return snap.Holders(viewer)
}

// Holders lists every name with an inbox in this snapshot, summarized.
//
// The viewer is always present, first-class rather than a special case bolted on
// by the caller: a person inspecting the fleet's mail is themselves an
// addressable name, and a roster that omitted them would answer "whose mail is
// this" with a list that excludes the one reading it.
func (s *Inboxes) Holders(viewer string) ([]InboxHolder, error) {
	index := s.index

	sources := map[string]map[string]bool{}
	note := func(name, source string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if sources[name] == nil {
			sources[name] = map[string]bool{}
		}
		sources[name][source] = true
	}

	if v := strings.TrimSpace(viewer); v != "" {
		note(v, InboxHolderViewer)
	}
	if FleetNames != nil {
		for _, n := range FleetNames() {
			note(n, InboxHolderCatalog)
		}
	}
	subs, err := Subscriptions()
	if err != nil {
		return nil, err
	}
	for _, sub := range subs {
		note(sub.Subscriber, InboxHolderSubscribed)
		note(sub.To, InboxHolderSubscribed)
	}
	for to := range index {
		note(to, InboxHolderAddressed)
	}

	out := make([]InboxHolder, 0, len(sources))
	for name, srcs := range sources {
		view, verr := inspectInbox(name, viewer, index)
		if verr != nil {
			return nil, verr
		}
		holder := InboxHolder{
			Name: name, Kind: view.Kind, Total: len(view.Items),
			Unread: view.Unread, Cursor: view.Cursor,
		}
		for s := range srcs {
			holder.Sources = append(holder.Sources, s)
		}
		sort.Strings(holder.Sources)
		if n := len(view.Items); n > 0 {
			holder.LatestTS = view.Items[n-1].TS
		}
		out = append(out, holder)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
