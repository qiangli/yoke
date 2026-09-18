package principal

// Roles are the names the ADDRESSER already resolves — `bashy ping steward`
// posts to the seat's stable topic — and the resolver must answer about them
// the same way, or two commands that both resolve names give contradictory
// answers about the same name in the same second (measured on a live host
// 2026-08-25: `ping steward` delivered while `whois steward` said "names
// nothing"). See docs/agent-comms-synergy.md.
//
// pkg/principal cannot import pkg/bus (bus-adjacent packages import this one
// back), so the role table arrives through a registered source: pkg/bus
// bridges its HostRoles registry — the very table the addresser consults —
// into here from its own init(). One table, two front doors, no drift.

import (
	"strings"

	"github.com/qiangli/yoke/pkg/room"
)

// HostRole is an addressable seat on this host. Label is what a person types
// ("steward", "conductor:22"); Topic is the stable board address it resolves
// to ("steward.<scope>"), which is what makes mail survive a handover.
type HostRole struct {
	Label string
	Topic string
	// Holder is who occupies the seat RIGHT NOW, or empty for a vacant one.
	//
	// It is carried rather than looked up because a seat with no answer to "who
	// is in it" is not a usable answer: the reason to ask about `conductor:99`
	// is to know who is accountable for sprint 99 today. It is also why nothing
	// may CACHE this — the holder is exactly the part that changes.
	Holder string
}

// roleSources are the registered role tables. Composed, never replaced: the
// bridge from pkg/bus is one source, and a host may add its own.
var roleSources []func() []HostRole

// RegisterRoleSource adds a source of addressable roles. Sources compose;
// registering never replaces an earlier registration.
func RegisterRoleSource(src func() []HostRole) {
	if src == nil {
		return
	}
	roleSources = append(roleSources, src)
}

// hostRoles lists every addressable role the registered sources know.
func hostRoles() []HostRole {
	var out []HostRole
	for _, src := range roleSources {
		out = append(out, src()...)
	}
	return out
}

// resolveRole answers for a seat, by label or by its topic — the same two
// spellings the addresser accepts.
func (r *Resolver) resolveRole(name string) (Resolution, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Resolution{}, false
	}
	for _, hr := range hostRoles() {
		if !strings.EqualFold(hr.Label, name) && !strings.EqualFold(hr.Topic, name) {
			continue
		}
		return Resolution{
			URN: URN(KindRole, hr.Label, r.owner), Kind: KindRole, Name: hr.Label,
			Owner: r.owner, Source: SourceHost, Confidence: Declared,
			Summary: roleSummary(hr),
			Facts:   roleFacts(hr),
			Contacts: []Contact{{
				Method: "mb", Address: "bashy ping " + hr.Label + " \"<message>\"",
				Source: SourceHost, Confidence: Declared, Live: true, Cost: 5,
			}},
		}, true
	}
	return Resolution{}, false
}

// roleSummary names the CURRENT holder, because that is the question. A seat
// answer that only confirms the seat exists sends the asker off to find the
// same thing again somewhere else.
func roleSummary(hr HostRole) string {
	if h := strings.TrimSpace(hr.Holder); h != "" {
		// A held seat with no LIVE reader is still a mailbox — say which it is.
		//
		// Two wrong answers are possible here and the summary must avoid both.
		// Claiming "mail survives a handover" when nothing is live is the
		// failure pkg/role names: "an open room with no live holder is a LIE."
		// But claiming UNREACHABLE is just as wrong in the other direction,
		// because the bus stores mail regardless of liveness — deliveryState
		// returns `queued` for a recipient that is simply behind, and an
		// external agentic CLI is only live at its turn boundaries, so it would
		// read as unreachable between every turn while its mail piles up
		// correctly.
		//
		// So the distinction is PUSH vs QUEUE, not reachable vs not.
		if t, _ := room.OwnerTransportFor(h); !t.Deliverable() {
			return "seat on this host, held by " + h +
				" — no live session, so mail QUEUES for its next read rather than being pushed"
		}
		return "addressable seat on this host, currently held by " + h +
			" — mail to it survives a handover"
	}
	return "addressable seat on this host, currently VACANT — mail to it waits for a holder"
}

func roleFacts(hr HostRole) [][2]string {
	facts := [][2]string{{"topic", hr.Topic}}
	if h := strings.TrimSpace(hr.Holder); h != "" {
		return append(facts, [2]string{"holder", h})
	}
	return append(facts, [2]string{"holder", "(vacant)"})
}
