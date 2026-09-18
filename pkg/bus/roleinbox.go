package bus

// A ROLE has an inbox, and it is the role's — not its current holder's.
//
// `steward ping --help` states the contract: "It addresses the SEAT, not the
// holder, so a message survives a handoff or a takeover — the holder changes,
// the responsibility does not." Only the prose did that. The code addressed
// `To: Holder.Name`, and the seat records a TOOL identity (`dhnt:tool/codex`,
// because fleet.DetectTool "yields a TOOL, never an agent") while every bus
// subscriber is a fleet AGENT name (`codex-gpt5.6-sol`).
//
// So Subscription.Matches missed on all three of its accept paths at once:
//
//	To      "codex" — no subscriber by that name exists
//	Room    the seat's meet ref — subscribers' room is the public board
//	Topics  EMPTY on every default inbox, by design (see ensure.go: an inbox is
//	        not a subscription to the firehose)
//
// The message was published, stored and durable — DEMOTE-NEVER-DROP held — and
// unreadable by any read verb. That is the worst shape a message store has: not
// lost, so nothing reports it; not deliverable, so nobody gets it.
//
// The repair is to address the thing that does not change. A role's topic is
// derived from its assignment (`role.Assignment.Topic`) and deliberately
// excludes the holder, so it is already the stable name — it just needed to be
// a SUBSCRIBER as well as a topic.
//
// # Why the topic is also the subscriber name
//
// Using the topic string as the inbox name keeps one identity instead of
// minting a second one that could disagree with it — which is the entire class
// of bug this fixes, and its third instance in one day. A reader asks for the
// role by the same string a sender addresses.

import (
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/role"
)

// IsRoleName reports whether a subscriber name is a role topic (`steward.<ref>`,
// `conductor.<ref>`) rather than an agent's own name.
//
// The vocabulary comes from pkg/role rather than a copy kept here, because a
// second list of what counts as a role is a second opinion that drifts — the
// same reasoning that put harness detection behind fleet.DetectTool instead of
// a private marker table. pkg/role has no internal dependencies, so this costs
// no coupling.
//
// It matters on the READ path. `pending` opens an inbox for any name it is
// handed, so that anyone can join the board simply by reading it — no subscribe
// step, no catalog entry, which is the property that makes the board work with
// no setup. For an AGENT that is exactly right, and it opens at the timeline
// head because nothing addressed to a name could exist before that name had an
// inbox.
//
// A role is the opposite case and the difference is not cosmetic: pings are
// published to a role's topic whether or not anyone has ever read it, so its
// backlog is real. Opening a role at the head silently skips precisely the
// messages that were already waiting — which is how a diagnostic read of this
// host's steward seat stranded a ping that had been correctly delivered to the
// timeline minutes earlier.
func IsRoleName(name string) bool {
	kind, _, ok := strings.Cut(strings.TrimSpace(name), ".")
	if !ok || kind == "" {
		return false
	}
	// Only the kinds that actually own an addressable seat. `worker` is not one:
	// a worker is addressed through the conductor that owns it, and inventing an
	// inbox for it here would create an address nothing sends to.
	switch role.Kind(kind) {
	case role.Steward, role.Conductor:
		return true
	}
	return false
}

// EnsureRoleInbox gives a role a durable inbox, keyed on its topic.
//
// Returns true when one was created. Idempotent, so it is safe to call on every
// claim, every heartbeat and every send — which is the point: a ping must not
// depend on a claim having run some earlier day under some earlier version.
//
// The subscription accepts on BOTH To and Topics. A 1:1 ping addresses the
// role by name; a broadcast to everyone holding a role of this kind addresses
// the topic. Both are the same inbox, so a reader cannot miss one by watching
// the other.
//
// It grants no interrupt rights and joins no room: an inbox, not a doorbell,
// exactly as EnsureSubscription does for an agent. A role is a bigger
// responsibility, not a bigger permission.
func EnsureRoleInbox(topic string) (bool, error) {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return false, fmt.Errorf("bus: a role inbox needs a topic")
	}
	if existing, err := LoadSubscription(topic); err == nil && existing.Subscriber != "" {
		// ADDITIVE REPAIR ONLY, and only of the two fields that make the inbox
		// reachable. An operator who tuned InterruptFrom or MaxPerMin expressed
		// a policy; silently resetting either would be a security regression
		// wearing a repair's clothes. Same rule as EnsureSubscription.
		changed := false
		if strings.TrimSpace(existing.To) == "" {
			existing.To = topic
			changed = true
		}
		if !hasTopic(existing.Topics, topic) {
			existing.Topics = append(existing.Topics, topic)
			changed = true
		}
		if changed {
			return false, SaveSubscription(existing)
		}
		return false, nil
	}
	return true, SaveSubscription(Subscription{
		Subscriber: topic,
		To:         topic,
		Topics:     []string{topic},
		// NOT joined to the public board. An agent is a board member because it
		// is a person-shaped participant; a role is an address, and folding
		// every broadcast into it would bury the messages actually sent TO the
		// role under everything sent to everyone.
		//
		// Since is deliberately 0, NOT the timeline head. EnsureSubscription
		// opens an agent's mailbox at the head because nothing addressed to it
		// could exist before it had an address. A role is the opposite case:
		// pings were being published to this topic before the inbox existed,
		// and they are still in the timeline. Starting at 0 delivers that
		// backlog rather than skipping past it — DEMOTE-NEVER-DROP applies to a
		// message that was undeliverable just as much as to one that was
		// refused.
	})
}

func hasTopic(topics []string, want string) bool {
	for _, t := range topics {
		if t == want {
			return true
		}
	}
	return false
}

// SeatPending reads a role inbox: resolve, read, and mark — the same three
// steps `bus pending` takes, exposed so a role's own CLI can offer a read verb
// that does not require its holder to know the topic string.
//
// That mattered more than it sounds. Reading a seat meant
// `bashy bus pending --as steward.dragon-u501-b683b300b1`, which needs both the
// verb and a scope id nobody memorises — and a channel whose read side costs
// that much stays unread, which fails exactly like a broken one while looking
// healthy.
//
// MARK, never delete: the message keeps its place in the buffer with a read
// stamp, so `--all` still answers "what was this seat told, and when" long
// after the fact.
func SeatPending(topic string, peek, all bool) ([]Pending, error) {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return nil, fmt.Errorf("bus: a seat inbox needs a topic")
	}
	if _, err := ResolveFor(topic); err != nil {
		// Not fatal, and deliberately so: whatever was already buffered is still
		// worth showing. Refusing to print a real backlog because one resolution
		// pass failed would trade a partial answer for none.
		_ = err
	}
	var items []Pending
	var err error
	if all {
		items, err = ReadPending(topic)
	} else {
		items, err = UnreadPending(topic)
	}
	if err != nil || peek || all || len(items) == 0 {
		return items, err
	}
	return items, MarkRead(topic, items[len(items)-1].Seq)
}
