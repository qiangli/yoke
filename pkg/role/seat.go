package role

// SEAT — the OCCUPANCY of a role.
//
// A seat is not a kind of role. A role says WHICH responsibility (steward of
// this machine, conductor of sprint #3); a seat says WHO holds it right now and
// whether they are still breathing. Role is the noun, seat is the state of it
// being occupied, and Contact is where that occupant can be reached. Those
// three are the whole vocabulary.
//
// # Title, qualification, position, occupancy — four things, not two
//
// It is worth separating these once, because three of them are routinely called
// "role":
//
//	TITLE          steward, conductor — Kind. Many agents may hold a title.
//	QUALIFICATION  whether a given agent CAN hold it. Lives in the fleet
//	               (bands, `agents list --min-band`), judged at appointment.
//	POSITION       one instance of a title — steward of THIS machine-and-login,
//	               conductor of sprint #3. That is Assignment.
//	OCCUPANCY      who holds one position right now. That is a Seat, and it is
//	               where "exactly one" is enforced.
//
// So a host may well have several agents qualified to steward it, and only one
// of them holds the seat. The others are not stewards-in-waiting in any
// recorded sense — they are simply agents the appointment could have gone to.
//
// This type exists because the tree had grown THREE answers to one question:
//
//	steward   Seat + Authority + Liveness, journal-backed, epoch-fenced
//	sprint    weaveStoryLease{Holder, At} against a 30-minute const
//	weave     weaveOrchestratorLease{…, HeartbeatAt, ExpiresAt, Generation}
//
// Three staleness rules for one concept means "is this held, and by whom" is
// answered three times and differently — and the differences were not
// deliberate. Two of them could not say "I don't know", so a missing or
// impossible heartbeat read as HELD, which is the one answer that gets a
// successor to wait for somebody who is never coming back.
//
// # What is unified here is the CONTRACT, not the storage
//
// Each domain keeps its own record and its own extras — steward's epoch,
// weave's Generation and the tool driving it — because those are real
// differences that a common type would have to either drop or force on
// everyone. What they share is the verdict: given a holder, a heartbeat and a
// TTL, is this seat live, lapsed, unknown, or vacant.
//
// No stored format changes, so nothing needs migrating. That is deliberate:
// unifying the words is worth doing today, and moving three live stores is a
// separate decision with a separate risk.
//
// # Unknown is a verdict, not a gap
//
// The tri-state is the part worth copying from steward, and it is the reason
// this is not a bool. "Held and breathing", "held and lapsed", and "held, but
// nothing here can say whether they are alive" call for three different
// actions: leave it, take it over, go and look. Collapsing the third into
// either of the others is how a successor either seizes a live seat or waits on
// a dead one.

import "time"

// Liveness is what can be said about a seat's holder right now.
type Liveness string

const (
	// LivenessVacant — nobody holds it.
	LivenessVacant Liveness = "vacant"
	// LivenessLive — the holder heartbeated within the TTL.
	LivenessLive Liveness = "live"
	// LivenessLapsed — held, and the heartbeat is older than the TTL. Takeable.
	LivenessLapsed Liveness = "lapsed"
	// LivenessUnknown — held, and nothing here can say whether the holder is
	// alive: no heartbeat was recorded, or the one recorded is impossible.
	//
	// It is NOT a weaker "lapsed". A lapsed seat is evidence the holder stopped;
	// an unknown one is the absence of evidence either way, and the correct
	// response is to look rather than to seize.
	LivenessUnknown Liveness = "unknown"
)

// Takeable reports a seat a successor may claim without the incumbent's
// cooperation. Unknown is deliberately excluded — see LivenessUnknown.
func (l Liveness) Takeable() bool { return l == LivenessVacant || l == LivenessLapsed }

// clockSkew is the tolerance for a heartbeat slightly ahead of us.
//
// Two machines' clocks differ, and a filesystem's timestamp granularity is its
// own source of drift, so a heartbeat a moment in the future is ordinary. One
// materially in the future is not, and is refused below.
const clockSkew = 2 * time.Second

// Seat is who holds a role and when they last said so.
type Seat struct {
	// Holder is the occupant: an AGENT NAME as bashy resolves it, not a free
	// string and not a human's guess at one.
	//
	// The name is the RUNTIME key — one conversation store, one kb attribution,
	// one bus cursor hang off it — and it resolves through the fleet to
	// `tool:model`, the CAPABILITY key. Both matter and they are not
	// interchangeable: two agents may share a binding and still be separate
	// occupants, which is exactly why a seat records the name.
	//
	// What must never be stored here is a nickname or a band. A tier floats as
	// the model landscape shifts and a shorthand rots into a lie, so a seat
	// that recorded one would eventually name an occupant that no longer means
	// what the record says. Resolve at write time; store what resolved.
	Holder string `json:"holder,omitempty"`
	// AcquiredAt is when this tenure began.
	AcquiredAt time.Time `json:"acquired_at,omitzero"`
	// HeartbeatAt is the last time the holder reported being alive. Zero means
	// none was recorded — which is UNKNOWN, never "fine".
	HeartbeatAt time.Time `json:"heartbeat_at,omitzero"`
	// TTL is how long a heartbeat stays believable.
	TTL time.Duration `json:"ttl,omitempty"`
}

// Live reports the seat's state at a moment.
//
// The rules, in the order they are applied and for the reason each exists:
//
//	no holder            vacant
//	no heartbeat         unknown — held per the record, nothing says it breathes
//	heartbeat ahead      unknown — a clock that disagrees is not evidence, and
//	                     a future heartbeat would never lapse, keeping a dead
//	                     holder "live" forever
//	heartbeat before
//	  the tenure began    unknown — it belongs to a previous occupant
//	older than the TTL    lapsed
//	otherwise             live
func (s Seat) Live(now time.Time) Liveness {
	switch {
	case s.Holder == "":
		return LivenessVacant
	case s.HeartbeatAt.IsZero():
		return LivenessUnknown
	case s.HeartbeatAt.After(now.Add(clockSkew)):
		return LivenessUnknown
	case !s.AcquiredAt.IsZero() && s.HeartbeatAt.Before(s.AcquiredAt.Add(-clockSkew)):
		// A heartbeat predating the tenure it claims to belong to is a stale
		// record left by whoever held the seat last, not a signal about the
		// current occupant.
		return LivenessUnknown
	case s.TTL > 0 && now.Sub(s.HeartbeatAt) >= s.TTL:
		return LivenessLapsed
	default:
		return LivenessLive
	}
}

// Held reports a seat with an occupant, whatever their liveness.
func (s Seat) Held() bool { return s.Holder != "" }
