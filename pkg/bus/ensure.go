package bus

// R0 — every identity has an inbox, always.
//
// `Subscription.Matches` delivers a 1:1 notification only when a STORED
// subscription carries `To: <name>`. Subscriptions were opt-in, created by
// `bus subscribe` and nothing else — so on a real host there were NONE, and a
// notification addressed to an agent matched nothing and reached nobody. The
// message was durable and undelivered, which is the same failure as a meeting
// room nobody polls, on better bones.
//
// Opt-in addressing is the shape of an opt-in smoke alarm. The address book
// (`bashy agent list`) enumerates the fleet, so the fleet is exactly the set
// that needs inboxes, and creating them is a reconciliation rather than a
// decision.
//
// # An inbox, never a doorbell
//
// The default subscription grants the power to be REACHED and nothing else:
//
//	To             the agent's own name — the DM address, and the whole point
//	Topics         EMPTY. An inbox is not a subscription to the firehose;
//	               topic interest stays something an operator asks for.
//	Room           the public BOARD, so a post addressed to nobody in
//	               particular reaches everybody. Membership, not interest.
//	InterruptFrom  EMPTY, which means NOBODY. Auto-subscribe must never hand
//	               out the power to break into a turn — that is the governance
//	               boundary the subscription type defaults closed on purpose,
//	               and widening it as a side effect of "make agents reachable"
//	               would turn an inbox into an attack surface.
//
// So a message to a freshly-reconciled agent is QUEUED and read at a turn
// boundary. Interruption remains granted by name, per the report/author split
// the fleet uses everywhere else.
//
// # It never overwrites
//
// An existing subscription is left exactly as it is, with ONE additive
// exception: a subscription written before the board existed has no Room, so
// its owner silently misses every broadcast, and reconcile joins it to the
// board. That is adding public membership, not overriding a decision. An
// operator who tuned InterruptFrom or MaxPerMin has expressed a policy, and a
// reconciliation that silently reset either would be a security regression
// disguised as a repair.
//
// # A new inbox starts at the HEAD, not at zero
//
// Since is the sidecar's read offset, and the sidecar delivers everything after
// it. A subscription created with Since = 0 would hand its agent the entire
// timeline the moment it first connects — every unrelated notification the host
// has ever emitted, as if it were new. So a new inbox opens at the current head:
// it receives what arrives from now on, which is what "you now have an address"
// means. Nothing addressed to the agent is lost, because before this there was
// no address to lose it from.

import (
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/room"
)

// BoardRoom is the well-known room every agent joins, so a post addressed to
// NOBODY in particular reaches everybody.
//
// Subscription.Matches accepts on To, Room, or Topics. A default inbox carries
// only `To: <name>`, which is exactly right for a directed post and useless for
// a broadcast — with no recipient there is no To to match, so the post would
// reach nobody while looking sent. Room membership is what makes the board
// public.
const BoardRoom = "board"

// FleetNames is the seam to the agent catalog, injected by the host.
//
// pkg/bus must not import pkg/fleet: the bus is the transport and the catalog
// is policy, and a transport that knows the roster is one you cannot test
// without one. This mirrors steward.OpenRoom and fleet's own LiveProbe — the
// host wires the real thing, and a nil hook simply means the fleet is not
// reconcilable here rather than an error.
var FleetNames func() []string

// Audience is a selector over the address book. A zero value selects nothing;
// the caller decides what an empty selection means.
//
// Binding fields are ANDed; Role is mutually exclusive with all binding fields.
// Thus `--band 4 --tool ycode` is "L4 agents on ycode" rather
// than the union. A union would make a wider blast radius the easier thing to
// type, and on a board the wider blast radius is the one that turns messages
// into noise nobody reads.
type Audience struct {
	Band     int    `json:"band,omitempty"`     // 0 = any
	Tool     string `json:"tool,omitempty"`     // "" = any
	Provider string `json:"provider,omitempty"` // "" = any (anthropic, gemini, …)
	Family   string `json:"family,omitempty"`   // "" = any (opus, sonnet, gemini-flash, …)
	Version  string `json:"version,omitempty"`  // "" = any (5, 4.8, 3.6, …)

	// Role selects on what an agent is DOING rather than on its binding.
	//
	// Every other field here is a property of the agent's tool:model — none
	// says "is currently managing a sprint". So a manager wanting exactly its
	// peers had to post to EVERYONE (over-broad, and capped for anyone who has
	// not declared the concern) or hardcode names, which rot the moment a
	// lease moves. The sprint records know every live lease holder; until now
	// the messaging layer could not ask.
	//
	// Resolution is HOST policy like the rest of FleetSelect: pkg/bus is
	// transport and must not learn to read the sprint store. LIVE holders
	// only — a stale lease names a conductor who died without handing off and
	// an unowned sprint names nobody; addressing either is mail nobody reads.
	Role string `json:"role,omitempty"` // "" = any (conductor, …)
}

// Empty reports a selector that names no criterion.
func (a Audience) Empty() bool {
	return a.Band == 0 && a.Tool == "" && a.Provider == "" && a.Family == "" &&
		a.Version == "" && a.Role == ""
}

// Validate rejects selectors whose criteria cannot be combined. Hosts must
// validate direct FleetSelect calls as well as the Send admission path.
func (a Audience) Validate() error {
	if a.Role == "" {
		return nil
	}
	if strings.TrimSpace(a.Role) == "" {
		return fmt.Errorf("audience: --role requires a nonempty role name")
	}
	var filters []string
	if a.Band != 0 {
		filters = append(filters, "--band")
	}
	if a.Tool != "" {
		filters = append(filters, "--tool")
	}
	if a.Provider != "" {
		filters = append(filters, "--provider")
	}
	if a.Family != "" {
		filters = append(filters, "--family")
	}
	if a.Version != "" {
		filters = append(filters, "--version")
	}
	if len(filters) > 0 {
		return fmt.Errorf("audience: --role cannot be combined with binding filters: %s", strings.Join(filters, ", "))
	}
	return nil
}

// FleetSelect resolves an Audience to agent names, injected by the host for the
// same reason FleetNames is: pkg/bus is the transport and the roster is policy.
//
// `bashy agent list` IS the address book, so a selector has to be answered by
// the catalog that owns it rather than by a copy kept here — a second opinion
// about who is L4 is a second opinion that can drift.
var FleetSelect func(Audience) ([]string, error)

// EnsureSubscription gives subscriber a default inbox if it has none.
//
// Returns true when one was created. Idempotent: an existing subscription is
// returned untouched, so this is safe to call on every catalog load.
func EnsureSubscription(subscriber string) (bool, error) {
	subscriber = strings.TrimSpace(subscriber)
	if subscriber == "" {
		return false, fmt.Errorf("bus: an inbox needs a subscriber name")
	}
	if existing, err := LoadSubscription(subscriber); err == nil && existing.Subscriber != "" {
		// ADDITIVE REPAIR, and the only field this ever touches. A subscription
		// written before the board existed has no Room, so its owner silently
		// misses every broadcast. Joining the public room is not overriding an
		// operator's policy the way resetting InterruptFrom or MaxPerMin would
		// be — those are left exactly as found, here and everywhere.
		if strings.TrimSpace(existing.Room) == "" {
			existing.Room = BoardRoom
			return false, SaveSubscription(existing)
		}
		return false, nil
	}
	return true, SaveSubscription(Subscription{
		Subscriber: subscriber,
		To:         subscriber,
		// Everyone is in the board room, so a broadcast reaches them. This is
		// membership, not interest: it grants no topic subscription and no
		// interrupt rights.
		Room: BoardRoom,
		// Topics, InterruptFrom and MaxPerMin are deliberately left at their
		// zero values — see the package comment. An inbox, not a doorbell.
		Since: timelineHead(),
	})
}

// BindInstance attaches a subscriber's inbox to one Bashy-owned live room card
// for control-socket turn delivery. The returned release restores the previous
// value only while this binding still owns it, so an old session cannot clear a
// newer one during teardown.
func BindInstance(subscriber, instance string) (release func() error, err error) {
	subscriber = strings.TrimSpace(subscriber)
	instance = strings.TrimSpace(instance)
	if subscriber == "" || instance == "" {
		return nil, fmt.Errorf("bus: binding an inbox needs subscriber and instance")
	}
	if _, err := EnsureSubscription(subscriber); err != nil {
		return nil, err
	}
	sub, err := LoadSubscription(subscriber)
	if err != nil {
		return nil, err
	}
	previous := sub.Instance
	sub.Instance = instance
	if err := SaveSubscription(sub); err != nil {
		return nil, err
	}
	return func() error {
		current, err := LoadSubscription(subscriber)
		if err != nil {
			return err
		}
		if current.Instance != instance {
			return nil
		}
		current.Instance = previous
		return SaveSubscription(current)
	}, nil
}

// ReconcileSubscriptions ensures an inbox for every name, and reports which
// ones it had to create.
//
// The report is the point: it makes "every identity is addressable" a checkable
// claim rather than an assumption, which is what the address book rests on.
// A name that fails is collected and reported rather than aborting the sweep —
// one bad name must not leave the rest of the fleet unaddressable.
func ReconcileSubscriptions(names []string) (created []string, err error) {
	var failed []string
	for _, n := range names {
		switch made, e := EnsureSubscription(n); {
		case e != nil:
			failed = append(failed, n)
		case made:
			created = append(created, n)
		}
	}
	if len(failed) > 0 {
		return created, fmt.Errorf("bus: could not open an inbox for %s", strings.Join(failed, ", "))
	}
	return created, nil
}

// timelineHead is the sequence a new inbox starts from.
//
// A read failure yields 0, and that is the safe direction here: the cost is a
// replay of history the agent did not need, which is noise. The opposite
// default — skipping ahead past events on an unreadable timeline — would drop
// messages, and DEMOTE-NEVER-DROP forbids trading a delivery for tidiness.
func timelineHead() int64 {
	events, err := room.Timeline(1)
	if err != nil || len(events) == 0 {
		return 0
	}
	return events[len(events)-1].Seq
}
