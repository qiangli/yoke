// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/principal"
)

// Health is the honest verdict on the subsystem as a whole.
type Health string

const (
	// HealthOK — the journal is intact, and every claim in it has been CHECKED.
	// Reaching this is deliberately hard: it needs verification records, not
	// references. Most healthy stores sit at degraded, and that is the truth.
	HealthOK Health = "ok"
	// HealthDegraded — the record is readable, but something in it could not be
	// established: an unevidenced success, a self-declared degradation, or a claim
	// resting on references nobody has checked.
	HealthDegraded Health = "degraded"
	// HealthUnknown — the record ITSELF is damaged. What survives is still valid; what
	// came after it cannot be spoken for.
	//
	// There is no "failed" here on purpose. The subsystem never returns success in the
	// face of missing evidence, but neither does it invent a failure it cannot prove:
	// unknown means unknown.
	HealthUnknown Health = "unknown"
)

// Observation is an adapter's report on what it found IN THE WORLD about one entry's
// claim.
type Observation struct {
	Seq        uint64     `json:"seq"`
	TargetHash string     `json:"target_hash,omitempty"`
	Result     Outcome    `json:"result"` // success | failed | unknown
	Detail     string     `json:"detail,omitempty"`
	Evidence   []Evidence `json:"evidence,omitempty"`
	Observer   string     `json:"observer"`
}

// Observer is how reality actually gets compared against the journal.
//
// The core package is GENERIC and knows nothing about git, CI, GitHub, weave, or any
// other world an entry might be claiming things about. It cannot: the whole point of
// a host-scoped steward is that its journal spans every project on the machine, and
// baking in the checkers for all of them would make this package the union of every
// tool it records.
//
// So it takes adapters. A host supplies Observers that know how to go and look — "did
// commit de6485c actually land on main?", "did that CI run go green?", "is that
// service actually up?" — and reconciliation reports what they FOUND.
//
// AND WITH NO OBSERVER, IT SAYS SO. An earlier revision's reconcile reported that it
// had "compared the journal against reality" while comparing the journal against
// nothing but itself: it re-read its own entries, noticed which ones lacked evidence,
// and called that a reality check. That is not a reality check — it is a spellcheck.
// Reconciliation now sets RealityCompared only when an adapter actually returned
// observations, and states plainly, in prose, in the report, that nothing was checked
// when nothing was.
type Observer interface {
	// Name identifies the adapter in the report and in any verification it produces.
	Name() string
	// Observe examines the entries and returns what it could establish. Returning
	// nothing is a legitimate answer, and it is NOT the same as returning success.
	Observe(ctx context.Context, entries []Entry) ([]Observation, error)
}

// Reconciliation is the explicit "what do we actually know?" report.
//
// It is the verb a successor runs FIRST, before touching anything: who holds the
// seat, whether the journal is intact, which claims are unproven, which rest on
// unchecked references, and which artifacts have gone missing — the difference
// between inheriting a system and inheriting a story about a system.
type Reconciliation struct {
	SchemaVersion string    `json:"schema_version"`
	At            time.Time `json:"at"`
	Health        Health    `json:"health"`

	Seat View `json:"seat"`

	JournalIntact  bool   `json:"journal_intact"`
	JournalEntries int    `json:"journal_entries"`
	JournalHead    string `json:"journal_head"`
	// CorruptTail describes an unreadable tail, if any. The entries BEFORE it are
	// still fully valid and are still counted above — that is the whole point.
	CorruptTail string      `json:"corrupt_tail,omitempty"`
	CorruptKind CorruptKind `json:"corrupt_kind,omitempty"`

	Board Board `json:"board"`

	// RealityCompared is false unless an Observer actually returned observations. When
	// it is false, NOTHING in this report was checked against the world — every claim
	// below stands exactly as the agent that made it left it.
	RealityCompared bool          `json:"reality_compared"`
	Observers       []string      `json:"observers,omitempty"`
	Observations    []Observation `json:"observations,omitempty"`
	ObserverErrors  []string      `json:"observer_errors,omitempty"`
	// RealityNote says, in prose, what the comparison was worth.
	RealityNote string `json:"reality_note"`

	// Unproven lists entries that claimed an outcome they did not evidence at all.
	Unproven []UnprovenClaim `json:"unproven,omitempty"`
	// Asserted lists entries whose claim rests on references NOBODY HAS CHECKED. Not
	// errors — the ordinary state of honest work — but never to be read as verified.
	Asserted []UnprovenClaim `json:"asserted,omitempty"`

	// MissingArtifacts lists artifacts whose bytes are gone. This is NOT an error and
	// never degrades health: transcripts are optional by contract, and every projection
	// is derived without them.
	MissingArtifacts []string `json:"missing_artifacts,omitempty"`

	// TamperedArtifacts lists artifacts whose bytes no longer match their recorded
	// digest. THIS is worth alarm — a present-but-altered artifact is a lie, where an
	// absent one is merely a gap.
	TamperedArtifacts []string `json:"tampered_artifacts,omitempty"`

	// CheckpointsVerified reports each stored checkpoint's reproducibility.
	CheckpointsVerified []CheckpointVerdict `json:"checkpoints_verified,omitempty"`
}

// UnprovenClaim is one entry whose assertion could not be established.
type UnprovenClaim struct {
	Seq        uint64  `json:"seq"`
	Hash       string  `json:"hash"`
	Workstream string  `json:"workstream,omitempty"`
	Claimed    Outcome `json:"claimed"`
	Effective  Outcome `json:"effective"`
	Summary    string  `json:"summary"`
	Why        string  `json:"why"`
}

// Reconcile reports what can and cannot be established. READ-ONLY.
//
// Pass Observers to compare the journal against the world. Pass none and the report
// says — in RealityCompared, in RealityNote, and in the health verdict — that nothing
// was compared. It is allowed, and required, to say "I don't know": a reconciliation
// that always produced a clean verdict would be worthless, and the only useful thing
// it can do is tell you precisely where the record runs out.
func (s *Store) Reconcile(ctx context.Context, now time.Time, observers ...Observer) (Reconciliation, error) {
	now = mustUTC(now)
	rep, err := s.Replay()
	if err != nil {
		return Reconciliation{}, err
	}
	view := s.viewFrom(rep, now)

	r := Reconciliation{
		SchemaVersion:  SchemaVersion,
		At:             now,
		Seat:           view,
		JournalIntact:  rep.Intact(),
		JournalEntries: len(rep.Entries),
		JournalHead:    rep.Head,
		Board:          ProjectBoard(rep.Entries, s.sealChecker()),
	}
	if rep.Corrupt {
		r.CorruptKind = rep.CorruptKind
		r.CorruptTail = fmt.Sprintf("line %d: %s (the %d entries before it are valid and are counted above)",
			rep.CorruptLine, rep.CorruptReason, len(rep.Entries))
	}

	// Which entries have a SEALED attestation against their exact bytes — one a trusted
	// verifier issued and still recognizes?
	//
	// Nothing the caller can write counts here. Not the --method prose, not a digest it
	// attached to its own evidence, not an attestation struct it filled in: if any of those
	// counted, "asserted" would be one sentence (or one sha256) away from "checked", which
	// is the exact distinction this whole report exists to draw. The seal is re-checked
	// against the injected verifier, so a store with none of them reports nothing as
	// attested — which is the truth about a host that cannot check anything.
	sc := s.sealChecker()
	bySeq := entriesBySeq(rep.Entries)
	attested := map[string]bool{}
	for _, e := range rep.Entries {
		if e.Kind != KindVerification || e.Verifies == nil || e.Verifies.Result != OutcomeSuccess {
			continue
		}
		if sealPromotes(e, bySeq[e.Verifies.TargetSeq], sc) {
			attested[e.Verifies.TargetHash] = true
		}
	}

	for _, e := range rep.Entries {
		// Only WORLD claims are graded. A verification is the check itself, and a record
		// fact (a seat claim, a checkpoint, a repair receipt) is made true by being
		// written — there is nothing out there to send an observer at. See Kind.RecordFact
		// for why grading them would make the host permanently degraded and the health
		// verdict flip merely because somebody recorded one.
		if !e.Kind.Authoritative() || e.Outcome == "" || e.Kind == KindVerification || e.Kind.RecordFact() {
			continue
		}
		claim := UnprovenClaim{
			Seq: e.Seq, Hash: e.Hash, Workstream: e.Workstream,
			Claimed: e.Outcome, Effective: e.EffectiveOutcome(), Summary: e.Summary,
		}
		switch {
		case e.Degraded():
			claim.Why = "outcome was recorded as " + string(e.Outcome)
			if e.Outcome == OutcomeSuccess {
				claim.Why = "claimed success with no evidence — a claim nobody can check is not a fact"
			}
			r.Unproven = append(r.Unproven, claim)
		case e.Outcome == OutcomeSuccess && !attested[e.Hash]:
			claim.Why = "claimed success with references nobody has checked — a reference is a pointer, not a verification"
			r.Asserted = append(r.Asserted, claim)
		}
	}

	// Reality: only if an adapter actually went and looked.
	for _, ob := range observers {
		if ob == nil {
			continue
		}
		r.Observers = append(r.Observers, ob.Name())
		obs, err := ob.Observe(ctx, rep.Entries)
		if err != nil {
			r.ObserverErrors = append(r.ObserverErrors, fmt.Sprintf("%s: %v", ob.Name(), err))
			continue
		}
		for _, o := range obs {
			if o.Observer == "" {
				o.Observer = ob.Name()
			}
			r.Observations = append(r.Observations, o)
		}
	}
	r.RealityCompared = len(r.Observations) > 0
	switch {
	case len(observers) == 0:
		r.RealityNote = "NOTHING was compared against reality: no observation adapter was supplied. " +
			"Every claim in this report stands exactly as the agent that made it left it."
	case !r.RealityCompared && len(r.ObserverErrors) > 0:
		r.RealityNote = "every observation adapter FAILED, so nothing was compared against reality."
	case !r.RealityCompared:
		r.RealityNote = "the observation adapters ran and returned no observations — nothing in the journal was " +
			"actually checked against the world."
	default:
		r.RealityNote = fmt.Sprintf("%d observation(s) from %d adapter(s). An observation is what an adapter FOUND; "+
			"it becomes part of the record only when recorded as a verification (`steward verify`).",
			len(r.Observations), len(r.Observers))
	}

	// Artifacts: absent is fine, altered is not.
	for _, e := range rep.Entries {
		if e.Artifact == nil || e.Artifact.Path == "" {
			continue
		}
		path := filepath.Join(s.dir, filepath.FromSlash(e.Artifact.Path))
		b, err := os.ReadFile(path)
		if err != nil {
			r.MissingArtifacts = append(r.MissingArtifacts,
				fmt.Sprintf("seq %d: %s (optional — no projection depends on it)", e.Seq, e.Artifact.Path))
			continue
		}
		if got := digestOf(b); got != e.Artifact.Digest {
			r.TamperedArtifacts = append(r.TamperedArtifacts,
				fmt.Sprintf("seq %d: %s (digest mismatch — the bytes were altered)", e.Seq, e.Artifact.Path))
		}
	}

	if cks, err := s.ListCheckpoints(); err == nil {
		for _, ck := range cks {
			if v, err := s.VerifyCheckpoint(ck); err == nil {
				r.CheckpointsVerified = append(r.CheckpointsVerified, v)
			}
		}
	}

	r.Health = r.deriveHealth()
	return r, nil
}

// deriveHealth grades the whole record. Damage to the record itself outranks
// unproven claims within it: if the journal is torn, everything past the tear is
// unknown, and calling that merely "degraded" would understate it.
func (r *Reconciliation) deriveHealth() Health {
	if !r.JournalIntact {
		return HealthUnknown
	}
	for _, v := range r.CheckpointsVerified {
		if !v.Reproducible {
			return HealthUnknown // a checkpoint that no longer re-derives means history moved
		}
	}
	if len(r.TamperedArtifacts) > 0 {
		return HealthUnknown
	}
	if len(r.Unproven) > 0 || r.Board.Degraded {
		return HealthDegraded
	}
	// A claim resting on references nobody checked is not OK. It is the normal state
	// of a working host, and it is still not a clean bill of health — calling it one is
	// exactly how "an agent said so" becomes "we verified it".
	if len(r.Asserted) > 0 {
		return HealthDegraded
	}
	// A seat whose liveness we cannot establish is a degradation, not a failure — a
	// lapsed heartbeat proves only a lapse, and an unreadable one proves even less.
	if r.Seat.Liveness == LivenessUnknown {
		return HealthDegraded
	}
	return HealthOK
}

// RecordReconciliation appends the reconciliation to the journal as an explicit
// event, so "we checked, and here is what we could not establish" becomes part of the
// permanent record rather than a thing printed once and lost.
//
// Authoritative, and gated like everything else. The entry's outcome mirrors the
// health verdict — including unknown — so a reconciliation that found damage can never
// be replayed later as a success. And its summary states whether reality was actually
// compared: a reconcile entry that let a reader assume a check happened would be the
// most dangerous entry in the journal.
func (s *Store) RecordReconciliation(actor principal.Ref, epoch uint64, r Reconciliation, now time.Time) (Entry, error) {
	outcome := OutcomeSuccess
	switch r.Health {
	case HealthDegraded:
		outcome = OutcomeDegraded
	case HealthUnknown:
		outcome = OutcomeUnknown
	}

	compared := "reality NOT compared (no adapter)"
	if r.RealityCompared {
		compared = fmt.Sprintf("reality compared by %s", strings.Join(r.Observers, "+"))
	}
	summary := fmt.Sprintf("reconciled: %s — %d entries, %d workstreams, %d unproven, %d asserted-not-verified; %s",
		r.Health, r.JournalEntries, len(r.Board.Workstreams), len(r.Unproven), len(r.Asserted), compared)
	if r.CorruptTail != "" {
		summary += "; journal tail unreadable"
	}

	return s.Record(Entry{
		Actor:   actor,
		Kind:    KindReconcile,
		Summary: summary,
		Outcome: outcome,
		Evidence: []Evidence{
			{Kind: "digest", Ref: fmt.Sprintf("journal-head:%d", r.JournalEntries), Digest: r.JournalHead, Note: "journal-head"},
			{Kind: "digest", Ref: fmt.Sprintf("board:%d", r.Board.Watermark), Digest: r.Board.Digest, Note: "board-digest"},
		},
	}, epoch, now)
}

// ─── repair ───────────────────────────────────────────────────────────────────

// RepairPlan is what a repair WOULD do, and — far more often — why it refuses to.
type RepairPlan struct {
	Corrupt      bool        `json:"corrupt"`
	Repairable   bool        `json:"repairable"`
	Kind         CorruptKind `json:"kind,omitempty"`
	Reason       string      `json:"reason,omitempty"`
	ValidEntries int         `json:"valid_entries"`
	ValidBytes   int64       `json:"valid_bytes"`
	SuffixBytes  int64       `json:"suffix_bytes"`
	SuffixDigest string      `json:"suffix_digest,omitempty"`
	// SuffixPreview is the first bytes of what would be discarded, so an operator can
	// see with their own eyes that it is a torn fragment and not a record.
	SuffixPreview string `json:"suffix_preview,omitempty"`
}

// PlanRepair analyses a damaged journal and decides whether the damage is a torn
// final append — the ONLY thing a repair may touch.
//
// The distinction it draws is the whole safety property, so here it is in full.
//
// A TORN FINAL APPEND is what a crash actually leaves: the process died partway
// through writing the last line, so the file ends with an incomplete fragment and NO
// terminating newline. Nothing that was ever completed is in those bytes — by
// definition, since a completed append is fsynced with its newline. Discarding them
// loses nothing, and refusing to would strand the journal forever over a few bytes
// nobody can read.
//
// EVERYTHING ELSE is refused, and the two refusals that matter most:
//
//   - MID-LOG DAMAGE. If the unreadable region is followed by more lines, then
//     whatever is after it was completed, and truncating from the damage point
//     would destroy completed records. The suffix here contains newlines, and that
//     is exactly how we detect it.
//   - A WELL-FORMED RECORD THAT DOES NOT CHAIN. A parseable entry whose hash,
//     prev_hash, seq, or epoch is wrong is not a torn write — it is a record that
//     was altered, or one written around a record that was removed. This is the
//     signature of TAMPERING, and a tool that silently truncated it away would be
//     the attacker's best friend: it would delete the evidence and call it a repair.
//
// A repair that can only ever remove garbage is a repair. A repair that can remove
// data is a data-loss tool with a reassuring name.
func (s *Store) PlanRepair() (RepairPlan, error) {
	rep, err := s.Replay()
	if err != nil {
		return RepairPlan{}, err
	}
	plan := RepairPlan{
		Corrupt:      rep.Corrupt,
		Kind:         rep.CorruptKind,
		ValidEntries: len(rep.Entries),
		ValidBytes:   rep.ValidBytes,
	}
	if !rep.Corrupt {
		return plan, nil
	}

	b, err := os.ReadFile(s.journalPath())
	if err != nil {
		return plan, err
	}
	if int64(len(b)) < rep.ValidBytes {
		plan.Reason = "the journal shrank while it was being read"
		return plan, nil
	}
	suffix := b[rep.ValidBytes:]
	plan.SuffixBytes = int64(len(suffix))
	plan.SuffixDigest = digestOf(suffix)
	plan.SuffixPreview = preview(suffix, 120)

	switch {
	case rep.CorruptKind != CorruptTornAppend:
		plan.Reason = fmt.Sprintf("the damage at line %d is not a torn append (%s) — a completed record is unreadable "+
			"or does not chain, which means tampering or loss, not a crash", rep.CorruptLine, rep.CorruptReason)
	case len(suffix) == 0:
		plan.Reason = "there is nothing after the last valid entry"
	case bytes.ContainsRune(suffix, '\n'):
		// A newline in the suffix means at least one COMPLETE line follows the damage.
		// Whatever it is, it was fully written, and it is not ours to throw away.
		plan.Reason = fmt.Sprintf("the unreadable region at line %d is followed by %d more complete line(s) — "+
			"this is damage in the MIDDLE of the log, and truncating from here would discard records that were "+
			"completely written", rep.CorruptLine, bytes.Count(suffix, []byte("\n")))
	case looksLikeEntry(suffix):
		plan.Reason = fmt.Sprintf("the trailing bytes at line %d parse as a journal entry but do not chain — "+
			"a complete record that does not verify is evidence of tampering, not a torn write", rep.CorruptLine)
	default:
		plan.Repairable = true
		plan.Reason = fmt.Sprintf("the final append was torn: %d trailing byte(s) with no terminating newline, "+
			"unparsable, after %d valid entries", plan.SuffixBytes, plan.ValidEntries)
	}
	return plan, nil
}

// looksLikeEntry reports whether the bytes are a complete, parseable journal entry.
// Such a thing in a corrupt suffix is NOT a torn write — the writer finished, and the
// record still does not verify.
func looksLikeEntry(b []byte) bool {
	var e Entry
	if err := jsonUnmarshalStrict(bytes.TrimSpace(b), &e); err != nil {
		return false
	}
	return e.Hash != "" || e.Seq != 0
}

// RepairResult reports what a repair actually did.
type RepairResult struct {
	Discarded      int64  `json:"discarded_bytes"`
	SuffixDigest   string `json:"suffix_digest"`
	QuarantinePath string `json:"quarantine_path"`
	Receipt        Entry  `json:"receipt"`
	ValidEntries   int    `json:"valid_entries"`
}

// Repair truncates a TORN FINAL APPEND — and nothing else — quarantining the exact
// bytes it discards and recording a durable receipt under the current holder and
// epoch.
//
// Every clause of that sentence is load-bearing, and each replaces something the
// earlier revision got wrong:
//
//   - TORN FINAL APPEND, AND NOTHING ELSE. It refuses (ErrNotRepairable) on mid-log
//     damage or on a complete-but-unchained record. See PlanRepair.
//
//   - AUTHORIZED. It goes through the same gate as every other mutation: the holder,
//     at the current epoch. A damaged journal is not a licence for a stranger to
//     truncate the host's record — if anything it is the moment that matters most.
//     (The gate used is authorizeDamaged, which skips only the readability check,
//     because refusing to repair a journal on the grounds that it needs repairing
//     would be a fine joke and a useless tool.)
//
//   - QUARANTINED. The discarded bytes are copied out, by digest, BEFORE the
//     truncation. A repair that destroys the only copy of what it removed cannot be
//     audited, and "the tool ate it" is not an acceptable answer to "what was in
//     those bytes?".
//
//   - RECEIPTED, ATOMICALLY, AND THE RECEIPT MAY NOT FAIL SILENTLY. Two revisions got
//     this wrong in two different ways, and the second is the subtler one.
//
//     The first wrote its receipt with `_, _ = s.Record(...)`, so a failure silently
//     produced a truncated journal with NO record that anything had been removed.
//
//     The second fixed the swallowing but kept the SHAPE: truncate the file, then append
//     the receipt. Those are two separate durable writes, and a crash — or a kill, or a
//     full disk — in the window between them leaves exactly the state the receipt exists
//     to prevent: a journal that is SHORTER, with nothing in it saying why. That is
//     bit-for-bit indistinguishable from a journal somebody edited to remove a record
//     they did not like, and the loud error message the code printed on the way out is no
//     help at all, because in the crash case there is nobody to print it to.
//
//     So the repair is now ONE atomic write. The valid prefix and the fully-formed,
//     already-authorized receipt are assembled in a temp file, fsynced, and renamed over
//     the journal, with the directory fsynced after. The observable journal is therefore
//     either the ORIGINAL CORRUPT BYTES or the REPAIRED-AND-RECEIPTED BYTES. There is no
//     third state, at any instant, for any observer — see the failpoint tests, which kill
//     the repair at each stage and assert exactly that.
func (s *Store) Repair(actor principal.Ref, epoch uint64, now time.Time) (RepairResult, error) {
	now = mustUTC(now)
	var out RepairResult
	err := s.withLock(func() error {
		rep, err := s.Replay()
		if err != nil {
			return err
		}
		out.ValidEntries = len(rep.Entries)
		if !rep.Corrupt {
			return nil // nothing to do; not an error
		}

		plan, err := s.PlanRepair()
		if err != nil {
			return err
		}
		// AUTHORITY FIRST — before we touch a single byte. A torn tail is not a licence
		// for a stranger to truncate the host's record; if anything it is the moment that
		// matters most. authorizeDamaged skips only the readability gate, because refusing
		// to repair a journal on the grounds that it needs repairing would be a fine joke
		// and a useless tool.
		if _, err := authorizeDamaged(rep, actor, epoch); err != nil {
			return err
		}
		if !plan.Repairable {
			return &ErrNotRepairable{Plan: plan}
		}

		b, err := os.ReadFile(s.journalPath())
		if err != nil {
			return err
		}
		if int64(len(b)) < plan.ValidBytes {
			return fmt.Errorf("steward: the journal shrank while the repair was being planned — refusing to touch it")
		}
		suffix := b[plan.ValidBytes:]
		if digestOf(suffix) != plan.SuffixDigest {
			return fmt.Errorf("steward: the journal changed while the repair was being planned — refusing to truncate")
		}

		// (1) QUARANTINE, DURABLY, FIRST. writeBytesAtomic fsyncs the bytes and the
		// directory entry, so the discarded suffix is on disk BEFORE anything else moves.
		// If this fails, nothing has been destroyed and nothing has been changed.
		qname := fmt.Sprintf("%s-%s.bin", now.UTC().Format("20060102T150405Z"),
			strings.TrimPrefix(plan.SuffixDigest, "sha256:")[:12])
		qpath := filepath.Join(s.quarantineDir(), qname)
		if err := writeBytesAtomic(qpath, suffix); err != nil {
			return fmt.Errorf("steward: cannot quarantine the bytes this repair would discard, so the repair is "+
				"refused — nothing has been changed: %w", err)
		}
		if err := failpoint("repair.after-quarantine"); err != nil {
			return err
		}

		qrel := filepath.ToSlash(filepath.Join("quarantine", qname))

		// (2) BUILD the receipt against the VALID PREFIX — the state the journal will be
		// in — without writing anything. It chains to rep.Head/rep.HeadSeq, which is
		// exactly where the good history ends.
		receipt, line, err := prepareEntry(rep, Entry{
			Actor: actor,
			Epoch: epoch,
			Kind:  KindRepair,
			Summary: fmt.Sprintf("repaired journal: discarded %d torn trailing byte(s) after seq %d; "+
				"the exact bytes are quarantined at %s", plan.SuffixBytes, rep.HeadSeq, qrel),
			Rationale: plan.Reason,
			Outcome:   OutcomeDegraded, // data was discarded — never a clean success
			Evidence: []Evidence{
				{Kind: "quarantine", Ref: qrel, Digest: plan.SuffixDigest, Note: "discarded-suffix"},
				{Kind: "digest", Ref: fmt.Sprintf("valid-bytes:%d", plan.ValidBytes), Digest: rep.Head, Note: "journal-head-before"},
			},
		}, now)
		if err != nil {
			return fmt.Errorf("steward: cannot build the repair receipt, so the repair is refused — "+
				"nothing has been changed: %w", err)
		}

		// (3) ONE ATOMIC REPLACEMENT: valid prefix ‖ receipt. Written to a temp file,
		// fsynced, renamed over the journal, directory fsynced. A reader at ANY instant —
		// including a successor mid-recovery, including after a power cut — sees either the
		// old corrupt journal or the repaired-and-receipted one. The shortened-without-a-
		// receipt state that the truncate-then-append shape could produce does not exist.
		repaired := make([]byte, 0, int(plan.ValidBytes)+len(line))
		repaired = append(repaired, b[:plan.ValidBytes]...)
		repaired = append(repaired, line...)

		if err := failpoint("repair.before-replace"); err != nil {
			return err
		}
		if err := writeBytesAtomic(s.journalPath(), repaired); err != nil {
			return fmt.Errorf("steward: the repair could not be written, so it was NOT performed — the journal is "+
				"untouched and the torn bytes are still there (a copy is quarantined at %s): %w", qrel, err)
		}

		// ─── THE RENAME LANDED. THE REPAIR IS COMMITTED. ──────────────────────────────
		//
		// Everything from here on is AFTER the fact, and must say so. writeBytesAtomic
		// fsynced the bytes and renamed over the journal: the repaired-and-receipted
		// journal IS the journal now, for every reader, at every instant, including one
		// that arrives after a power cut. Nothing below can un-commit it.
		//
		// So the result is populated HERE, before anything else is allowed to fail, and
		// every failure below is reported as ErrCommitted rather than as a bare error.
		// The previous revision did neither: the after-replace failpoint returned a naked
		// error with `out` still zeroed, and the readback errors returned naked errors too.
		// A caller seeing `Repair() -> RepairResult{}, err` reasonably concludes the repair
		// did not happen — and a caller that concludes that RETRIES. The retry replays
		// against a journal that is already repaired, finds it intact, and reports "nothing
		// to repair", so the operator is told, in sequence, that the repair failed and that
		// there was never anything wrong. That is not a confusing error; it is a false one.
		out.Discarded = plan.SuffixBytes
		out.SuffixDigest = plan.SuffixDigest
		out.QuarantinePath = qrel
		out.Receipt = receipt
		out.ValidEntries = len(rep.Entries) + 1

		// A repair is not recovered by a heartbeat — it is already whole. What a failure
		// below means is that we could not CONFIRM the result, so the remedy is to go and
		// look, not to run anything.
		remedy := fmt.Sprintf("The journal is repaired: the torn bytes are gone, the receipt is at seq %d, and the "+
			"discarded bytes are quarantined at %s. What did not complete is the read-back that CONFIRMS it. "+
			"Run `steward reconcile` (a pure read) to see the journal's actual state before doing anything else.",
			receipt.Seq, qrel)

		if err := failpoint("repair.after-replace"); err != nil {
			return committedWith("repair", receipt.Seq, receipt.Epoch, err, remedy)
		}

		// Paranoia, and cheap: read back what we just installed. If the repaired journal
		// does not replay clean, we have produced a state nobody understands, and the
		// operator needs to hear it now rather than at the next write — but they need to
		// hear it as "the repair committed AND the result is wrong", which is a different
		// and much worse fact than "the repair failed".
		rep2, err := s.Replay()
		if err != nil {
			return committedWith("repair", receipt.Seq, receipt.Epoch,
				fmt.Errorf("the repaired journal could not be read back: %w", err), remedy)
		}
		if rep2.Corrupt {
			return committedWith("repair", receipt.Seq, receipt.Epoch,
				fmt.Errorf("the repaired journal is STILL unreadable (line %d: %s) — this should not happen; "+
					"do not write to this store until it is understood", rep2.CorruptLine, rep2.CorruptReason),
				remedy)
		}
		return nil
	})
	return out, err
}

func preview(b []byte, n int) string {
	s := strings.ToValidUTF8(string(b), "?")
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
