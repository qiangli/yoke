// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/principal"
)

// DefaultGrantTTL is how long an authorization stays usable. Short on purpose: a
// capability to seize the host's seat is not a thing to leave lying around, and the
// human who minted it is, by construction, right there.
const DefaultGrantTTL = 15 * time.Minute

// MaxGrantTTL caps --ttl. A capability that outlives the situation that justified it
// is a backdoor with a nice name.
const MaxGrantTTL = 24 * time.Hour

// Provenance says WHAT ARTIFACT was supplied with an authorization. It is a label on
// the paperwork, and — this is the correction the previous revision needed — it is NOT
// what makes the authorization valid. That is the Attestation, which comes from an
// injected Verifier (see verifier.go). Provenance describes; Attestation enforces.
type Provenance string

const (
	// ProvenanceOperatorAssertion — somebody ran `steward authorize` at a terminal and
	// asserted, by typing, that they are the named actor.
	//
	// BE CLEAR ABOUT WHAT THIS IS NOT. It is not proof that a human was present, and no
	// amount of care in this package can make it one: the process runs as the user, and
	// a pty an agent allocated produces exactly the bytes a human's keyboard does. It is
	// AUDIT-GRADE — a deliberate, attended act that lands in the permanent record — and
	// the CLI's terminal confirmation attests to it at exactly that grade (GradeAudit),
	// which is accepted only for an ATTENDED transition and never for an unattended one.
	ProvenanceOperatorAssertion Provenance = "operator-assertion"

	// ProvenanceExternalReceipt — an artifact produced outside this process was supplied:
	// an approval comment, a signed ticket, a change record, a pager acknowledgement. The
	// bytes are copied into the store and pinned by digest.
	//
	// A DIGEST IS INTEGRITY, NOT AUTHENTICITY. It proves the bytes did not change since
	// they were stored. It proves NOTHING about who wrote them — an agent can write a
	// file, hash it, and hand it over as an approval, and the digest will verify
	// perfectly. The previous revision leaned on a receipt as if the hash authenticated
	// the issuer; it does not, and treating it that way made the unattended path — the
	// one that most needs a control — the easiest to satisfy.
	//
	// A receipt becomes evidence only when a Verifier that can check its ISSUER (a
	// signature against a key the agent does not hold, a call to the service that issued
	// it) says so, at GradeVerified. On its own it is a pinned blob and is recorded as
	// exactly that.
	ProvenanceExternalReceipt Provenance = "external-receipt"
)

// ExternalReceipt pins an out-of-band approval artifact to exact bytes.
//
// Issuer is FREE TEXT and is not authenticated by this package — see
// ProvenanceExternalReceipt. The digest binds the bytes; nothing here binds the author.
type ExternalReceipt struct {
	Issuer string `json:"issuer"`       // who issued it out of band — free text, NOT authenticated
	ID     string `json:"id,omitempty"` // its identifier over there (a PR number, a ticket)
	Path   string `json:"path"`         // store-relative copy of the bytes
	Digest string `json:"digest"`       // sha256 of those bytes — INTEGRITY only
	Bytes  int64  `json:"bytes,omitempty"`
}

// Grant is a durable, single-use capability to ACQUIRE THE SEAT — by claiming it or by
// seizing it.
//
// Every field is a lock on how far it reaches:
//
//	ID          a 128-bit nonce. Consumption is recorded in the JOURNAL (the seat entry
//	            that spends it names it), so replay refuses a second use even if the
//	            grant file is restored from a backup. The journal is the authority here
//	            too.
//	Action      claim or takeover. A grant is not a general-purpose skeleton key, and a
//	            capability minted to claim a vacant seat does not authorize seizing a
//	            live one — those are different acts with different victims.
//	Grantee     WHO may use it. A grant minted for one agent is not a coupon another
//	            can pick up.
//	Scope       the machine/account seat it was minted against. A capability does not
//	            travel between seats.
//	FromEpoch   the situation it authorizes acting on: the epoch being SEIZED, or the
//	            watermark a vacant/lapsed seat is being claimed from. If the seat moves
//	            on — someone else claims, releases, takes over — the grant is dead.
//	ExpiresAt   when it stops working.
//	Attestation what actually AUTHORIZED it, from the injected Verifier. This is the
//	            enforceable field; everything above merely bounds it.
//
// What changed from the previous revision, and why: the grant used to rest on nothing
// but its own existence as a file, which an agent that wanted the seat could simply
// write. Now the grant is inert on its own — the transition that spends it re-asks the
// injected verifier (verifier.go), so the capability is a BOUND on an authority the
// verifier grants, not the grant of it.
type Grant struct {
	SchemaVersion string           `json:"schema_version"`
	ID            string           `json:"id"`
	Action        string           `json:"action"`
	Grantee       principal.Ref    `json:"grantee"`
	Scope         string           `json:"scope"`
	FromEpoch     uint64           `json:"from_epoch"`
	Actor         string           `json:"actor"`
	Provenance    Provenance       `json:"provenance"`
	Receipt       *ExternalReceipt `json:"receipt,omitempty"`
	Attestation   *Attestation     `json:"attestation,omitempty"`
	Reason        string           `json:"reason,omitempty"`
	IssuedAt      time.Time        `json:"issued_at"`
	ExpiresAt     time.Time        `json:"expires_at"`
}

// The actions a grant may authorize. Both are ACQUISITIONS OF AUTHORITY, and both are
// authorized, which is the second correction this revision makes.
//
// The previous revision gated only the takeover, reasoning that claiming a vacant or
// lapsed seat takes nothing from anybody. That is true of a VACANT seat on a fresh host
// and false everywhere else that matters:
//
//   - A LAPSED seat has an incumbent. "Lapsed" proves a heartbeat gap and nothing more
//     — the holder may be mid-thought, rate-limited, or paused at a human prompt — and
//     claiming it FENCES them. An unattended agent that could claim a lapsed seat could
//     wait thirty minutes and depose a working steward with no authorization at all,
//     which is the takeover it was not allowed to perform, spelled differently.
//   - A VACANT seat is the seat of authority for the whole host. Becoming the steward is
//     the thing that lets an agent write the host's authoritative record. "Anyone may
//     become the authority as long as they get there first" is not an access-control
//     policy, it is a race.
//
// So both are grants. They are separate actions because they are separate acts: a
// capability to claim an empty chair must not be spendable on shoving somebody out of it.
const (
	ActionClaim    = "claim"
	ActionTakeover = "takeover"
)

func validAction(a string) bool { return a == ActionClaim || a == ActionTakeover }

// GrantRequest is what a caller asks Authorize for.
type GrantRequest struct {
	// Action is claim or takeover. Required.
	Action string
	// Grantee is the agent that will use the grant. Zero value means "whoever runs the
	// next acquisition on this host" — which is refused: an unbound capability is a
	// skeleton key.
	Grantee principal.Ref
	// Actor is the operator identity being ASSERTED (a human's name, an on-call
	// handle). Required, and recorded verbatim. It is a claim, not a credential.
	Actor  string
	Reason string
	TTL    time.Duration

	// Attended is the host's OBSERVATION that a terminal is attached. It is passed to
	// the verifier, which decides what to make of it; it is never itself an
	// authorization.
	Attended bool

	// ReceiptPath / ReceiptIssuer / ReceiptID supply an external approval artifact.
	// Supplying one makes the grant an external-receipt grant — which pins bytes, and
	// authenticates nobody. See ProvenanceExternalReceipt.
	ReceiptPath   string
	ReceiptIssuer string
	ReceiptID     string
}

// SeatRequest is what an acquisition — Claim or Takeover — is handed.
type SeatRequest struct {
	// GrantID names a grant in the store; GrantPath reads one from a file (for one
	// minted elsewhere and copied in). Exactly one is required.
	GrantID   string
	GrantPath string

	// Attended is the host's OBSERVATION that a terminal is attached. An observation,
	// never a credential — a caller can set it, and it is recorded as an assertion in the
	// journal. What it buys is that an UNATTENDED acquisition cannot be authorized by an
	// audit-grade attestation: with no human present, a human-channel confirmation
	// attests to nothing, so the unattended path (a cron job, a runaway agent loop, a CI
	// runner) must produce a verifier-established authority or fail. See
	// Store.verifyCapability.
	Attended bool

	// Intent is what the holder says they are taking the seat to do. Liveness-cache only.
	Intent string
}

// ErrUnauthorized is returned when an acquisition's capability is missing, invalid,
// expired, replayed, or not strong enough for the circumstances.
type ErrUnauthorized struct {
	Action string
	Why    string
}

func (e *ErrUnauthorized) Error() string {
	act := e.Action
	if act == "" {
		act = "acquiring the seat"
	}
	return fmt.Sprintf("steward: %s refused — %s.\n"+
		"Taking the host's seat of authority is not an agent's call to make on its own: an agent that could decide to "+
		"become the steward would eventually decide to do it to a healthy one. Mint a capability first:\n"+
		"  steward authorize --action %s --actor <who> --reason <why>\n"+
		"then: steward %s --grant <id>", act, e.Why, act, act)
}

// newNonce mints a 128-bit grant id.
func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("steward: cannot mint an authorization nonce: %w", err)
	}
	return "g-" + hex.EncodeToString(b), nil
}

// Authorize mints a durable authorization capability, AFTER the injected verifier has
// established the authority behind it.
//
// It does NOT require the seat — that is the whole point. The agent that needs taking
// over is the one holding the seat, so a capability only its holder could mint would be
// useless exactly when it is needed.
//
// It DOES bind the grant to the current epoch (or, for a vacant seat, to the current
// watermark), so what it authorizes is acting on the situation as it stands right now.
//
// And it does NOT, on its own, authorize anything: the grant records the mint-time
// attestation, but the acquisition that spends it RE-ASKS the verifier. A grant is a
// bound on an authority, never the source of one — see verifier.go for why a capability
// that lives in the store can never be better than the store.
func (s *Store) Authorize(ctx context.Context, req GrantRequest, now time.Time) (Grant, error) {
	now = mustUTC(now)
	if !validAction(req.Action) {
		return Grant{}, &ErrUnauthorized{Action: req.Action,
			Why: fmt.Sprintf("an authorization is for %q or %q, not %q", ActionClaim, ActionTakeover, req.Action)}
	}
	if strings.TrimSpace(req.Actor) == "" {
		return Grant{}, &ErrUnauthorized{Action: req.Action, Why: "an authorization must name the operator asserting it (--actor)"}
	}
	if req.Grantee.Name == "" && req.Grantee.Episode == "" {
		return Grant{}, &ErrUnauthorized{Action: req.Action, Why: "an authorization must name the agent it is for (an unbound capability is a skeleton key)"}
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = DefaultGrantTTL
	}
	if ttl > MaxGrantTTL {
		return Grant{}, &ErrUnauthorized{Action: req.Action,
			Why: fmt.Sprintf("a capability to acquire the seat may not live longer than %s (asked for %s)", MaxGrantTTL, ttl)}
	}
	// FAIL CLOSED BEFORE ANYTHING ELSE. With no root of trust there is nothing that
	// could authorize this, and minting a capability we know we will refuse to honour
	// would be a lie told in advance.
	if s.verifier == nil {
		return Grant{}, &ErrNoVerifier{Action: req.Action}
	}

	var g Grant
	err := s.withLock(func() error {
		rep, err := s.Replay()
		if err != nil {
			return err
		}
		auth := deriveAuthority(rep)

		id, err := newNonce()
		if err != nil {
			return err
		}
		g = Grant{
			SchemaVersion: SchemaVersion,
			ID:            id,
			Action:        req.Action,
			Grantee:       req.Grantee,
			Scope:         s.scope.ID,
			FromEpoch:     auth.Epoch,
			Actor:         strings.TrimSpace(req.Actor),
			Provenance:    ProvenanceOperatorAssertion,
			Reason:        req.Reason,
			IssuedAt:      now,
			ExpiresAt:     now.Add(ttl),
		}
		if req.ReceiptPath != "" {
			if strings.TrimSpace(req.ReceiptIssuer) == "" {
				return &ErrUnauthorized{Action: req.Action,
					Why: "an external receipt must say who issued it (--receipt-issuer): an artifact with no stated source is not even auditable"}
			}
			rc, err := s.storeReceipt(req)
			if err != nil {
				return err
			}
			g.Provenance, g.Receipt = ProvenanceExternalReceipt, rc
		}

		// THE GATE. The verifier — not this package, not the presence of a file, not the
		// caller's say-so — establishes the authority.
		at, err := s.verifyCapability(ctx, Capability{
			Phase:     PhaseMint,
			Action:    g.Action,
			Scope:     g.Scope,
			Grantee:   g.Grantee,
			Actor:     g.Actor,
			Reason:    g.Reason,
			FromEpoch: g.FromEpoch,
			Nonce:     g.ID,
			IssuedAt:  g.IssuedAt,
			ExpiresAt: g.ExpiresAt,
			Receipt:   g.Receipt,
			Attended:  req.Attended,
			Seat:      s.viewFrom(rep, now),
		})
		if err != nil {
			return err
		}
		g.Attestation = &at

		return writeJSONAtomic(filepath.Join(s.grantDir(), g.ID+".json"), g)
	})
	if err != nil {
		return Grant{}, err
	}
	return g, nil
}

// storeReceipt copies an approval artifact into the store and pins it by digest, so
// the bytes that justified a seizure survive the seizure and cannot be swapped
// afterwards.
func (s *Store) storeReceipt(req GrantRequest) (*ExternalReceipt, error) {
	f, err := os.Open(req.ReceiptPath)
	if err != nil {
		return nil, fmt.Errorf("steward: authorization receipt: %w", err)
	}
	defer f.Close()

	b, err := io.ReadAll(io.LimitReader(f, MaxReceiptBytes+1))
	if err != nil {
		return nil, fmt.Errorf("steward: authorization receipt: %w", err)
	}
	if int64(len(b)) > MaxReceiptBytes {
		return nil, &ErrUnauthorized{Why: fmt.Sprintf("the authorization receipt exceeds %d bytes", MaxReceiptBytes)}
	}
	if len(b) == 0 {
		return nil, &ErrUnauthorized{Why: "the authorization receipt is empty — an empty artifact attests to nothing"}
	}
	dig := digestOf(b)
	name := strings.TrimPrefix(dig, "sha256:") + ".bin"
	if err := writeBytesAtomic(filepath.Join(s.receiptDir(), name), b); err != nil {
		return nil, err
	}
	return &ExternalReceipt{
		Issuer: strings.TrimSpace(req.ReceiptIssuer),
		ID:     strings.TrimSpace(req.ReceiptID),
		Path:   filepath.ToSlash(filepath.Join("receipts", name)),
		Digest: dig,
		Bytes:  int64(len(b)),
	}, nil
}

// loadGrantFor resolves the grant an acquisition names.
func (s *Store) loadGrantFor(action string, req SeatRequest) (Grant, error) {
	switch {
	case req.GrantID != "" && req.GrantPath != "":
		return Grant{}, &ErrUnauthorized{Action: action, Why: "name a grant by id OR by file, not both"}
	case req.GrantID != "":
		return s.LoadGrant(action, req.GrantID)
	case req.GrantPath != "":
		var g Grant
		found, err := readJSON(req.GrantPath, &g)
		if err != nil {
			return Grant{}, &ErrUnauthorized{Action: action, Why: "the grant file is unreadable: " + err.Error()}
		}
		if !found {
			return Grant{}, &ErrUnauthorized{Action: action, Why: "no grant file at " + req.GrantPath}
		}
		return g, nil
	default:
		return Grant{}, &ErrUnauthorized{Action: action, Why: "no authorization was presented"}
	}
}

// LoadGrant reads a stored grant by id.
func (s *Store) LoadGrant(action, id string) (Grant, error) {
	if strings.ContainsAny(id, `/\.`) {
		return Grant{}, &ErrUnauthorized{Action: action, Why: fmt.Sprintf("%q is not a grant id", id)}
	}
	var g Grant
	found, err := readJSON(filepath.Join(s.grantDir(), id+".json"), &g)
	if err != nil {
		return Grant{}, &ErrUnauthorized{Action: action, Why: "the grant is unreadable: " + err.Error()}
	}
	if !found {
		return Grant{}, &ErrUnauthorized{Action: action, Why: fmt.Sprintf("no such authorization %q", id)}
	}
	return g, nil
}

// grantConsumed reports whether a grant id has ALREADY been used, according to the
// journal.
//
// The journal is the authority for consumption, not the grant file. A file can be
// deleted, restored from a backup, or copied out and back; the hash-chained record
// of the seat event that used it cannot. This is what makes a grant genuinely
// single-use rather than single-use-if-nobody-touches-the-filesystem.
//
// It checks BOTH acquisitions — claimed and takeover — because both now consume a
// capability. A replay check that only looked at takeovers would let a claim grant be
// spent twice, which is the same hole in the newer half of the mechanism.
func grantConsumed(rep *Replay, id string) bool {
	for _, e := range rep.Entries {
		switch e.Kind {
		case KindSeatClaimed, KindSeatTakeover:
			if e.Authz != nil && e.Authz.GrantID == id {
				return true
			}
		}
	}
	return false
}

// verifyGrant is the full check on a capability, run under the store lock against a
// fresh replay — and it ENDS at the injected verifier, which is the only part of it an
// agent with write access to this store cannot satisfy on its own.
//
// The static checks below BOUND the capability: right seat, right action, right
// grantee, right situation, not expired, not already spent. Every one is necessary and
// not one is sufficient, because every one of them is a check on bytes the agent could
// have written itself. The verifier is what closes that, and it is asked LAST — so it is
// asked only about capabilities that are otherwise well-formed, and its answer is the
// one the transition actually rests on.
func (s *Store) verifyGrant(ctx context.Context, rep *Replay, g Grant, action string, holder principal.Ref, auth Authority, req SeatRequest, now time.Time) (*Attestation, error) {
	fail := func(why string) (*Attestation, error) {
		return nil, &ErrUnauthorized{Action: action, Why: why}
	}
	switch {
	case g.SchemaVersion != SchemaVersion:
		return fail(fmt.Sprintf("the authorization has schema %q, not %q", g.SchemaVersion, SchemaVersion))
	case g.ID == "":
		return fail("the authorization carries no nonce, so its single use cannot be tracked")
	case g.Action != action:
		return fail(fmt.Sprintf("the authorization is for %q, not %q — a capability minted to %s the seat does not authorize a %s",
			g.Action, action, g.Action, action))
	case g.Scope != s.scope.ID:
		return fail(fmt.Sprintf("the authorization was minted for seat %q, not this one (%q) — a capability does not travel between machines or accounts", g.Scope, s.scope.ID))
	case grantConsumed(rep, g.ID):
		return fail(fmt.Sprintf("authorization %s has already been used (the journal records the seat event that consumed it) — a capability is single-use", g.ID))
	case !SameHolder(g.Grantee, holder):
		return fail(fmt.Sprintf("authorization %s was minted for %s, not for %s", g.ID, holderName(g.Grantee), holderName(holder)))
	case g.FromEpoch != auth.Epoch:
		return fail(fmt.Sprintf("authorization %s authorizes acting on epoch %d, but the seat is at epoch %d — "+
			"the situation it was minted for is over, and it does not authorize whatever replaced it", g.ID, g.FromEpoch, auth.Epoch))
	case g.IssuedAt.IsZero() || g.ExpiresAt.IsZero():
		return fail("the authorization has no validity window")
	case now.Before(g.IssuedAt.UTC().Add(-clockSkew)):
		return fail(fmt.Sprintf("authorization %s is not valid yet (issued %s) — a capability dated into the future is a forgery", g.ID, g.IssuedAt.UTC().Format(time.RFC3339)))
	case !now.Before(g.ExpiresAt.UTC()):
		return fail(fmt.Sprintf("authorization %s expired at %s", g.ID, g.ExpiresAt.UTC().Format(time.RFC3339)))
	case strings.TrimSpace(g.Actor) == "":
		return fail("the authorization names no operator")
	}

	switch g.Provenance {
	case ProvenanceExternalReceipt:
		// Re-hash the pinned bytes. This establishes INTEGRITY — the artifact is the one
		// the grant was minted against — and NOTHING ELSE. Who wrote it is the verifier's
		// question, and it is asked below.
		if err := s.verifyReceipt(action, g.Receipt); err != nil {
			return nil, err
		}
	case ProvenanceOperatorAssertion:
		// Nothing to check: an assertion asserts. What it is worth is decided by the
		// attestation the verifier returns, never by its own say-so.
	default:
		return fail(fmt.Sprintf("the authorization has an unknown provenance %q", g.Provenance))
	}

	// THE GATE. Everything above was a check on store bytes. This is the one that is not.
	at, err := s.verifyCapability(ctx, Capability{
		Phase:     PhaseConsume,
		Action:    action,
		Scope:     g.Scope,
		Grantee:   g.Grantee,
		Actor:     g.Actor,
		Reason:    g.Reason,
		FromEpoch: g.FromEpoch,
		Nonce:     g.ID,
		IssuedAt:  g.IssuedAt,
		ExpiresAt: g.ExpiresAt,
		Receipt:   g.Receipt,
		Attended:  req.Attended,
		Prior:     g.Attestation,
		Seat:      s.viewFrom(rep, now),
	})
	if err != nil {
		return nil, err
	}
	return &at, nil
}

// verifyReceipt re-hashes the stored approval artifact, establishing that the bytes are
// the ones the grant was minted against.
//
// INTEGRITY, NOT AUTHENTICITY. A receipt whose bytes no longer match its digest is worse
// than no receipt — it is one somebody edited — but a receipt whose bytes DO match is
// only unaltered, not genuine. Nothing here says who wrote it, and an agent can write a
// file and hash it as easily as a human can. Authenticity is the verifier's job.
func (s *Store) verifyReceipt(action string, rc *ExternalReceipt) error {
	if rc == nil {
		return &ErrUnauthorized{Action: action, Why: "the authorization claims an external receipt but carries none"}
	}
	if rc.Digest == "" || rc.Path == "" {
		return &ErrUnauthorized{Action: action, Why: "the external receipt is not pinned to any bytes"}
	}
	b, err := os.ReadFile(filepath.Join(s.dir, filepath.FromSlash(rc.Path)))
	if err != nil {
		return &ErrUnauthorized{Action: action, Why: "the external receipt's bytes are gone (" + rc.Path + ") — the artifact offered in justification must be there to be audited"}
	}
	if got := digestOf(b); got != rc.Digest {
		return &ErrUnauthorized{Action: action, Why: "the external receipt's bytes no longer match its digest — the artifact was altered"}
	}
	return nil
}

// markGrantConsumed is a courtesy: it moves the spent grant out of the way so
// `steward grants` does not show it as available. It is BEST-EFFORT and is allowed
// to fail silently, because the journal — not this file — is what makes the grant
// single-use (see grantConsumed). Failing a completed takeover because we could not
// tidy up a cache would be strictly worse than an untidy cache.
func (s *Store) markGrantConsumed(g Grant, epoch uint64, now time.Time) {
	_ = writeJSONAtomic(filepath.Join(s.grantDir(), g.ID+".consumed.json"), struct {
		Grant
		ConsumedAt    time.Time `json:"consumed_at"`
		ConsumedEpoch uint64    `json:"consumed_epoch"`
	}{Grant: g, ConsumedAt: now, ConsumedEpoch: epoch})
	_ = os.Remove(filepath.Join(s.grantDir(), g.ID+".json"))
}

// GrantStatus is a grant as `steward grants` reports it: the capability, plus
// whether it can still be used and why not.
type GrantStatus struct {
	Grant    Grant  `json:"grant"`
	Usable   bool   `json:"usable"`
	Consumed bool   `json:"consumed"`
	Reason   string `json:"reason,omitempty"`
}

// ListGrants reports every grant in the store and its true status, checked against
// the journal.
func (s *Store) ListGrants(now time.Time) ([]GrantStatus, error) {
	now = mustUTC(now)
	rep, err := s.Replay()
	if err != nil {
		return nil, err
	}
	auth := deriveAuthority(rep)

	des, err := os.ReadDir(s.grantDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []GrantStatus
	for _, de := range des {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		var g Grant
		if found, err := readJSON(filepath.Join(s.grantDir(), de.Name()), &g); err != nil || !found {
			continue
		}
		st := GrantStatus{Grant: g, Consumed: grantConsumed(rep, g.ID)}
		switch {
		case st.Consumed:
			st.Reason = "already used"
		case !now.Before(g.ExpiresAt.UTC()):
			st.Reason = "expired"
		case g.FromEpoch != auth.Epoch:
			st.Reason = fmt.Sprintf("minted for epoch %d, seat is at %d", g.FromEpoch, auth.Epoch)
		case g.Scope != s.scope.ID:
			st.Reason = "minted for another seat"
		default:
			st.Usable = true
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Grant.IssuedAt.After(out[j].Grant.IssuedAt) })
	return out, nil
}
