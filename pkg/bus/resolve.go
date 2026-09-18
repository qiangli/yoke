package bus

// RESOLVE ON READ — the half of delivery that needs no daemon.
//
// The sidecar pre-resolves notifications off an agent's critical path, which is
// the right design for INTERRUPTS: breaking into a running turn has to happen
// while the turn is running, so somebody must be watching. But nothing was
// watching. On a real host `bus subscriptions` was empty and no sidecar process
// existed, so a notification sat in the timeline matching nothing, and even
// after R0 gave every identity an inbox it stayed unresolved until a human
// happened to run `bus sidecar --once`.
//
// The observation that closes the gap cheaply: QUEUEING NEEDS NO SOCKET. Only
// an interrupt does. Everything else is "append to the agent's buffer", and the
// agent asking for its buffer is a perfectly good moment to do it.
//
// So `bus pending` resolves for its own subscriber before reading. That is the
// (c) half of the agreed (a)+(c) model, and it covers the cases (a) cannot:
//
//	cold          not running when the message was sent; resolves on next read
//	shell-only    running under bashy but not bashy-LAUNCHED, so it has no
//	              control socket and no sidecar could ever push to it
//	no sidecar    nobody started one, which is the state every host is in
//
// The sidecar remains the optimisation, not the requirement. A host with one
// gets interrupts; a host without one still gets every message, one turn later.
//
// # Why an interrupt is not attempted here
//
// The agent is READING RIGHT NOW. Urgency has already been satisfied by the act
// of asking, so re-delivering as an interrupt would break into the very turn
// that came to collect it. Entries resolved on this path are therefore recorded
// as queued — which is what actually happened, and a record that says what
// happened is the whole point.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/ref"
	"github.com/qiangli/yoke/pkg/room"
)

// RegisterRefs wires message-board posts and host-room bus events into the
// shared ref registry.
func RegisterRefs(g *ref.Registry) {
	g.Register(ref.MB, ref.ResolverFunc(resolveMBRef))
	g.Register(ref.Bus, ref.ResolverFunc(resolveBusRef))
}

func resolveMBRef(id string) (ref.Node, error) {
	seq, err := parsePositiveSeq("mb", id)
	if err != nil {
		return ref.Node{}, err
	}
	p, ok, err := findPost(seq)
	if err != nil {
		return ref.Node{}, err
	}
	if !ok {
		return ref.Node{}, fmt.Errorf("%w: mb:%d", ref.ErrNotFound, seq)
	}
	n := ref.NewNode(ref.MB, strconv.FormatInt(p.Seq, 10))
	n.Title = refFirstLine(p.Body)
	n.Status = "posted"
	n.Where = BoardDir()
	n.Open = "bashy mb show " + strconv.FormatInt(p.Seq, 10)
	return n, nil
}

func resolveBusRef(id string) (ref.Node, error) {
	seq, err := parsePositiveSeq("bus", id)
	if err != nil {
		return ref.Node{}, err
	}
	e, ok, err := findEvent(seq)
	if err != nil {
		return ref.Node{}, err
	}
	if !ok {
		return ref.Node{}, fmt.Errorf("%w: bus:%d", ref.ErrNotFound, seq)
	}
	n := ref.NewNode(ref.Bus, strconv.FormatInt(e.Seq, 10))
	n.Title = eventTitle(e)
	n.Where = room.Dir()
	n.Open = "bashy bus watch --json --from " + strconv.FormatInt(e.Seq, 10)
	return n, nil
}

func parsePositiveSeq(kind, id string) (int64, error) {
	seq, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(id, "#")), 10, 64)
	if err != nil || seq < 1 {
		return 0, fmt.Errorf("%s: %q is not a positive sequence", kind, id)
	}
	return seq, nil
}

func findPost(seq int64) (Post, bool, error) {
	posts, err := Posts()
	if err != nil {
		return Post{}, false, err
	}
	for _, p := range posts {
		if p.Seq == seq {
			return p, true, nil
		}
	}
	return findArchivedPost(seq)
}

func findArchivedPost(seq int64) (Post, bool, error) {
	paths, err := filepath.Glob(filepath.Join(archiveDir(), "*.jsonl"))
	if err != nil {
		return Post{}, false, err
	}
	for _, path := range paths {
		p, ok, err := scanPostFile(path, seq)
		if err != nil {
			return Post{}, false, err
		}
		if ok {
			return p, true, nil
		}
	}
	return Post{}, false, nil
}

func scanPostFile(path string, seq int64) (Post, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return Post{}, false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var p Post
		if json.Unmarshal([]byte(line), &p) != nil {
			continue
		}
		if p.Seq == seq {
			return p, true, nil
		}
	}
	return Post{}, false, sc.Err()
}

func findEvent(seq int64) (room.Event, bool, error) {
	events, err := room.Timeline(0)
	if err != nil {
		return room.Event{}, false, err
	}
	for _, e := range events {
		if e.Seq == seq {
			return e, true, nil
		}
	}
	return findArchivedEvent(seq)
}

func findArchivedEvent(seq int64) (room.Event, bool, error) {
	paths, err := filepath.Glob(filepath.Join(room.Dir(), "archive", "*.jsonl"))
	if err != nil {
		return room.Event{}, false, err
	}
	for _, path := range paths {
		e, ok, err := scanEventFile(path, seq)
		if err != nil {
			return room.Event{}, false, err
		}
		if ok {
			return e, true, nil
		}
	}
	return room.Event{}, false, nil
}

func scanEventFile(path string, seq int64) (room.Event, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return room.Event{}, false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e room.Event
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if e.Seq == seq {
			return e, true, nil
		}
	}
	return room.Event{}, false, sc.Err()
}

func eventTitle(e room.Event) string {
	subject := strings.TrimSpace(e.Topic)
	if subject == "" {
		subject = refFirstLine(e.Body)
	} else if body := refFirstLine(e.Body); body != "" {
		subject += " - " + body
	}
	if subject == "" {
		return strings.TrimSpace(e.Type)
	}
	return strings.TrimSpace(e.Type + " " + subject)
}

func refFirstLine(s string) string {
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// ResolveFor delivers everything subscriber has not yet been given, into its
// pending buffer. Returns how many were queued.
//
// Ordering is the sidecar's rule, and it is not negotiable: the subscription's
// offset advances only AFTER the buffer has been written. Advancing first and
// then failing to append would consume a notification the agent never learns
// existed — a silent drop, which leaves it acting on stale assumptions. A
// duplicate is recoverable; a loss is not.
//
// A subscriber with no subscription resolves nothing and is not an error: after
// R0 that means a name outside the address book, and refusing to print an
// empty buffer for it would help nobody.
func ResolveFor(subscriber string) (int, error) {
	sub, err := LoadSubscription(subscriber)
	if err != nil || sub.Subscriber == "" {
		return 0, nil
	}
	events, err := room.Timeline(0)
	if err != nil {
		return 0, err
	}
	return resolveForEvents(sub, events)
}

// resolveForEvents is ResolveFor over an already-parsed timeline. SnapshotInbox
// reads the timeline ONCE and hands it to both halves of the drain: a parse of
// the host timeline is the expensive step (hundreds of ms on a mature host),
// and doing it twice per snapshot was half of the idle-session CPU in coreutils
// story #127.
func resolveForEvents(sub Subscription, events []room.Event) (int, error) {
	subscriber := sub.Subscriber
	var high int64
	queued := 0
	for _, e := range events {
		if e.Seq > high {
			high = e.Seq
		}
		if e.Seq <= sub.Since || !sub.Matches(e) {
			continue
		}
		// DeliveryQueued unconditionally: see the package comment. The caller is
		// reading, so an interrupt would break into the turn that came to collect
		// the message.
		if err := AppendPending(subscriber, Pending{
			SchemaVersion: SchemaVersion,
			Seq:           e.Seq, TS: e.TS,
			Principal: e.Principal, Topic: e.Topic, To: e.To, Room: e.Room,
			Body:     e.Body,
			Delivery: DeliveryQueued,
		}); err != nil {
			return queued, err
		}
		queued++
	}

	if queued > 0 || high > sub.Since {
		sub.Since = high
		if err := SaveSubscription(sub); err != nil {
			return queued, err
		}
	}
	return queued, nil
}
