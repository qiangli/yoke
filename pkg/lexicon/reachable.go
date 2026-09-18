package lexicon

// The two things `define` could not answer for, and both are ADDRESSES.
//
// Measured 2026-08-03 on a host called dragon:
//
//	$ bashy define dragon
//	dragon — unknown here
//
// That is the machine it was running on. And `define steward` resolved to the
// VERB `bashy steward` rather than to the seat a steward was actively holding.
// So the resolver that knows what every command, env var, alias, interface and
// agent binding on the host means could not name either the host itself or the
// roles running on it.
//
// This is worth fixing on its own merits — "what is dragon" is a fair question
// to ask a glossary — but it is also load-bearing for anything that resolves a
// bare token to a destination. A dispatcher that has to guess whether `steward`
// means a host or a seat is guessing about identity, and guessing about
// identity is the defect that produced three separate bugs in one day.
//
// # Why these are kinds and not a special case
//
// Both go through the same Concept machinery as everything else, so a token
// that is BOTH a host and a role reports as both — `define` already renders
// multi-sense terms ("codex is 2 things here"), and that rendering is exactly
// what a caller needs to refuse rather than guess.

import (
	"os"
	"strings"
)

// Kinds contributed by the reachability inventory.
const (
	// KindHost is a machine this host can name: itself, or an entry in the
	// static host table. NOT a DNS lookup — see Hosts.
	KindHost Kind = "host"
	// KindSeat is a role with an accountable holder on this host: the steward
	// seat, a sprint's conductor. The ADDRESS, not the person holding it.
	KindSeat Kind = "seat"
)

const (
	hostScopeNote = "A machine this host can name — itself, or a name in its static host table. " +
		"Reachability is not implied: the name resolves here, which is not the same as the host being up."
	seatScopeNote = "A ROLE on this host, addressed by the seat rather than by whoever holds it. " +
		"The holder changes across a handoff; the address does not, which is why mail sent to it survives one."
)

// SeatInfo is one role seat the host can name.
//
// Holder is deliberately optional and deliberately not identity-scrubbed the
// way Discover's addresses are: a seat's whole purpose is to be publicly
// answerable, and "who is accountable here" is the question it exists to
// answer. It is a tool or agent name, never a human's.
type SeatInfo struct {
	// Topic is the address (`steward.<scope>`, `conductor.<sprint>`), and it is
	// the ID a caller sends to.
	Topic string
	// Label is what a person calls it ("steward", "conductor:22").
	Label string
	// Holder is who holds it now, empty when vacant.
	Holder string
	// Vacant reports a seat that exists as an address but has no accountable
	// holder — a real and useful answer, and the one `define` must give rather
	// than reporting the seat as unknown.
	Vacant bool
}

// SeatSource is the seam to the role stores, injected by the host.
//
// pkg/lexicon must not import pkg/steward: the glossary is a projection and the
// seat is authority state, and a glossary that reaches into an authority store
// is one you cannot test without one. Same seam shape as bus.FleetNames and
// bus.DetectHarness — and, as with those, WIRING IT IS THE LOAD-BEARING STEP:
// a nil hook means the host simply cannot answer for seats, which is exactly
// how "mechanism built, last hop never wired" keeps happening here.
var SeatSource func() []SeatInfo

// AddReachable projects the host's own name and its role seats.
func (s *Store) AddReachable(ov Overlay) {
	s.addSelfHost(ov)
	s.addSeats(ov)
}

// addSelfHost answers "what is this machine called".
//
// Only the host's OWN name, and only from the kernel. Reading /etc/hosts would
// project every name in it, and on a developer box that file is a private map
// of an internal network — the kind of thing docs/redaction policy exists to
// keep out of a glossary that gets emitted into committed files. The local
// hostname is already public in every prompt and log line on the machine.
func (s *Store) addSelfHost(ov Overlay) {
	name, err := os.Hostname()
	if err != nil {
		return
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	// A FQDN and its short form are one machine, not two. The short form is the
	// term people actually type, so it leads and the full name is an alias.
	short, _, _ := strings.Cut(name, ".")
	c := &Concept{
		ID:         "host:" + short,
		Kind:       KindHost,
		PrefLabel:  short,
		Definition: "this machine",
		ScopeNote:  hostScopeNote,
		Source:     "kernel",
	}
	if short != name {
		c.AltLabels = []string{name}
	}
	s.add(*c, ov)
}

// addSeats projects the role addresses this host can name.
func (s *Store) addSeats(ov Overlay) {
	if SeatSource == nil {
		return
	}
	for _, seat := range SeatSource() {
		topic := strings.TrimSpace(seat.Topic)
		label := strings.TrimSpace(seat.Label)
		if topic == "" || label == "" {
			continue
		}
		def := "the " + label + " seat on this host"
		switch {
		case seat.Vacant:
			// A vacant seat is a REAL answer. Reporting it as unknown would be
			// the same error as a lookup that guesses: it hides the one fact a
			// caller needs, which is that there is an address and nobody behind
			// it.
			def += " — VACANT, no accountable holder"
		case seat.Holder != "":
			def += " — held by " + seat.Holder
		}
		s.add(Concept{
			ID:         "seat:" + topic,
			Kind:       KindSeat,
			PrefLabel:  label,
			AltLabels:  []string{topic},
			Definition: def,
			ScopeNote:  seatScopeNote,
			Source:     "seat-store",
		}, ov)
	}
}
