// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/qiangli/yoke/pkg/principal"
)

// TTL is how long the seat survives without a heartbeat before its liveness is
// reported as LAPSED.
//
// Thirty minutes, matching the claim registry's lease, and for the same reason: an
// LLM steward works in bursts and may be idle between them. Too short and a
// thinking steward loses the seat mid-thought; too long and a crashed one blocks the
// host until a human intervenes. Erring long is cheap here because the cost is a
// wait, not a deadlock — a lapsed seat is claimable without authorization, and the
// fencing epoch makes a late-returning incumbent safe.
const TTL = 30 * time.Minute

// Liveness is what the heartbeat can honestly tell us. There is deliberately no
// "dead" — see LivenessLapsed.
type Liveness string

const (
	// LivenessLive — the holder heartbeated within the TTL, and the heartbeat record
	// AGREES with the journal (right holder, right epoch, sane timestamps).
	LivenessLive Liveness = "live"

	// LivenessLapsed — a heartbeat record that agrees with the journal, and is older
	// than the TTL.
	//
	// This proves A LIVENESS LAPSE AND NOTHING MORE. It does not prove the holder
	// crashed, quit, or died: it may be mid-thought, rate-limited, paused at a human
	// prompt, or on a bad network, and it may come back at any moment. The seat is
	// ordinarily claimable, but the claim FENCES rather than buries the incumbent —
	// which is exactly why a lapse is safe to act on despite proving so little.
	LivenessLapsed Liveness = "lapsed"

	// LivenessUnknown — the seat is held per the journal, and we CANNOT SAY anything
	// about the holder's liveness. Missing heartbeat file, unparsable file, wrong
	// schema, a holder or epoch that disagrees with the journal, a timestamp from the
	// future, a heartbeat predating the tenure it claims to belong to.
	//
	// UNKNOWN IS NOT CLAIMABLE. This is the difference between this revision and the
	// one before it, and it is the whole safety argument. "I looked and the incumbent
	// is late" is a fact about the incumbent. "I cannot find or trust the liveness
	// record" is a fact about the RECORD — and every way of producing it is also a way
	// an attacker (or a bug, or a botched backup restore) produces it deliberately.
	// Deleting one file must not be enough to take the host's seat away from a
	// healthy steward, so recovery from unknown goes through Takeover, which demands
	// an authorization capability and says so in the permanent record.
	LivenessUnknown Liveness = "unknown"

	// LivenessVacant — nobody holds the seat, per the journal.
	LivenessVacant Liveness = "vacant"
)

// Authority is the seat state DERIVED FROM THE JOURNAL — the half that matters, and
// the half that survives losing everything else.
//
// Nothing here is read from seat.json. Delete seat.json and every field below is
// reconstructed by replay; that is what makes crash recovery work with no handoff
// note and no cooperation from the incumbent.
type Authority struct {
	Holder     principal.Ref `json:"holder"`
	Epoch      uint64        `json:"epoch"`
	Vacant     bool          `json:"vacant"`
	AcquiredAt time.Time     `json:"acquired_at,omitzero"`

	// TakenOverFrom / Authz are set when the current tenure began with an authorized
	// takeover, so the record always says who seized the seat, from whom, and under
	// what capability.
	TakenOverFrom *principal.Ref `json:"taken_over_from,omitempty"`
	Authz         *AuthzRef      `json:"authz,omitempty"`
}

// Seat is the LIVENESS record (seat.json). It is a CACHE, never an authority:
// holder and epoch are duplicated here only so a status check is one small read
// instead of a full replay, and any disagreement with the journal is resolved in the
// journal's favour — by DISCARDING the cache, not by trusting it.
type Seat struct {
	SchemaVersion string        `json:"schema_version"`
	Holder        principal.Ref `json:"holder"`
	Epoch         uint64        `json:"epoch"`
	AcquiredAt    time.Time     `json:"acquired_at"`
	Heartbeat     time.Time     `json:"heartbeat"`
	PID           int           `json:"pid,omitempty"`
	Intent        string        `json:"intent,omitempty"`
}

// View is the complete answer to "who is the steward, and are they alive?" —
// authority from the journal, liveness from the heartbeat, kept visibly separate.
type View struct {
	SchemaVersion string    `json:"schema_version"`
	Authority     Authority `json:"authority"`
	Liveness      Liveness  `json:"liveness"`
	Heartbeat     time.Time `json:"heartbeat,omitzero"`
	Since         time.Time `json:"since,omitzero"`
	PID           int       `json:"pid,omitempty"`
	Intent        string    `json:"intent,omitempty"`

	// LivenessReason says WHY liveness is unknown. An unexplained "unknown" is an
	// invitation to guess, and the guess is always "it's probably fine".
	LivenessReason string `json:"liveness_reason,omitempty"`

	// Claimable reports whether Claim would succeed right now: the seat is VACANT, or
	// a trustworthy heartbeat says the holder has LAPSED. Nothing else. An unknown
	// liveness is explicitly NOT claimable — recovering from it is a Takeover.
	Claimable bool `json:"claimable"`
}

// deriveAuthority replays the seat lifecycle events. Epoch is MONOTONIC: a release
// does not lower it, because an epoch that could go backwards would let a fenced
// holder become un-fenced by waiting.
func deriveAuthority(rep *Replay) Authority {
	var a Authority
	a.Vacant = true
	for _, e := range rep.Entries {
		switch e.Kind {
		case KindSeatClaimed, KindSeatTakeover:
			a.Holder = e.Actor
			a.Epoch = e.Epoch
			a.Vacant = false
			a.AcquiredAt = parseTime(e.Time)
			a.TakenOverFrom, a.Authz = nil, nil
			if e.Kind == KindSeatTakeover {
				if prior := priorHolderOf(e); prior != nil {
					a.TakenOverFrom = prior
				}
				a.Authz = e.Authz
			}
		case KindSeatReleased:
			a.Vacant = true
			a.Holder = principal.Ref{}
			a.AcquiredAt = time.Time{}
			a.TakenOverFrom, a.Authz = nil, nil
			// Epoch is deliberately NOT reset — see the monotonicity note above.
		}
	}
	// The journal's high-water epoch wins over the last seat event's: an entry can
	// never be written under an epoch that a later claim would reuse.
	if rep.MaxEpoch > a.Epoch {
		a.Epoch = rep.MaxEpoch
	}
	return a
}

// priorHolderOf recovers the fenced holder recorded on a takeover entry. Stored in
// Evidence (kind "principal") so it survives in the hash-chained record rather than
// only in prose.
func priorHolderOf(e Entry) *principal.Ref {
	for _, ev := range e.Evidence {
		if ev.Kind == "principal" && ev.Note == "prior-holder" {
			return &principal.Ref{Name: ev.Ref}
		}
	}
	return nil
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// SameHolder reports whether two refs are the same logical steward.
//
// Compared by episode-or-(name,host), never by PID: one logical agent runs many
// processes (a shell, a subagent, a hook), and none of them should be told it is
// colliding with itself.
//
// Note what this does NOT grant. Being the same logical principal gets you past the
// IDENTITY gate and no further: the epoch is checked first and separately, so the
// same agent returning with a token from a tenure that has since ended is FENCED,
// exactly like a stranger would be. Identity is not authority; the token is.
func SameHolder(a, b principal.Ref) bool {
	if a.Episode != "" && a.Episode == b.Episode {
		return true
	}
	return a.Name != "" && a.Name == b.Name && a.Host == b.Host
}

// Self resolves who this process is, minting a stable identity when the ambient
// environment has none. Delegates to the coord identity rule so a steward, a claim,
// and an audit record all name the same agent the same way.
func Self() principal.Ref { return selfRef() }

// ─── errors ───────────────────────────────────────────────────────────────────

// ErrHeld is returned by Claim when a LIVE steward already holds the seat. It is
// not an error to route around — it is the singleton working.
//
// Yours reports that the live holder IS the caller. That is no longer a shortcut to a
// free refresh — see Claim — so the case gets its own message: what a live holder needs
// is Heartbeat, which presents the fencing epoch, and presenting the epoch is the entire
// point. A holder that cannot produce its epoch is telling us something, and what it is
// telling us is that it may be a zombie.
type ErrHeld struct {
	View  View
	Yours bool
}

func (e *ErrHeld) Error() string {
	if e.Yours {
		return fmt.Sprintf("steward: you already hold the seat (epoch %d, last heartbeat %s), and it is LIVE. "+
			"Claiming does not renew it: renewing is `steward heartbeat --epoch %d`, which PRESENTS the epoch, and a claim "+
			"that refreshed a held tenure without one would be a way to keep a tenure alive without ever proving it is "+
			"still yours — which is exactly what a steward that was superseded while it was away would do",
			e.View.Authority.Epoch, e.View.Heartbeat.Local().Format(time.RFC3339), e.View.Authority.Epoch)
	}
	return fmt.Sprintf("steward: the seat is held by %s (epoch %d, last heartbeat %s) — "+
		"this machine/account has exactly one steward. If they are truly gone and you must recover the seat, "+
		"that is a takeover: mint an authorization (`steward authorize --action takeover`) and run "+
		"`steward takeover --grant <id>`",
		holderName(e.View.Authority.Holder), e.View.Authority.Epoch,
		e.View.Heartbeat.Local().Format(time.RFC3339))
}

// ErrLivenessUnknown is returned by Claim when the seat is held but its liveness
// record cannot be trusted. It is NOT an invitation to claim anyway.
type ErrLivenessUnknown struct{ View View }

func (e *ErrLivenessUnknown) Error() string {
	return fmt.Sprintf("steward: %s holds the seat at epoch %d, and the liveness record cannot be trusted (%s). "+
		"That is a fact about the RECORD, not about the holder — it does not mean they are gone, and every way of "+
		"producing it (a deleted file, a restored backup, a hostile `rm`) is also a way to produce it DELIBERATELY. "+
		"Claiming is refused. If you must recover the seat, take it over on the record: "+
		"`steward authorize` then `steward takeover --grant <id>`. The holder can also simply prove liveness with "+
		"`steward heartbeat --epoch %d`, which rebuilds the record from the journal",
		holderName(e.View.Authority.Holder), e.View.Authority.Epoch, e.View.LivenessReason, e.View.Authority.Epoch)
}

// ErrFenced is returned when a mutation arrives bearing a superseded epoch — the
// classic returning-zombie case: a steward that lapsed, was taken over, and came
// back believing it still holds the seat.
//
// It is REJECTED, loudly, rather than being allowed to interleave its writes with
// the current steward's. This is the entire payoff of the epoch: a stale heartbeat
// only ever proved a liveness lapse, so the incumbent coming back is not a bug — it
// is EXPECTED — and the fence is what makes it harmless.
type ErrFenced struct {
	Presented uint64
	Current   uint64
	Holder    principal.Ref
}

func (e *ErrFenced) Error() string {
	return fmt.Sprintf("steward: fenced — you presented epoch %d but the seat is at epoch %d (held by %s). "+
		"Your tenure ended while you were away; this mutation is rejected. "+
		"Re-read the journal (`steward log`) before doing anything else: the world moved on without you",
		e.Presented, e.Current, holderName(e.Holder))
}

// ErrNoEpoch is returned when an authoritative mutation presents no fencing token.
//
// There is no "0 means whatever I currently hold". That convenience was the hole:
// it let a returning zombie — who by definition does not know its tenure ended — get
// its writes accepted by simply not mentioning an epoch, which is precisely what an
// agent that does not know it was superseded will do.
type ErrNoEpoch struct{ Current uint64 }

func (e *ErrNoEpoch) Error() string {
	return fmt.Sprintf("steward: this mutation presented no fencing epoch. Every authoritative write must present the "+
		"epoch it believes it holds (--epoch, or $%s exported at claim time); the seat is currently at epoch %d. "+
		"There is deliberately no 'use whatever is current' shortcut: an agent that does not know its tenure ended is "+
		"exactly the agent that would take it",
		EpochEnv, e.Current)
}

// ErrNotHolder is returned when a non-holder attempts an authoritative write.
type ErrNotHolder struct {
	Actor  principal.Ref
	Holder principal.Ref
	Vacant bool
}

func (e *ErrNotHolder) Error() string {
	if e.Vacant {
		return "steward: the seat is vacant — claim it first (`steward claim`) before writing to the journal"
	}
	return fmt.Sprintf("steward: %s holds the seat; %s may not write to the journal",
		holderName(e.Holder), holderName(e.Actor))
}

func holderName(r principal.Ref) string {
	if r.Name != "" {
		return r.Name
	}
	if r.Episode != "" {
		return r.Episode
	}
	return "an unnamed steward"
}

// ─── the authority gate ───────────────────────────────────────────────────────

// authorize is THE gate. Every authoritative journal append in this package goes
// through it — record, decide, verify, transcript, workstream open/update/close,
// checkpoint, reconcile, and repair — with no exceptions and no side doors.
//
// The gates, in order, and the order is load-bearing:
//
//  1. The journal must be READABLE. A corrupt tail refuses the write rather than
//     forking the chain around the damage. (Repair is the one caller that legally
//     starts from a corrupt journal, and it uses authorizeDamaged for exactly that.)
//  2. The seat must be HELD. Nobody writes the host's authoritative record on a
//     vacant seat.
//  3. A fencing epoch must be PRESENTED. Zero is not a value, it is an absence.
//  4. The presented epoch must be the CURRENT epoch — checked BEFORE identity.
//  5. The actor must be the HOLDER.
//
// Why 4 before 5: the case this exists for is a steward that lapsed, was taken over,
// and came back still holding its old epoch. By then it is no longer the holder, so
// checking identity first would reject it as a mere bystander (ErrNotHolder) and
// never tell it the one thing it needs to know — your tenure ENDED, the world moved
// on, re-read the journal. Both errors refuse the write, so safety is identical; but
// only one of them explains a zombie to itself, and an agent that misreads "you are
// not the holder" as "I should just claim the seat again" will happily overwrite the
// steward that replaced it.
//
// And gate 4 is what fences the SAME logical principal holding a stale token: being
// yourself is not a credential, and a token from a tenure that ended is stale no
// matter whose hand it is in.
func authorize(rep *Replay, actor principal.Ref, epoch uint64) (Authority, error) {
	if rep.Corrupt {
		return Authority{}, &ErrCorruptTail{Line: rep.CorruptLine, Reason: rep.CorruptReason, Kind: rep.CorruptKind, ValidEntries: len(rep.Entries)}
	}
	return authorizeDamaged(rep, actor, epoch)
}

// authorizeDamaged is authorize WITHOUT the readability gate — for Repair alone,
// whose entire job is to act on a journal it has just found damaged. It still
// demands the seat, a nonzero epoch, the current epoch, and the holder: a torn tail
// is not a licence to let a stranger truncate the host's record.
func authorizeDamaged(rep *Replay, actor principal.Ref, epoch uint64) (Authority, error) {
	auth := deriveAuthority(rep)
	if auth.Vacant {
		return Authority{}, &ErrNotHolder{Actor: actor, Vacant: true}
	}
	if epoch == 0 {
		return Authority{}, &ErrNoEpoch{Current: auth.Epoch}
	}
	if epoch != auth.Epoch {
		return Authority{}, &ErrFenced{Presented: epoch, Current: auth.Epoch, Holder: auth.Holder}
	}
	if !SameHolder(auth.Holder, actor) {
		return Authority{}, &ErrNotHolder{Actor: actor, Holder: auth.Holder}
	}
	return auth, nil
}

// appendAuthorized is the ONLY path from a caller to the journal. It gates, stamps
// the authorized epoch onto the entry, and appends.
//
// The caller must hold the store lock and must have replayed to obtain rep.
func (s *Store) appendAuthorized(rep *Replay, e Entry, epoch uint64, now time.Time) (Entry, error) {
	auth, err := authorize(rep, e.Actor, epoch)
	if err != nil {
		return Entry{}, err
	}
	if e.Kind.SeatEvent() {
		// Seat lifecycle mints its own epoch and must go through Claim / Takeover /
		// Release, which know how to bump it. Letting a generic write forge one would
		// make the fencing ladder climbable by anyone already on it.
		return Entry{}, fmt.Errorf("steward: %s is a seat lifecycle event — use claim/takeover/release, not a generic write", e.Kind)
	}
	// The entry is stamped with the epoch the gate just VERIFIED, not with whatever
	// the caller put in the struct.
	e.Epoch = auth.Epoch
	return appendEntry(s.journalPath(), rep, e, now)
}

// ─── views ────────────────────────────────────────────────────────────────────

// Status returns the current seat view: authority replayed from the journal,
// liveness read from the heartbeat file — and only believed if it checks out.
func (s *Store) Status(now time.Time) (View, error) {
	now = mustUTC(now)
	rep, err := s.Replay()
	if err != nil {
		return View{}, err
	}
	return s.viewFrom(rep, now), nil
}

// validateSeatCache decides whether a heartbeat record may be believed AT ALL.
//
// The cache is not evidence of authority — the journal is — so every field it
// duplicates is a field it can DISAGREE with, and a disagreement is never resolved
// in the cache's favour. It is thrown away, and liveness becomes unknown.
//
// Each check is here because the alternative is a way to take the seat:
//
//	schema     — a record this version cannot fully parse may mean something else.
//	holder     — a seat.json naming someone else is either stale or planted.
//	epoch      — a seat.json from a previous tenure says nothing about this one.
//	zero beat  — "no heartbeat" is not "an old heartbeat".
//	future     — a heartbeat from the future keeps a dead holder "live" forever;
//	             a heartbeat from before its own tenure began is incoherent.
func validateSeatCache(seat Seat, auth Authority, now time.Time) error {
	if seat.SchemaVersion != SchemaVersion {
		return fmt.Errorf("heartbeat record has schema %q, not %q", seat.SchemaVersion, SchemaVersion)
	}
	if !SameHolder(seat.Holder, auth.Holder) {
		return fmt.Errorf("heartbeat record names %s but the journal says %s holds the seat",
			holderName(seat.Holder), holderName(auth.Holder))
	}
	if seat.Epoch != auth.Epoch {
		return fmt.Errorf("heartbeat record is for epoch %d but the seat is at epoch %d", seat.Epoch, auth.Epoch)
	}
	if seat.Heartbeat.IsZero() {
		return fmt.Errorf("heartbeat record carries no heartbeat")
	}
	if seat.Heartbeat.UTC().After(now.Add(clockSkew)) {
		return fmt.Errorf("heartbeat is in the future (%s) — it would never lapse",
			seat.Heartbeat.UTC().Format(time.RFC3339))
	}
	if !auth.AcquiredAt.IsZero() && seat.Heartbeat.UTC().Before(auth.AcquiredAt.Add(-clockSkew)) {
		return fmt.Errorf("heartbeat (%s) predates the tenure it claims to belong to (%s)",
			seat.Heartbeat.UTC().Format(time.RFC3339), auth.AcquiredAt.Format(time.RFC3339))
	}
	return nil
}

// viewFrom composes authority (journal) with liveness (validated cache).
func (s *Store) viewFrom(rep *Replay, now time.Time) View {
	auth := deriveAuthority(rep)
	v := View{SchemaVersion: SchemaVersion, Authority: auth, Since: auth.AcquiredAt}

	if auth.Vacant {
		v.Liveness = LivenessVacant
		v.Claimable = true
		return v
	}

	var seat Seat
	found, err := readJSON(s.seatPath(), &seat)
	switch {
	case err != nil:
		v.Liveness, v.LivenessReason = LivenessUnknown, "heartbeat record is unreadable: "+err.Error()
		return v
	case !found:
		v.Liveness, v.LivenessReason = LivenessUnknown, "no heartbeat record exists"
		return v
	}
	if err := validateSeatCache(seat, auth, now); err != nil {
		v.Liveness, v.LivenessReason = LivenessUnknown, err.Error()
		return v
	}

	v.Heartbeat = seat.Heartbeat.UTC()
	v.PID = seat.PID
	v.Intent = seat.Intent
	if now.Sub(v.Heartbeat) < TTL {
		v.Liveness = LivenessLive
	} else {
		v.Liveness = LivenessLapsed
		v.Claimable = true // a trustworthy record that says "late" — the one ordinary recovery
	}
	return v
}

// ─── seat lifecycle ───────────────────────────────────────────────────────────

// Claim acquires a VACANT or LAPSED seat, atomically, UNDER AN AUTHORIZATION CAPABILITY.
//
// It reads the journal, decides, and writes — all under one lock, so two agents racing
// for an empty seat cannot both win.
//
// It takes the seat in exactly two situations, and NOTHING else:
//
//	VACANT  — the journal says nobody holds it.
//	LAPSED  — a heartbeat record that AGREES with the journal (right holder, right
//	          epoch, sane timestamps) says the holder is past the TTL.
//
// Every other state fails closed. A LIVE seat is ErrHeld — including one held by the
// caller itself, see below. A seat whose liveness is UNKNOWN (no heartbeat file, corrupt
// file, wrong schema, wrong holder, wrong epoch, a timestamp from the future) is
// ErrLivenessUnknown: that is a fact about the RECORD, not the holder, and every way of
// producing it is also a way of producing it deliberately. Recovering from either is a
// Takeover.
//
// TWO THINGS CHANGED HERE, and both were holes rather than refinements.
//
// IT IS AUTHORIZED. Claiming used to be free — any process that could see a vacant or
// lapsed seat became the steward by asking. But a LAPSED seat has an incumbent, and
// "lapsed" proves a heartbeat gap and nothing else: the holder may be mid-thought,
// rate-limited, or paused at a human prompt. An unattended agent that could claim a
// lapsed seat could simply WAIT OUT the TTL and depose a working steward — the takeover
// it was forbidden to perform, spelled differently and with no record of authorization.
// And a vacant seat is still the seat of authority for the whole host; "whoever gets
// there first" is a race, not an access-control policy. So both go through a Grant, and
// through the injected verifier that grant is bounded by (see verifier.go).
//
// THERE IS NO SAME-HOLDER SHORTCUT. Claiming a seat you already hold and are live in
// used to be quietly treated as a heartbeat — no epoch presented, no journal entry, seat
// refreshed. That is a way to REFRESH A HELD TENURE WITHOUT PRESENTING THE EPOCH, and
// the epoch is the one thing that tells a steward its tenure ended. The agent that most
// wants that shortcut is precisely the one that must not have it: a zombie, whose
// identity still matches the journal's holder because it IS the holder of a tenure it
// does not know is over — one that lapsed and was reclaimed by... itself, restarted. A
// live holder renews through Heartbeat(holder, epoch), which presents the token and is
// fenced like everything else. Being yourself is not a credential; the token is.
func (s *Store) Claim(ctx context.Context, holder principal.Ref, req SeatRequest, now time.Time) (View, error) {
	now = mustUTC(now)
	// FAIL CLOSED FIRST. With no root of trust there is nothing that could authorize this,
	// and a store that cannot grant authority should say so plainly rather than walking the
	// caller through the paperwork before refusing on principle.
	if s.verifier == nil {
		return View{}, &ErrNoVerifier{Action: ActionClaim}
	}
	var out View
	err := s.withLock(func() error {
		rep, err := s.Replay()
		if err != nil {
			return err
		}
		if rep.Corrupt {
			return &ErrCorruptTail{Line: rep.CorruptLine, Reason: rep.CorruptReason, Kind: rep.CorruptKind, ValidEntries: len(rep.Entries)}
		}
		v := s.viewFrom(rep, now)

		switch v.Liveness {
		case LivenessLive:
			// Including when the live holder IS the caller. See the doc comment: renewing a
			// held seat is Heartbeat's job, because Heartbeat presents the epoch.
			return &ErrHeld{View: v, Yours: SameHolder(v.Authority.Holder, holder)}
		case LivenessUnknown:
			return &ErrLivenessUnknown{View: v}
		case LivenessVacant, LivenessLapsed:
			// the two ordinary paths — and both are authorized
		default:
			return &ErrLivenessUnknown{View: v}
		}

		prev := v.Authority
		grant, err := s.loadGrantFor(ActionClaim, req)
		if err != nil {
			return err
		}
		at, err := s.verifyGrant(ctx, rep, grant, ActionClaim, holder, prev, req, now)
		if err != nil {
			return err
		}

		epoch := rep.MaxEpoch + 1
		authz := authzRefOf(grant, at, req)
		e := Entry{
			Actor:     holder,
			Epoch:     epoch,
			Kind:      KindSeatClaimed,
			Summary:   fmt.Sprintf("%s claimed the steward seat at epoch %d under grant %s (%s, actor %q)", holderName(holder), epoch, grant.ID, grant.Provenance, grant.Actor),
			Rationale: req.Intent,
			Outcome:   OutcomeSuccess,
			Authz:     authz,
			Evidence: []Evidence{
				{Kind: "grant", Ref: grant.ID, Note: "authorization"},
				{Kind: "seat", Ref: fmt.Sprintf("epoch:%d", epoch), Note: "acquisition"},
			},
		}
		if req.Intent == "" {
			e.Rationale = grant.Reason
		}
		if grant.Receipt != nil {
			e.Evidence = append(e.Evidence, Evidence{
				Kind: "digest", Ref: grant.Receipt.Path, Digest: grant.Receipt.Digest, Note: "authorization-receipt",
			})
		}
		if !prev.Vacant {
			// Record whom this claim fenced. The prior holder never agreed to this and was
			// never asked — the record says so plainly.
			e.Evidence = append(e.Evidence, Evidence{
				Kind: "principal", Ref: holderName(prev.Holder), Note: "prior-holder",
			})
			e.Summary += fmt.Sprintf(" (lapsed seat previously held by %s at epoch %d)",
				holderName(prev.Holder), prev.Epoch)
		}
		stored, err := appendEntry(s.journalPath(), rep, e, now)
		if err != nil {
			return err
		}
		s.markGrantConsumed(grant, epoch, now)

		auth := Authority{Holder: holder, Epoch: epoch, AcquiredAt: now, Authz: authz}
		if !prev.Vacant {
			p := prev.Holder
			auth.TakenOverFrom = &p
		}
		out = View{
			SchemaVersion: SchemaVersion, Authority: auth,
			Liveness: LivenessLive, Heartbeat: now, Since: now, PID: os.Getpid(), Intent: req.Intent,
		}
		// The claim is COMMITTED from here on. A failure to write the derived liveness
		// cache does not un-claim it, and must not be reported as if it had — see
		// ErrCommitted.
		return committed("claim", stored.Seq, epoch, s.writeSeat(auth, req.Intent, now))
	})
	return out, err
}

// authzRefOf records, in the journal, what a seat acquisition was authorized by — the
// capability's bounds AND the attestation that actually established it.
func authzRefOf(g Grant, at *Attestation, req SeatRequest) *AuthzRef {
	ref := &AuthzRef{
		GrantID:    g.ID,
		Action:     g.Action,
		Provenance: g.Provenance,
		Actor:      g.Actor,
		FromEpoch:  g.FromEpoch,
		IssuedAt:   g.IssuedAt.UTC().Format(time.RFC3339Nano),
		ExpiresAt:  g.ExpiresAt.UTC().Format(time.RFC3339Nano),
		Attended:   req.Attended,
		Receipt:    g.Receipt,
	}
	if at != nil {
		ref.Attestation = at
	}
	return ref
}

// Takeover seizes the seat — live, lapsed, or unreadable — under a durable
// authorization capability, bumping the epoch so the prior holder is fenced from
// that instant.
//
// This is the RECOVERY path, and it is deliberately the loud one. It does not ask
// the incumbent (an incumbent that could be asked would not need taking over), it
// does not wait for a lease to expire, and it records the capability it was
// performed under — grant id, provenance, actor, expiry, receipt — in the journal
// forever, because an unexplained seizure of authority is indistinguishable from a
// hijack.
//
// The capability is a Grant (see authz.go), and consuming it is single-use: the
// takeover entry names the grant id, and replay refuses any later takeover naming
// the same id. Read Grant's doc for exactly what a grant does and does not prove —
// the short version is that it is durable, replay-protected and auditable, and it is
// NOT cryptographic evidence that a human was in the room.
func (s *Store) Takeover(ctx context.Context, holder principal.Ref, req SeatRequest, now time.Time) (View, error) {
	now = mustUTC(now)
	if s.verifier == nil {
		return View{}, &ErrNoVerifier{Action: ActionTakeover}
	}
	var out View
	err := s.withLock(func() error {
		rep, err := s.Replay()
		if err != nil {
			return err
		}
		// A takeover on a damaged journal would fork the chain. Repair first — which
		// itself requires the seat, so a stranger cannot use damage as a way in.
		if rep.Corrupt {
			return &ErrCorruptTail{Line: rep.CorruptLine, Reason: rep.CorruptReason, Kind: rep.CorruptKind, ValidEntries: len(rep.Entries)}
		}
		prev := deriveAuthority(rep)

		grant, err := s.loadGrantFor(ActionTakeover, req)
		if err != nil {
			return err
		}
		at, err := s.verifyGrant(ctx, rep, grant, ActionTakeover, holder, prev, req, now)
		if err != nil {
			return err
		}

		epoch := rep.MaxEpoch + 1
		authz := authzRefOf(grant, at, req)
		e := Entry{
			Actor:     holder,
			Epoch:     epoch,
			Kind:      KindSeatTakeover,
			Summary:   fmt.Sprintf("%s took over the steward seat at epoch %d under grant %s (%s, actor %q)", holderName(holder), epoch, grant.ID, grant.Provenance, grant.Actor),
			Rationale: grant.Reason,
			Outcome:   OutcomeSuccess,
			Authz:     authz,
			Evidence: []Evidence{
				{Kind: "grant", Ref: grant.ID, Note: "authorization"},
				{Kind: "seat", Ref: fmt.Sprintf("epoch:%d", epoch), Note: "takeover"},
			},
		}
		if grant.Receipt != nil {
			e.Evidence = append(e.Evidence, Evidence{
				Kind: "digest", Ref: grant.Receipt.Path, Digest: grant.Receipt.Digest, Note: "authorization-receipt",
			})
		}
		if !prev.Vacant {
			e.Evidence = append(e.Evidence, Evidence{
				Kind: "principal", Ref: holderName(prev.Holder), Note: "prior-holder",
			})
			e.Summary += fmt.Sprintf(", fencing %s (epoch %d)", holderName(prev.Holder), prev.Epoch)
		}
		stored, err := appendEntry(s.journalPath(), rep, e, now)
		if err != nil {
			return err
		}
		// The journal now records the grant as consumed; the file is only a cache of
		// it, so failing to mark it is not worth failing a completed takeover.
		s.markGrantConsumed(grant, epoch, now)

		newAuth := Authority{Holder: holder, Epoch: epoch, AcquiredAt: now, Authz: authz}
		if !prev.Vacant {
			p := prev.Holder
			newAuth.TakenOverFrom = &p
		}
		out = View{
			SchemaVersion: SchemaVersion, Authority: newAuth,
			Liveness: LivenessLive, Heartbeat: now, Since: now, PID: os.Getpid(), Intent: req.Intent,
		}
		// COMMITTED. See ErrCommitted: the seizure landed in the journal, and a failure to
		// write the derived liveness cache must not be reported as if it had not.
		return committed("takeover", stored.Seq, epoch, s.writeSeat(newAuth, req.Intent, now))
	})
	return out, err
}

// Release vacates the seat cleanly. It captures NO repository state — no diff, no
// branch, no working tree. A steward hands over a MANDATE, not a checkout; work in
// flight travels by `bashy handoff`, which is a different verb precisely because it
// is a different thing.
//
// Releasing is a courtesy, not a correctness requirement: an unreleased seat still
// lapses, and the epoch still fences whoever comes back.
//
// It is fenced like every other mutation. Releasing on a stale epoch is the most
// dangerous form of the zombie problem — a fenced steward "tidying up" on its way out
// would vacate the seat of the steward that replaced it.
func (s *Store) Release(holder principal.Ref, epoch uint64, note string, now time.Time) error {
	now = mustUTC(now)
	return s.withLock(func() error {
		rep, err := s.Replay()
		if err != nil {
			return err
		}
		if !rep.Corrupt && deriveAuthority(rep).Vacant {
			return nil // releasing a vacant seat is a no-op, not an error
		}
		e := Entry{
			Actor:     holder,
			Kind:      KindSeatReleased,
			Rationale: note,
			Outcome:   OutcomeSuccess,
		}
		auth, err := authorize(rep, holder, epoch)
		if err != nil {
			return err
		}
		e.Epoch = auth.Epoch
		e.Summary = fmt.Sprintf("%s released the steward seat (epoch %d)", holderName(holder), auth.Epoch)
		e.Evidence = []Evidence{{Kind: "seat", Ref: fmt.Sprintf("epoch:%d", auth.Epoch), Note: "release"}}

		// Release is a seat event, so it cannot go through appendAuthorized (which
		// forbids them by design). The gate above is the same one, run explicitly.
		stored, err := appendEntry(s.journalPath(), rep, e, now)
		if err != nil {
			return err
		}
		// The seat file is liveness only; with the seat vacated it has nothing left to
		// say. Its removal loses no authority — that lives in the journal, which now says
		// the seat is empty regardless of what this stale cache claims. So the release IS
		// COMMITTED even if the removal fails, and reporting a bare error here would invite
		// a retry that appends a second release.
		if err := failpoint("seat.remove"); err != nil {
			return committed("release", stored.Seq, auth.Epoch, err)
		}
		if err := os.Remove(s.seatPath()); err != nil && !os.IsNotExist(err) {
			return committed("release", stored.Seq, auth.Epoch, err)
		}
		return nil
	})
}

// Heartbeat refreshes the holder's liveness. It writes no journal entry: a heartbeat
// is not history, it is a pulse, and a journal that recorded every pulse would bury
// the events that matter under them.
//
// It is FENCED like every other authoritative act. A heartbeat is a claim to be the
// live holder — the single most consequential claim in the system, since it is what
// keeps everyone else out — so a zombie must not be able to refresh a tenure that
// ended. It also happens to be the holder's way OUT of an unknown liveness: the
// journal still knows they hold the seat, so heartbeating rebuilds a valid record
// from it.
func (s *Store) Heartbeat(holder principal.Ref, epoch uint64, now time.Time) error {
	now = mustUTC(now)
	return s.withLock(func() error {
		rep, err := s.Replay()
		if err != nil {
			return err
		}
		auth, err := authorize(rep, holder, epoch)
		if err != nil {
			return err
		}
		return s.writeSeat(auth, "", now)
	})
}

// writeSeat persists the liveness record, preserving the intent and the original
// acquisition time across a refresh so "steward since 3pm" keeps meaning when the
// TENURE began rather than when the last command ran.
//
// The previous record is only consulted if it VALIDATES against the same authority.
// Carrying a field forward out of a cache we would refuse to read is how a bad cache
// launders itself into a good one.
func (s *Store) writeSeat(auth Authority, intent string, now time.Time) error {
	if err := failpoint("seat.write"); err != nil {
		return err
	}
	seat := Seat{
		SchemaVersion: SchemaVersion,
		Holder:        auth.Holder,
		Epoch:         auth.Epoch,
		AcquiredAt:    auth.AcquiredAt,
		Heartbeat:     now,
		PID:           os.Getpid(),
		Intent:        intent,
	}
	var prev Seat
	if found, err := readJSON(s.seatPath(), &prev); err == nil && found {
		if err := validateSeatCache(prev, auth, now); err == nil {
			if !prev.AcquiredAt.IsZero() {
				seat.AcquiredAt = prev.AcquiredAt
			}
			if seat.Intent == "" {
				seat.Intent = prev.Intent
			}
		}
	}
	if seat.AcquiredAt.IsZero() {
		seat.AcquiredAt = now
	}
	return writeJSONAtomic(s.seatPath(), seat)
}
