// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"context"
	"fmt"
	"time"

	"github.com/qiangli/yoke/pkg/principal"
)

// ─── the root of trust ────────────────────────────────────────────────────────
//
// THE PROBLEM THIS SOLVES, stated plainly, because the previous revision's answer was
// prose and prose does not stop anybody.
//
// Every acquisition of authority — claiming a vacant seat, claiming a lapsed one,
// seizing a live one — must be authorized. The previous revision made the seizure
// consume a Grant: a durable, single-use, expiring, epoch-bound capability, written to
// the store. It then admitted, at length and honestly, that the agent could simply
// WRITE THAT FILE ITSELF, since the package runs as the user and the grant is just JSON
// on the user's disk. The admission was correct. It was also the whole problem: a
// control an agent can satisfy by writing a file is not a control, it is a log entry.
//
// A capability rooted in store state can never be better than the store. So the root of
// trust has to live somewhere the agent's filesystem access does not reach: in the
// HOST PROCESS, as injected code.
//
// That is what Verifier is. The store will not perform an authority transition unless a
// Verifier — supplied by the embedding host at construction time, never loaded from
// disk, never named in a config file the agent can edit — is asked, and approves. With
// no Verifier, every authority transition FAILS CLOSED. Not "proceeds with a warning";
// not "records that it was unverified". Fails.
//
// WHAT THIS DOES AND DOES NOT BUY. An agent that can rewrite the binary, or inject its
// own Verifier into the host, has already won — no in-process check survives an attacker
// who controls the process. What it stops is the realistic failure, which is the whole
// class the previous design left open: an agent with ordinary FILE access to the store
// deciding, on its own initiative, to authorize itself. It cannot forge its way past an
// interface it does not implement.

// Grade is how much an attestation is actually worth. Two values, and the gap between
// them is the entire honesty of this package.
type Grade string

const (
	// GradeAudit — a human channel this process CANNOT AUTHENTICATE. The typed-PTY
	// confirmation is the archetype: somebody with a terminal typed the epoch back.
	//
	// It is AUDIT-GRADE, NOT SECURITY-GRADE, and the difference is not academic. The
	// process cannot tell a human's keystrokes from a pty an agent allocated and wrote
	// into — both are a terminal, both produce the same bytes, and there is no second
	// party to ask. What it delivers is a deliberate, attended act that lands in the
	// permanent record; it does not deliver proof that a human was in the room, and this
	// package will never say it does.
	//
	// So it is accepted ONLY for an ATTENDED transition, where a human plausibly is
	// present, and it is labelled as an assertion in the journal forever.
	GradeAudit Grade = "audit"

	// GradeVerified — established by a trusted verifier rooted OUTSIDE this store and
	// outside agent-controlled state: a signature checked against a key the agent does
	// not hold, an approval fetched from a service the agent cannot write to, a
	// confirmation carried over the host's own human channel (bashy meet, a host UI).
	//
	// This is the ONLY grade that authorizes an UNATTENDED transition, and the only one
	// this package will ever describe as verified.
	GradeVerified Grade = "verified"
)

// CapPhase says which side of a capability's life the verifier is being asked about.
type CapPhase string

const (
	// PhaseMint — a capability is being created. The verifier establishes the human (or
	// external) authority behind it.
	PhaseMint CapPhase = "mint"
	// PhaseConsume — a capability is being SPENT on an actual authority transition. The
	// verifier is asked AGAIN, and its approval here is what the transition rests on.
	//
	// Asking twice is deliberate. A mint-time attestation is a record in the store, and
	// a record in the store is precisely what an agent can fabricate; re-asking the
	// injected verifier at the moment of the transition is what makes the check
	// unforgeable-from-disk. A verifier that trusts its own mint-time attestation may of
	// course just return it — but that has to be ITS decision, made in code the host
	// wrote, not an assumption this package makes on its behalf.
	PhaseConsume CapPhase = "consume"
)

// Capability is the authority transition a Verifier is asked to approve. Everything the
// decision could reasonably turn on is here, so a verifier never has to go and read the
// store — which is the state it exists to be independent of.
type Capability struct {
	Phase  CapPhase `json:"phase"`
	Action string   `json:"action"` // ActionClaim | ActionTakeover

	Scope   string        `json:"scope"`   // the seat: one machine, one account
	Grantee principal.Ref `json:"grantee"` // the agent that will hold the seat
	Actor   string        `json:"actor"`   // the operator identity ASSERTED at mint time
	Reason  string        `json:"reason,omitempty"`

	// FromEpoch is what the capability authorizes acting on: the epoch being SEIZED for
	// a takeover, or the vacant/lapsed watermark being claimed from. Either way it pins
	// the capability to the situation it was minted for.
	FromEpoch uint64 `json:"from_epoch"`

	Nonce     string    `json:"nonce"`
	IssuedAt  time.Time `json:"issued_at,omitzero"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`

	// Receipt is an out-of-band artifact pinned by digest. A DIGEST IS INTEGRITY, NOT
	// AUTHENTICITY: it proves the bytes did not change, and says nothing whatever about
	// who issued them — an agent can write a file, hash it, and call it an approval. A
	// verifier that can check the issuer (a signature, an API call to the source) is what
	// turns it into evidence; this package will not promote it on its own.
	Receipt *ExternalReceipt `json:"receipt,omitempty"`

	// Attended is the host's OBSERVATION that a terminal is attached. Never a credential
	// — a caller can set it — which is exactly why an unattended transition may not be
	// authorized by an audit-grade attestation and an attended one may.
	Attended bool `json:"attended"`

	// Prior is the attestation recorded when the capability was minted. Nil at PhaseMint.
	// It comes OUT OF THE STORE, so a verifier must treat it as a hint, never as proof.
	Prior *Attestation `json:"prior,omitempty"`

	// Seat is the seat as it stands right now, for a human channel to display: who holds
	// it, whether they are alive, what they said they were doing.
	Seat View `json:"seat"`
}

// Attestation is a verifier's verdict, recorded in the grant and in the journal forever.
type Attestation struct {
	Verifier string    `json:"verifier"`
	Channel  string    `json:"channel"` // how the authority was established: "pty", "meet", "sigstore", …
	Grade    Grade     `json:"grade"`
	Approved bool      `json:"approved"`
	Why      string    `json:"why,omitempty"`
	At       time.Time `json:"at,omitzero"`
	// Binding is a digest of whatever the verifier checked (a signed payload, an approval
	// body), so a later auditor can go and re-check the same bytes.
	Binding string `json:"binding,omitempty"`
}

// Verifier is the injected root of trust. See the block comment above for why it must
// be injected and cannot be configured.
//
// bashy's integration hook: a host with a human channel (bashy meet, a desktop
// confirmation, an approval service) implements this and passes WithVerifier at Open.
// Its VerifyCapability puts the transition in front of the human on THAT channel — one
// the agent has no way to write into — and returns GradeVerified. That is the upgrade
// path from the audit-grade PTY confirmation the CLI ships with, and nothing else in
// this package has to change for it: the enforcement point already exists and already
// fails closed.
type Verifier interface {
	// Name identifies the verifier in the record.
	Name() string
	// VerifyCapability establishes — or refuses — the authority behind a capability.
	//
	// Returning an error means the verifier could not establish it. Returning an
	// Attestation with Approved=false means it established that the answer is NO. Both
	// refuse the transition; the difference is only what the record says.
	VerifyCapability(ctx context.Context, c Capability) (Attestation, error)
}

// ErrNoVerifier is what an authority transition returns when no root of trust was
// injected. FAIL CLOSED IS THE FEATURE.
//
// The alternative — proceed, and record that nothing verified it — is what the previous
// revision did, and it produced a system that described itself as authorized while
// authorizing nothing. A store with no verifier can still be READ (status, board, log,
// reconcile all work); it simply cannot hand anybody authority.
type ErrNoVerifier struct{ Action string }

func (e *ErrNoVerifier) Error() string {
	return fmt.Sprintf("steward: %s needs an authorization capability, and this store has NO TRUSTED VERIFIER, "+
		"so there is nothing that could establish one. Refusing.\n"+
		"Acquiring the seat is an authority transition, and authority cannot be granted by the same filesystem the "+
		"requester writes to: a capability rooted in store state is one the agent can mint for itself by writing a "+
		"file. The root of trust must be injected by the host (steward.WithVerifier) — the CLI wires a typed-terminal "+
		"confirmation, which is AUDIT-grade and works only when a terminal is actually attached; an unattended "+
		"transition needs a host verifier that can establish authority outside this store.", e.Action)
}

// ErrUnverified is returned when a verifier ran and did not establish the authority.
type ErrUnverified struct {
	Action string
	Why    string
	Att    *Attestation
}

func (e *ErrUnverified) Error() string {
	s := fmt.Sprintf("steward: %s refused — %s", e.Action, e.Why)
	if e.Att != nil && e.Att.Verifier != "" {
		s += fmt.Sprintf(" (verifier %q, channel %q, grade %s)", e.Att.Verifier, e.Att.Channel, e.Att.Grade)
	}
	return s
}

// verifyCapability is THE enforcement point. Every authority transition in this package
// goes through it, and there is no side door.
//
// Two gates, in order:
//
//  1. A VERIFIER MUST EXIST. No verifier, no authority. See ErrNoVerifier.
//  2. IT MUST APPROVE, AT A GRADE THE CIRCUMSTANCES ALLOW. An unattended transition —
//     a cron job, a CI runner, a headless agent loop, a runaway subagent — requires
//     GradeVerified: something rooted outside this store established the authority. An
//     audit-grade attestation is accepted only where a human plausibly is present, and
//     is recorded as the assertion it is.
//
// Gate 2 is the one that makes the unattended path honest. An agent with no terminal
// cannot produce a typed confirmation, and a receipt it wrote itself hashes exactly as
// well as one a human wrote — so if audit-grade were enough when unattended, the entire
// control would reduce to "the agent must write a file first".
func (s *Store) verifyCapability(ctx context.Context, c Capability) (Attestation, error) {
	if s.verifier == nil {
		return Attestation{}, &ErrNoVerifier{Action: c.Action}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	at, err := s.verifier.VerifyCapability(ctx, c)
	if err != nil {
		return Attestation{}, &ErrUnverified{Action: c.Action, Why: err.Error(), Att: &at}
	}
	if at.Verifier == "" {
		at.Verifier = s.verifier.Name()
	}
	if at.At.IsZero() {
		at.At = time.Now().UTC()
	}
	at.At = at.At.UTC()

	if !at.Approved {
		why := at.Why
		if why == "" {
			why = "the verifier did not approve it"
		}
		return at, &ErrUnverified{Action: c.Action, Why: why, Att: &at}
	}
	switch at.Grade {
	case GradeVerified:
		// Established outside this store. Good anywhere.
	case GradeAudit:
		if !c.Attended {
			return at, &ErrUnverified{
				Action: c.Action,
				Why: "this is an UNATTENDED authority transition and the only attestation is AUDIT-grade. " +
					"An audit-grade attestation is a human-channel act this process cannot authenticate; with no human " +
					"present it attests to nothing at all. An unattended transition needs a verifier that can establish " +
					"authority outside this store (steward.WithVerifier) — a signature it can check, a service it can " +
					"ask, a human channel the agent cannot write into",
				Att: &at,
			}
		}
	default:
		return at, &ErrUnverified{Action: c.Action, Why: fmt.Sprintf("the attestation has an unknown grade %q", at.Grade), Att: &at}
	}
	return at, nil
}
