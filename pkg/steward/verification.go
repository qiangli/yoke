// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"context"
	"fmt"
	"time"

	"github.com/qiangli/yoke/pkg/principal"
)

// ─── the root of trust for PROMOTION ──────────────────────────────────────────
//
// verifier.go roots the acquisition of AUTHORITY outside the store, and explains at
// length why a control the agent can satisfy by writing a file is not a control. This
// file applies exactly the same argument to the other thing in this package that grants
// confidence rather than taking it away: PROMOTION of a claim to verified on the board.
//
// The previous revision left that promotion caller-forgeable, in two ways, and both were
// the same mistake wearing different clothes:
//
//  1. Verification.Adapter was a PUBLIC *Attestation — a plain struct with an Approved
//     bool and a Grade string. "A trusted verification adapter's attestation", said the
//     comment. Nothing made it one. Any caller could write
//
//	Verification{Adapter: &Attestation{Approved: true, Grade: GradeVerified}}
//
//     and the board promoted the strand to VERIFIED. The struct was named after a trust
//     relationship it did not have; the agent supplied both the claim and the credential
//     that vouched for it.
//
//  2. DIGEST-BOUND EVIDENCE promoted on its own. A digest proves INTEGRITY — these bytes
//     did not change — and says nothing whatever about whether a check ran. An agent
//     writes any file it likes, hashes it, attaches `file:/tmp/pass.log#sha256:…`, and the
//     row goes green. It need not even write the file: nothing rehashes it at promotion
//     time, so thirty-two arbitrary bytes typed at the prompt did just as well.
//
// Both are the trust-me claim the package exists to refuse, laundered one entry down the
// log. So promotion now rests where authority already does: on a VerificationVerifier the
// HOST injects, which the agent cannot implement, name in a config file, or write to disk.
//
// AND IT IS ASKED TWICE, for the same reason PhaseConsume exists. Once at Attest, to mint
// the Seal; once again at PROJECTION, to re-check the Seal that came back off disk. The
// second ask is the load-bearing one: the journal is a file on the agent's filesystem, so
// a Seal read out of it is just bytes the agent could have typed. Only the verifier can
// say whether the Seal is one it actually issued — and a verifier that cannot say so
// (Token empty, RecheckSeal false) promotes nothing.
//
// WHAT THIS BUYS, PRECISELY. Not proof against an agent that controls the process — no
// in-process check survives that, and verifier.go says so. What it stops is the entire
// realistic class: an agent with ordinary FILE access to the store deciding, on its own
// initiative, that its own work is verified. It cannot forge its way past an interface it
// does not implement.

// VerificationClaim is what a VerificationVerifier is asked to establish: somebody says
// the entry at TargetSeq came true, and this is everything a verifier needs to go and
// find out — WITHOUT reading the store, which is the state it exists to be independent of.
//
// It is rebuilt DETERMINISTICALLY from the journal at projection time (see claimOf), so
// the claim a Seal is re-checked against is the same claim it was minted for. Every field
// is journal-derived; nothing here comes from ambient process state, or the re-check
// could pass on one host and fail on another for reasons neither could see.
type VerificationClaim struct {
	Workstream string        `json:"workstream,omitempty"`
	Actor      principal.Ref `json:"actor"`
	Epoch      uint64        `json:"epoch"`

	// Target is the entry whose claim is under check, in full. The verifier gets the
	// bytes, not a pointer to them, and TargetHash pins which bytes those are.
	Target     Entry   `json:"target"`
	TargetSeq  uint64  `json:"target_seq"`
	TargetHash string  `json:"target_hash"`
	Result     Outcome `json:"result"`

	// Method and Observer are PROSE, carried for the verifier's benefit and decisive of
	// nothing. Evidence is the caller's references — likewise a hint, never a credential.
	Method   string     `json:"method,omitempty"`
	Observer string     `json:"observer,omitempty"`
	Evidence []Evidence `json:"evidence,omitempty"`
}

// Seal is a trusted verifier's verdict on a VerificationClaim: the ONLY thing in this
// package that can promote a strand to ConfidenceVerified.
//
// READ THE FIELDS IN THE RIGHT ORDER, because the obvious reading is the wrong one.
// Approved and Grade are DESCRIPTIVE — they are what a human reads in the log, and a
// forged Seal can set them to anything it likes. They decide nothing on their own.
//
// TOKEN IS THE WHOLE SEAL. It is OPAQUE to this package: a signature, an approval id, an
// HMAC over the canonical claim — whatever the host's verifier can later recognize as its
// own and nobody else can produce. Promotion asks the verifier to RE-CHECK it
// (RecheckSeal) against the claim rebuilt from the journal, so a Seal that was typed into
// the file by hand, or lifted off a different claim, fails there no matter how green its
// Approved bool looks.
//
// A Store MINTS these and never accepts one: Attest refuses a Verification that arrives
// with a Seal already set (see ErrSealSupplied). There is no path by which a caller's
// bytes become a Seal.
type Seal struct {
	Verifier string    `json:"verifier"`
	Grade    Grade     `json:"grade"`
	Approved bool      `json:"approved"`
	Why      string    `json:"why,omitempty"`
	At       time.Time `json:"at,omitzero"`

	// Binding is a digest of whatever the verifier actually looked at, so an auditor can
	// go and re-check the same bytes. Integrity, not authenticity — see Token.
	Binding string `json:"binding,omitempty"`

	// Token is the verifier's own proof that this Seal is its own. Opaque here, decisive
	// there. A Seal with no Token promotes nothing, because there is nothing to re-check.
	Token string `json:"token,omitempty"`
}

// VerificationVerifier is the injected root of trust for PROMOTION — the counterpart of
// Verifier (verifier.go), which roots the acquisition of AUTHORITY.
//
// A host that can actually establish whether work came true implements this: a CI adapter
// that asks the CI system, a git adapter that looks at the commit, a signing service the
// agent holds no key for. It is passed at Open (WithVerificationVerifier) and never loaded
// from disk, never named in a config file the agent can edit.
//
// WITHOUT ONE, NOTHING IS EVER PROMOTED. Verifications are still recorded — the log keeps
// its full value, and a human can still go and rehash the evidence — but the board leaves
// the strand at ConfidenceAsserted, which is exactly what it is. That is the fail-closed
// default, and it is the honest one: a store with no way to check a claim should not be
// producing green rows.
type VerificationVerifier interface {
	// Name identifies the verifier in the record.
	Name() string

	// VerifyClaim goes and establishes — or refuses — that the claim actually came true,
	// and returns the Seal that vouches for it.
	//
	// Returning an error means the verifier COULD NOT ESTABLISH the claim: it does not know,
	// the CI system was unreachable, this is not a claim it can speak to. The verification
	// is still recorded, UNSEALED, and promotes nothing — a store with no verifier at all
	// behaves the same way, so this is not a new hole, it is the existing floor.
	//
	// Returning a Seal with Approved=false means the verifier ESTABLISHED THAT THE ANSWER
	// IS NO. That is a refutation, and Attest refuses to record it as a success: a claim
	// the trusted verifier actively refuted must not enter the log wearing a success label
	// (record it with --result failed, which needs no credential at all).
	VerifyClaim(ctx context.Context, c VerificationClaim) (Seal, error)

	// RecheckSeal re-validates a Seal that came back OFF DISK against the claim rebuilt
	// from the journal. This is the enforcement point the projection uses, and it is what
	// makes promotion unforgeable-from-the-filesystem.
	//
	// It must return false for anything it did not itself issue, and for a Seal issued for
	// a DIFFERENT claim — a token lifted off one verification and pasted onto another is
	// the obvious attack, and binding the token to the claim (an HMAC over its canonical
	// form, an id looked up server-side) is what defeats it.
	//
	// It takes no context and must not block: projections are pure, cheap, and called from
	// read paths that hold no lock. A verifier that needs to ask a network service does
	// that at VerifyClaim time and records the answer in the Token.
	RecheckSeal(c VerificationClaim, s Seal) bool
}

// SealChecker is RecheckSeal, decoupled from the interface so a projection — which is a
// pure function of the journal, and takes no Store — can be handed the check without
// being handed the store.
//
// A nil SealChecker promotes NOTHING. That is the whole default: ProjectBoard(entries,
// nil) is the board of a host with no trusted verifier, and it is honest about it.
type SealChecker func(c VerificationClaim, s Seal) bool

// sealChecker is the store's checker, or nil when no verifier was injected.
func (s *Store) sealChecker() SealChecker {
	if s == nil || s.vverifier == nil {
		return nil
	}
	return s.vverifier.RecheckSeal
}

// HasVerificationVerifier reports whether this store can promote anything at all.
func (s *Store) HasVerificationVerifier() bool { return s != nil && s.vverifier != nil }

// claimOf rebuilds the claim a verification entry stands for, from the journal alone.
//
// DETERMINISM IS THE CONTRACT. This must produce byte-identical claims at mint time (from
// the entry about to be appended) and at projection time (from the entry read back), or a
// Seal would validate exactly once and never again. So it reads only fields that are set
// BEFORE the append — never e.Seq or e.Hash, which the append assigns — and takes the
// epoch explicitly, because e.Epoch is stamped by appendAuthorized and is still zero when
// the Seal is minted.
func claimOf(e Entry, epoch uint64, target Entry) VerificationClaim {
	c := VerificationClaim{
		Workstream: e.Workstream,
		Actor:      e.Actor,
		Epoch:      epoch,
		Target:     target,
		Evidence:   e.Evidence,
	}
	if v := e.Verifies; v != nil {
		c.TargetSeq = v.TargetSeq
		c.TargetHash = v.TargetHash
		c.Result = v.Result
		c.Method = v.Method
		c.Observer = v.Observer
	}
	return c
}

// sealPromotes is THE promotion gate, and there is no side door.
//
// Four conditions, and the last is the only one an agent cannot satisfy by writing bytes:
//
//  1. there is a Seal at all;
//  2. it is approved, at GradeVerified — descriptive fields, trivially forgeable, checked
//     first only because they are cheap;
//  3. a trusted verifier is present to be asked (nil checker → no promotion, ever);
//  4. THAT VERIFIER RECOGNIZES THE SEAL as one it issued FOR THIS CLAIM.
//
// Both Attest and ProjectBoard call this, on purpose. Attest is where the Seal is minted,
// so the check there is nearly free; the board is a projection of the JOURNAL, and a
// projection must be able to grade a record it did not write — including one that appeared
// in the file without ever going through Attest at all, which is precisely the case this
// exists for.
func sealPromotes(e Entry, target Entry, sc SealChecker) bool {
	v := e.Verifies
	if v == nil || v.Seal == nil || sc == nil {
		return false
	}
	seal := *v.Seal
	if !seal.Approved || seal.Grade != GradeVerified || seal.Token == "" {
		return false
	}
	return sc(claimOf(e, e.Epoch, target), seal)
}

// ErrSealSupplied is returned when a caller hands Attest a Verification that already
// carries a Seal.
//
// A Seal is MINTED by the store from a trusted verifier's answer. A caller that supplies
// one is doing the exact thing this whole file exists to stop — writing its own
// credential — so it is refused loudly rather than quietly overwritten, because a caller
// that tried this is a caller whose next line deserves to be read.
type ErrSealSupplied struct{ TargetSeq uint64 }

func (e *ErrSealSupplied) Error() string {
	return fmt.Sprintf("steward: refusing a verification of seq %d that arrived WITH ITS OWN SEAL.\n"+
		"A seal is not something a caller supplies — it is minted here, from the answer of a verification verifier the "+
		"HOST injected (steward.WithVerificationVerifier), and it is re-checked against that same verifier when the board "+
		"is projected. Supplying one is supplying your own credential for your own claim, which is the trust-me claim a "+
		"verification is supposed to replace.\n"+
		"Record the verification without a seal: it lands in the journal in full, and the board reports it as asserted — "+
		"which is what an unverified claim is.", e.TargetSeq)
}

// ErrRefuted is returned when the trusted verifier went and looked, and the claim is FALSE.
//
// This is not "could not establish" (which records unsealed and promotes nothing) — it is
// the verifier saying NO. Recording that as a success verification would put a refuted
// claim into the log wearing a success label, and the log is the one thing here that has
// to be true.
type ErrRefuted struct {
	TargetSeq uint64
	Seal      Seal
}

func (e *ErrRefuted) Error() string {
	why := e.Seal.Why
	if why == "" {
		why = "it did not say why"
	}
	return fmt.Sprintf("steward: refusing to record seq %d as VERIFIED SUCCESSFUL — the trusted verifier %q went and "+
		"looked, and it says the claim is FALSE: %s.\n"+
		"This is a refutation, not a failure to establish. Recording it as a success would put a claim the one trusted "+
		"party in this system actively refuted into the permanent record with a success label on it.\n"+
		"If the check really did fail, say so: `steward verify --seq %d --result failed --method …`, which needs no "+
		"credential at all. Doubt is free; confidence is not.",
		e.TargetSeq, e.Seal.Verifier, why, e.TargetSeq)
}

// mintSeal asks the injected verifier to establish a success claim, and returns the Seal
// it vouched with — or nil, when nothing trusted could speak to it.
//
// The nil-Seal cases are deliberate and are NOT holes:
//
//	no verifier      the store cannot promote anything (fail-closed default). Recorded,
//	                 unsealed, asserted.
//	verifier errored it could not establish the claim. Same outcome as having no verifier
//	                 at all — which is the floor, not a regression.
//	seal not usable  the verifier approved but returned something the projection could not
//	                 re-check (no token, wrong grade, or a token IT ITSELF does not
//	                 recognize). A seal that cannot survive the read path must not survive
//	                 the write path either, or the board and the journal would disagree.
//
// The refusal case (Approved=false) is the one that errors: see ErrRefuted.
func (s *Store) mintSeal(ctx context.Context, e Entry, epoch uint64, target Entry) (*Seal, error) {
	if s.vverifier == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c := claimOf(e, epoch, target)

	seal, err := s.vverifier.VerifyClaim(ctx, c)
	if err != nil {
		// Could not establish it. Record the check, promote nothing.
		return nil, nil
	}
	if !seal.Approved {
		if seal.Verifier == "" {
			seal.Verifier = s.vverifier.Name()
		}
		return nil, &ErrRefuted{TargetSeq: c.TargetSeq, Seal: seal}
	}
	if seal.Verifier == "" {
		seal.Verifier = s.vverifier.Name()
	}
	if seal.At.IsZero() {
		seal.At = time.Now().UTC()
	}
	seal.At = seal.At.UTC()

	// A seal the projection will refuse is a seal that must not be written. Otherwise the
	// journal would carry a green-looking credential that the board silently ignores, and
	// "why does this say verified in the log and asserted on the board" is a question
	// nobody should ever have to ask.
	if seal.Grade != GradeVerified || seal.Token == "" || !s.vverifier.RecheckSeal(c, seal) {
		return nil, nil
	}
	return &seal, nil
}
