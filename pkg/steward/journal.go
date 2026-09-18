// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/principal"
)

// SchemaVersion is the on-disk contract for every artifact this package writes:
// journal entries, the seat file, grants, and checkpoints. Bump on a breaking
// change — and note that a MISMATCH is never tolerated on the seat cache or a
// grant: a record this package cannot fully understand is not one it may act on.
const SchemaVersion = "bashy-steward-v1"

// genesis is the chain's public root: the PrevHash of the first entry in a fresh
// journal. Fixed and known, so a verifier needs no side channel to check the head.
const genesis = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// Kind classifies a journal entry — and, crucially, its AUTHORITY.
type Kind string

const (
	// KindEffect — AUTHORITATIVE. Something changed in the world (a command ran, a
	// PR merged, a service restarted). Carries evidence, or it projects unknown.
	KindEffect Kind = "effect"
	// KindObservation — AUTHORITATIVE. Something was observed to be true, without
	// this steward having caused it. Also evidence-bearing.
	KindObservation Kind = "observation"
	// KindDecision — AUTHORITATIVE. What was decided, and WHY. It asserts intent,
	// not effect, so it needs a rationale rather than evidence.
	KindDecision Kind = "decision"
	// KindVerification — AUTHORITATIVE. Someone went and CHECKED an earlier entry's
	// claim, and this is the durable attestation of what they found. It is the ONLY
	// thing that promotes a claim to "verified" on the board: a reference is a
	// pointer, an attestation is a check, and the whole package turns on the
	// difference.
	KindVerification Kind = "verification"
	// KindTranscript — NON-AUTHORITATIVE. An optional hash-linked conversation
	// artifact. No projection reads it. Deleting every one changes no view.
	KindTranscript Kind = "transcript"

	// Seat lifecycle. These are the AUTHORITY events: the epoch ladder derives from
	// them and from nowhere else, which is what makes the seat recoverable by replay
	// when seat.json is gone.
	KindSeatClaimed  Kind = "seat.claimed"
	KindSeatTakeover Kind = "seat.takeover"
	KindSeatReleased Kind = "seat.released"

	// Workstream lifecycle.
	KindWorkstreamOpen   Kind = "workstream.open"
	KindWorkstreamUpdate Kind = "workstream.update"
	KindWorkstreamClose  Kind = "workstream.close"

	// KindReconcile records an explicit reconciliation: someone compared the journal
	// against reality and wrote down what they found, including what could NOT be
	// established.
	KindReconcile Kind = "reconcile"
	// KindRepair records a journal repair: what was discarded, and where the
	// discarded bytes were quarantined.
	KindRepair Kind = "repair"
	// KindCheckpoint marks that a checkpoint was materialized at this watermark.
	KindCheckpoint Kind = "checkpoint"
)

var knownKinds = map[Kind]bool{
	KindEffect: true, KindObservation: true, KindDecision: true,
	KindVerification: true, KindTranscript: true,
	KindSeatClaimed: true, KindSeatTakeover: true, KindSeatReleased: true,
	KindWorkstreamOpen: true, KindWorkstreamUpdate: true, KindWorkstreamClose: true,
	KindReconcile: true, KindRepair: true, KindCheckpoint: true,
}

// Known reports whether this is a kind the package understands. An entry of an
// unknown kind is refused at append: a projection cannot honestly account for a
// record whose meaning it does not know.
func (k Kind) Known() bool { return knownKinds[k] }

// Authoritative reports whether anything may be DERIVED from an entry of this
// kind. Transcripts are the sole exception, and the exception is the point.
func (k Kind) Authoritative() bool { return k != KindTranscript }

// SeatEvent reports whether this kind establishes seat authority (and therefore
// mints its own epoch, rather than being fenced against the current one).
func (k Kind) SeatEvent() bool {
	switch k {
	case KindSeatClaimed, KindSeatTakeover, KindSeatReleased:
		return true
	}
	return false
}

// RecordFact reports whether an entry of this kind is made true BY BEING WRITTEN,
// rather than by something out in the world.
//
// The distinction decides who reconciliation may accuse. A world claim — an effect, an
// observation, a workstream closed as done — asserts that the machine, the repo, or the
// service is in some state, and a skeptic can go and look. A record fact asserts
// something about the JOURNAL ITSELF: this agent took the seat, this checkpoint was
// materialized at this watermark, these torn bytes were discarded. The entry does not
// describe the acquisition; the entry IS the acquisition. There is nowhere to send an
// observer, because there is nothing out there to see.
//
// Reconcile therefore holds record facts to no evidentiary standard, and the reason is
// not politeness — it is that grading them produces nonsense in both directions:
//
//   - Every seat claim carries Outcome success and points at its own epoch, so counting
//     it as an unchecked claim would mean the host is degraded from the moment anyone
//     becomes steward, forever, with no act available to anybody that could clear it.
//   - Recording a CLEAN reconciliation would append an entry claiming success that the
//     next reconciliation then reads back as unverified — so the act of writing down
//     "everything checks out" is what makes the next answer "degraded". A health signal
//     that flips because you asked it is not a health signal.
//
// What keeps this from becoming a laundering hole is that record facts are checked in
// the ways that actually apply to them, and those checks are stricter than the
// evidence ladder, not weaker: the hash chain verifies every entry on replay, a
// checkpoint is independently re-derived (CheckpointsVerified) and a mismatch drops
// health to unknown, and a damaged journal is reported as damage rather than as an
// unproven claim.
func (k Kind) RecordFact() bool {
	switch k {
	case KindSeatClaimed, KindSeatTakeover, KindSeatReleased,
		KindCheckpoint, KindReconcile, KindRepair:
		return true
	}
	return false
}

// Outcome is what an entry claims happened.
type Outcome string

const (
	OutcomeSuccess  Outcome = "success"
	OutcomeFailed   Outcome = "failed"
	OutcomeUnknown  Outcome = "unknown"
	OutcomeDegraded Outcome = "degraded"
)

// Evidence is a pointer to something a skeptic could go and check: a command that
// ran, a file, a commit, a test result, a URL.
//
// A piece of evidence is a REFERENCE, not a verification. This distinction is the
// spine of the package. "command:go test ./..." records that someone said they ran
// the tests; it does not record that the tests passed, and nothing in this package
// will ever pretend otherwise. Only two things are stronger:
//
//   - a DIGEST binds the reference to exact bytes, so the artifact it names can be
//     rehashed and caught if it changes;
//   - a VERIFICATION entry (KindVerification) is somebody's durable, authorized
//     attestation that they went and checked.
//
// The board promotes a claim to "verified" for the second of those, and for
// nothing else. See Confidence.
type Evidence struct {
	Kind   string `json:"kind"` // command | file | commit | test | url | digest | note | human | seat | principal | grant
	Ref    string `json:"ref"`
	Digest string `json:"digest,omitempty"`
	Note   string `json:"note,omitempty"`
}

// DigestBound reports whether this reference is pinned to exact bytes. Stronger
// than a bare reference; still not a verification.
func (e Evidence) DigestBound() bool { return strings.HasPrefix(e.Digest, "sha256:") }

var evidenceKinds = map[string]bool{
	"command": true, "file": true, "commit": true, "test": true, "url": true,
	"digest": true, "note": true, "human": true, "seat": true, "principal": true,
	"grant": true, "quarantine": true,
}

// ParseEvidence reads the CLI form "kind:ref[#digest]" — e.g.
// "command:go test ./...", "commit:de6485c", "file:/tmp/out.log#sha256:abc…".
// A bare string with no recognized kind prefix is recorded as a note-kind
// reference rather than being rejected: weak evidence is still better than
// silently dropped evidence, and a note projects no better than any other
// unverified reference anyway.
func ParseEvidence(s string) (Evidence, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Evidence{}, fmt.Errorf("steward: empty evidence")
	}
	ev := Evidence{Kind: "note", Ref: s}
	if k, rest, ok := strings.Cut(s, ":"); ok && rest != "" && evidenceKinds[k] {
		ev.Kind, ev.Ref = k, rest
	}
	if ref, dig, ok := strings.Cut(ev.Ref, "#"); ok && strings.HasPrefix(dig, "sha256:") {
		ev.Ref, ev.Digest = ref, dig
	}
	return ev, nil
}

// Artifact is an optional, hash-linked blob referenced by an entry — typically a
// transcript. The Digest binds the entry to exact bytes; the Path is a HINT about
// where those bytes were last seen.
//
// The bytes are allowed to be gone. A missing artifact is a degradation of the
// record's richness, never of its truth: every projection is derived from the
// entries, so a host that deletes every artifact still reports the same board.
type Artifact struct {
	Path   string `json:"path,omitempty"` // relative to the store dir
	Digest string `json:"digest"`         // sha256:…
	Bytes  int64  `json:"bytes,omitempty"`
	Media  string `json:"media,omitempty"`
}

// Verification is an attestation that an earlier entry's claim was CHECKED.
//
// It binds to the target's HASH, not just its seq. Binding to a seq alone would
// let an attestation of one claim be read as an attestation of whatever ends up at
// that sequence number — and since the whole point is to stop unearned trust, the
// attestation has to name the exact bytes it vouched for.
//
// NOTHING A CALLER CAN WRITE PROMOTES A CLAIM. This is the correction that matters here,
// and the previous two revisions each got it wrong in a different way.
//
// The first required a --method string and promoted on the strength of it — so "I re-ran
// the suite on a clean checkout", typed by an agent that did no such thing, produced the
// same green row as the truth. The second demanded something "enforceable" and accepted
// two things that were not: DIGEST-BOUND EVIDENCE (a digest proves the bytes did not
// change, never that a check ran — an agent hashes any file it likes, or types thirty-two
// bytes, since nothing rehashes it) and a public Adapter *Attestation the caller simply
// filled in with Approved=true, Grade=verified. Each moved the trust-me claim one field
// sideways and promoted it there instead.
//
// So promotion rests on a Seal, minted by a VerificationVerifier the HOST injected and
// RE-CHECKED by that verifier when the board is projected. See verification.go, which is
// the argument in full. Method and Evidence survive because a human reading the log wants
// them; they decide nothing.
type Verification struct {
	TargetSeq  uint64  `json:"target_seq"`
	TargetHash string  `json:"target_hash"`
	Result     Outcome `json:"result"`             // success | failed | unknown
	Method     string  `json:"method,omitempty"`   // how it was checked — PROSE. Promotes nothing.
	Observer   string  `json:"observer,omitempty"` // the verifier's name, or the operator

	// Seal is the trusted verifier's verdict, and the ONLY thing that promotes a claim to
	// verified. It is MINTED BY THE STORE from an injected VerificationVerifier's answer
	// and is never accepted from a caller — Attest refuses a Verification that arrives
	// with one already set (ErrSealSupplied). Its Approved/Grade fields are descriptive;
	// its Token is what the projection re-checks. See verification.go.
	Seal *Seal `json:"seal,omitempty"`
}

// Sealed reports whether a trusted verifier vouched for this verification AT ALL — a
// cheap, descriptive check for the log and the CLI.
//
// IT IS NOT THE PROMOTION GATE and must never be used as one: it reads only the bytes in
// the record, which is exactly what a forger controls. Promotion asks the injected
// verifier to re-check the Seal against the claim (sealPromotes). This says "there is a
// seal here"; only the verifier can say "and it is mine".
func (v *Verification) Sealed() bool {
	return v != nil && v.Seal != nil && v.Seal.Approved && v.Seal.Grade == GradeVerified && v.Seal.Token != ""
}

// Link is a pointer from a workstream to where the work actually lives.
type Link struct {
	Kind string `json:"kind"` // issue | pr | github | weave | kb | url | host
	Ref  string `json:"ref"`
}

// WorkstreamUpdate carries the Kanban fields a host-level board needs: who owns a
// strand, how urgent it is, what is blocking it, and what happens next.
//
// It is a JOURNAL ENTRY, not a mutable row. The board is still a pure projection —
// "set the priority to p0" is recorded as a fact that someone set it, at a time,
// under an epoch, and the board simply folds the latest value. That is what keeps
// a practical Kanban from quietly becoming a second, writable source of truth.
type WorkstreamUpdate struct {
	Lane       Lane     `json:"lane,omitempty"`
	Priority   Priority `json:"priority,omitempty"`
	Owner      string   `json:"owner,omitempty"`
	Agents     []string `json:"agents,omitempty"`
	Blockers   []string `json:"blockers,omitempty"`
	NextAction string   `json:"next_action,omitempty"`
	NextAt     string   `json:"next_at,omitempty"` // RFC3339, the next checkpoint
	Links      []Link   `json:"links,omitempty"`

	// Clear names fields to reset ("blockers", "agents", "links", "owner",
	// "next_action", "next_at", "priority", "lane"). Unblocking is as much of an
	// event as blocking, and a field you can only ever set is a field that rots.
	Clear []string `json:"clear,omitempty"`
}

// AuthzRef is the authorization a seat acquisition — a claim OR a takeover — was
// performed under, recorded IN the journal so the acquisition carries its own receipt
// forever.
//
// Attestation is the load-bearing field, and the rest of the struct is context. The
// capability's bounds (grant id, action, epoch, expiry) say what the authorization was
// ALLOWED to reach; the attestation says what actually ESTABLISHED it, at what grade,
// over what channel. An AuthzRef with an audit-grade attestation and one with a
// verified-grade attestation are different facts, and the journal keeps them different
// facts forever — see Grade.
type AuthzRef struct {
	GrantID     string           `json:"grant_id"`
	Action      string           `json:"action"` // claim | takeover
	Provenance  Provenance       `json:"provenance"`
	Actor       string           `json:"actor"` // the operator identity ASSERTED at mint time
	FromEpoch   uint64           `json:"from_epoch"`
	IssuedAt    string           `json:"issued_at"`
	ExpiresAt   string           `json:"expires_at"`
	Attended    bool             `json:"attended"` // the host OBSERVED a terminal. Not a credential.
	Receipt     *ExternalReceipt `json:"receipt,omitempty"`
	Attestation *Attestation     `json:"attestation,omitempty"`
}

// Verified reports whether the authority behind this acquisition was established by a
// trusted verifier rooted outside this store — as opposed to merely asserted at a
// terminal this process cannot authenticate.
func (a *AuthzRef) Verified() bool {
	return a != nil && a.Attestation != nil && a.Attestation.Approved && a.Attestation.Grade == GradeVerified
}

// Entry is one journal record. Field order is the canonical serialization order
// for the hash — DO NOT reorder without bumping SchemaVersion.
type Entry struct {
	Schema   string `json:"schema"`
	Seq      uint64 `json:"seq"`
	PrevHash string `json:"prev_hash"`
	ID       string `json:"id"`
	Time     string `json:"time"` // RFC3339Nano, UTC

	// Actor is who wrote this entry, and Epoch is the seat epoch they held when they
	// wrote it. Together they are the fencing token: an entry bearing a superseded
	// epoch is rejected at append (see ErrFenced) and is recognizable forever after
	// in the log. Epoch is NEVER zero on a stored entry.
	Actor principal.Ref `json:"actor"`
	Epoch uint64        `json:"epoch"`

	Kind Kind `json:"kind"`

	// Workstream is the strand of work this belongs to; Ref is a free pointer
	// (issue id, PR, host, service) that the board shows but does not interpret.
	Workstream string `json:"workstream,omitempty"`
	Ref        string `json:"ref,omitempty"`

	Summary   string     `json:"summary"`
	Rationale string     `json:"rationale,omitempty"`
	Outcome   Outcome    `json:"outcome,omitempty"`
	Evidence  []Evidence `json:"evidence,omitempty"`
	Artifact  *Artifact  `json:"artifact,omitempty"`

	Verifies *Verification     `json:"verifies,omitempty"`
	Update   *WorkstreamUpdate `json:"update,omitempty"`
	Authz    *AuthzRef         `json:"authz,omitempty"`

	// Hash is H(prev_hash ‖ canonical(entry with Hash="")). Last field, so the
	// canonical form is simply this entry with this one field zeroed.
	Hash string `json:"hash"`
}

// HasEvidence reports whether the entry carries at least one checkable reference.
func (e Entry) HasEvidence() bool { return len(e.Evidence) > 0 }

// EffectiveOutcome is the outcome a PROJECTION is allowed to believe, which is not
// always the outcome that was claimed.
//
// The rule: a claim of success with NOTHING to point at is not success — it is
// unknown. An agent that says "done ✅" and cannot name a single thing a skeptic
// could check has told you a story, and a system that records stories as facts is
// worse than one that records nothing, because it launders confidence.
//
// Note what this does NOT do: it does not promote a claim WITH references into a
// verified fact either. That is Confidence's job, and only a KindVerification entry
// gets a claim there. A reference means someone pointed; verification means someone
// checked.
//
// Degradation travels one way only. A FAILURE without evidence stays a failure; we
// never upgrade toward the happy path, because the cost of a false "success" is
// unbounded and the cost of a false "failed" is a second look.
func (e Entry) EffectiveOutcome() Outcome {
	if e.Outcome == OutcomeSuccess && !e.HasEvidence() {
		return OutcomeUnknown
	}
	return e.Outcome
}

// Degraded reports whether this entry's claim could not be established — either it
// says so itself, or it claimed success and brought nothing to back it up.
func (e Entry) Degraded() bool {
	switch e.EffectiveOutcome() {
	case OutcomeUnknown, OutcomeDegraded:
		return true
	}
	return false
}

// computeHash returns the chain hash for e given the previous entry's hash. The
// entry's own Hash is cleared first, so each link binds to the full content of the
// one before it: alter or delete any entry and every later entry's hash stops
// verifying.
func (e Entry) computeHash(prevHash string) string {
	e.Hash = ""
	body, _ := json.Marshal(e)
	var buf bytes.Buffer
	buf.WriteString(prevHash)
	buf.WriteByte(0) // separator, so prev‖body is unambiguous
	buf.Write(body)
	return digestOf(buf.Bytes())
}

// newID mints a stable, sortable entry id.
func newID(now time.Time, seq uint64) string {
	return fmt.Sprintf("%s-%04d", now.UTC().Format("20060102T150405Z"), seq)
}

// CorruptKind classifies what is wrong with a journal's tail, because the two
// possibilities call for opposite responses and conflating them is how a repair
// tool becomes a data-loss tool.
type CorruptKind string

const (
	// CorruptNone — the journal replays cleanly.
	CorruptNone CorruptKind = ""
	// CorruptTornAppend — the FINAL append was cut off mid-line: the trailing bytes
	// are an incomplete record with no terminating newline. This is what a crash or a
	// full disk actually leaves behind, it destroys nothing that was ever completed,
	// and it is the ONLY thing Repair will touch.
	CorruptTornAppend CorruptKind = "torn-append"
	// CorruptInvalid — anything else: a well-formed record that does not chain, a
	// hash that does not verify, a sequence gap, damage in the MIDDLE of the log, or
	// a suffix that still contains complete lines. None of these is a torn write.
	// Every one of them means either tampering or the loss of something that was
	// completed, and neither is a thing a tool may silently truncate away.
	CorruptInvalid CorruptKind = "invalid"
)

// Replay is the result of walking the journal: the valid prefix, plus an honest
// account of anything it could not read.
//
// "Valid PREFIX" is the important word. A journal whose final line was torn by a
// crash still has a perfectly good history before the tear, and that history is what
// a successor needs. Refusing to read the whole file because its last 40 bytes are
// garbage would turn a survivable crash into total amnesia — the exact failure this
// subsystem exists to prevent.
type Replay struct {
	Entries  []Entry
	Head     string // chain hash of the last valid entry (genesis if none)
	HeadSeq  uint64
	MaxEpoch uint64

	// Corrupt reports that bytes after the valid prefix could not be replayed.
	Corrupt       bool
	CorruptKind   CorruptKind
	CorruptLine   int    // 1-based line number where replay stopped
	CorruptReason string // why it stopped
	// ValidBytes is the byte offset just past the last valid entry — the exact
	// truncation point a repair uses, so a repair can never remove a valid entry.
	ValidBytes int64
}

// Intact reports a fully-readable journal.
func (r *Replay) Intact() bool { return !r.Corrupt }

// ErrCorruptTail is returned by any write when the journal's tail is unreadable.
// Writes refuse rather than silently forking the chain around the damage; reads
// carry on. Clearing it is an explicit, human-invoked act: `steward repair`.
type ErrCorruptTail struct {
	Line         int
	Reason       string
	Kind         CorruptKind
	ValidEntries int
}

func (e *ErrCorruptTail) Error() string {
	s := fmt.Sprintf("steward: journal tail is unreadable at line %d (%s); the %d entries before it are intact",
		e.Line, e.Reason, e.ValidEntries)
	if e.Kind == CorruptTornAppend {
		return s + " — this is a torn final append; `steward repair` quarantines the partial bytes and resumes"
	}
	return s + " — this is NOT a torn append (a completed record is unreadable or does not chain), so it will not be " +
		"auto-repaired: something was tampered with or lost, and truncating it would destroy the evidence"
}

// ErrNotRepairable is returned by Repair when the damage is not a torn final
// append. Failing closed here is the whole point: see CorruptInvalid.
type ErrNotRepairable struct {
	Plan RepairPlan
}

func (e *ErrNotRepairable) Error() string {
	return fmt.Sprintf("steward: refusing to repair — %s. %d valid entries precede the damage and are readable; "+
		"the %d unreadable byte(s) are NOT a torn final append, so discarding them could destroy a completed record. "+
		"Investigate by hand (`steward repair --plan` shows the exact bytes and their digest)",
		e.Plan.Reason, e.Plan.ValidEntries, e.Plan.SuffixBytes)
}

// replayReader walks a journal stream, verifying every link, and stops at the first
// byte it cannot trust — returning everything valid before it.
func replayReader(r io.Reader) *Replay {
	out := &Replay{Head: genesis}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)

	prevHash := genesis
	var prevSeq uint64
	var offset int64
	line := 0

	for sc.Scan() {
		line++
		raw := sc.Bytes()
		// +1 for the newline the scanner stripped. Tracked so a repair knows the exact
		// byte at which the good history ends.
		lineBytes := int64(len(raw)) + 1

		text := strings.TrimSpace(string(raw))
		if text == "" {
			offset += lineBytes
			continue
		}

		var e Entry
		if err := json.Unmarshal([]byte(text), &e); err != nil {
			// Unparsable: MAY be a torn write. Whether it actually is one is decided by
			// PlanRepair, which can see the bytes (a torn write has no trailing newline
			// and nothing after it); replay itself does not get to assume the friendly
			// explanation.
			out.stop(line, "unparsable entry: "+err.Error(), CorruptTornAppend)
			return out
		}
		if e.Schema != SchemaVersion {
			out.stop(line, fmt.Sprintf("schema %q is not %q", e.Schema, SchemaVersion), CorruptInvalid)
			return out
		}
		if e.PrevHash != prevHash {
			out.stop(line, "prev_hash mismatch (an earlier entry was altered or removed)", CorruptInvalid)
			return out
		}
		if e.Seq != prevSeq+1 {
			out.stop(line, fmt.Sprintf("seq gap (expected %d, got %d)", prevSeq+1, e.Seq), CorruptInvalid)
			return out
		}
		if e.Epoch == 0 {
			// No stored entry ever carried epoch 0. One that does was not written by
			// this package.
			out.stop(line, "entry carries epoch 0 — no authoritative write is ever unfenced", CorruptInvalid)
			return out
		}
		if want := e.computeHash(prevHash); want != e.Hash {
			out.stop(line, "hash mismatch (this entry was altered)", CorruptInvalid)
			return out
		}

		out.Entries = append(out.Entries, e)
		prevHash, prevSeq = e.Hash, e.Seq
		if e.Epoch > out.MaxEpoch {
			out.MaxEpoch = e.Epoch
		}
		offset += lineBytes
		out.ValidBytes = offset
	}
	if err := sc.Err(); err != nil {
		out.stop(line+1, "read error: "+err.Error(), CorruptInvalid)
		return out
	}
	out.Head, out.HeadSeq = prevHash, prevSeq
	return out
}

// stop records where replay lost trust, keeping every entry read so far.
func (r *Replay) stop(line int, reason string, kind CorruptKind) {
	r.Corrupt, r.CorruptLine, r.CorruptReason, r.CorruptKind = true, line, reason, kind
	if n := len(r.Entries); n > 0 {
		r.Head, r.HeadSeq = r.Entries[n-1].Hash, r.Entries[n-1].Seq
	}
}

// Verify walks a journal stream and reports whether the whole chain holds. Same
// walk as Replay; this is the caller-facing "is my history intact?".
func Verify(r io.Reader) *Replay { return replayReader(r) }

// readJournal replays the journal file. A missing journal is an empty one — a fresh
// host has no history, and that is not an error.
func readJournal(path string) (*Replay, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Replay{Head: genesis}, nil
		}
		return nil, err
	}
	defer f.Close()
	return replayReader(f), nil
}

// appendEntry links e to the chain head and durably appends it.
//
// The caller MUST hold the store lock, MUST have already replayed to obtain rep,
// and MUST have passed the authority gate (see Store.appendAuthorized — the ONLY
// caller of this function outside the seat lifecycle). Append never re-derives the
// head on its own: a read-decide-write that re-reads outside the lock is the race
// this whole package exists to avoid.
func appendEntry(path string, rep *Replay, e Entry, now time.Time) (Entry, error) {
	if rep.Corrupt {
		return Entry{}, &ErrCorruptTail{Line: rep.CorruptLine, Reason: rep.CorruptReason, Kind: rep.CorruptKind, ValidEntries: len(rep.Entries)}
	}
	e, line, err := prepareEntry(rep, e, now)
	if err != nil {
		return Entry{}, err
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return Entry{}, err
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return Entry{}, err
	}
	// fsync: an entry the journal reported as written must survive the power going
	// out a microsecond later. The journal is the only authority there is — if it can
	// lose a write, everything derived from it is a guess.
	if err := f.Sync(); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// prepareEntry links an entry to the chain head and serializes it, WITHOUT writing.
//
// Factored out of appendEntry for Repair, which must build its receipt and the repaired
// journal as ONE set of bytes to be swapped in atomically — it cannot truncate first and
// append the receipt second, because a crash between those two steps leaves a journal
// that is shorter with nothing in it saying why, which is indistinguishable from one
// somebody edited. See Store.Repair.
//
// It deliberately does NOT check rep.Corrupt: the caller owns that judgement, and Repair
// is precisely the caller for whom a corrupt replay is the normal input.
func prepareEntry(rep *Replay, e Entry, now time.Time) (Entry, []byte, error) {
	if !e.Kind.Known() {
		return Entry{}, nil, fmt.Errorf("steward: refusing to append an entry of unknown kind %q — "+
			"a projection cannot honestly account for a record whose meaning it does not know", e.Kind)
	}
	if e.Epoch == 0 {
		// Belt and braces: the gate already rejected epoch 0. If it ever gets here,
		// the gate has a hole, and an unfenced entry in the journal is exactly the
		// thing an attacker (or a bug) needs.
		return Entry{}, nil, &ErrNoEpoch{}
	}
	e.Schema = SchemaVersion
	e.Seq = rep.HeadSeq + 1
	e.PrevHash = rep.Head
	if e.Time == "" {
		e.Time = now.UTC().Format(time.RFC3339Nano)
	}
	if e.ID == "" {
		e.ID = newID(now, e.Seq)
	}
	sortEvidence(e.Evidence)
	e.Hash = e.computeHash(e.PrevHash)

	line, err := json.Marshal(e)
	if err != nil {
		return Entry{}, nil, err
	}
	return e, append(line, '\n'), nil
}

// sortEvidence gives evidence a canonical order so an entry's hash does not depend
// on the order the flags happened to arrive in.
func sortEvidence(evs []Evidence) {
	sort.SliceStable(evs, func(i, j int) bool {
		if evs[i].Kind != evs[j].Kind {
			return evs[i].Kind < evs[j].Kind
		}
		return evs[i].Ref < evs[j].Ref
	})
}
