// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/principal"
)

// Record appends an authoritative entry to the journal, FENCED against the seat.
//
// Every gate lives in one place (see authorize): the journal must be readable, the
// seat must be held, a NONZERO epoch must be presented, that epoch must be the
// current one, and the actor must be the holder. There is no "epoch 0 means whatever
// I hold" — see ErrNoEpoch for why that shortcut was the hole.
func (s *Store) Record(e Entry, epoch uint64, now time.Time) (Entry, error) {
	now = mustUTC(now)
	var out Entry
	err := s.withLock(func() error {
		rep, err := s.Replay()
		if err != nil {
			return err
		}
		stored, err := s.appendAuthorized(rep, e, epoch, now)
		if err != nil {
			return err
		}
		out = stored
		// Writing to the journal IS a heartbeat: a steward actively recording is
		// self-evidently alive, and making it prove that separately is busywork that
		// only ever fails at the worst moment. The authority here is the one the gate
		// just verified, so this cannot refresh a tenure that has ended.
		//
		// The entry is COMMITTED at this point. If the liveness cache write fails, the
		// caller is told so — WITH the seq it committed — rather than being handed a bare
		// error it would reasonably retry, appending the same record twice. See ErrCommitted.
		return committed("record", stored.Seq, stored.Epoch, s.writeSeat(deriveAuthority(rep), "", now))
	})
	return out, err
}

// Decide is the front door for an explicit decision record: what was decided, and
// WHY.
//
// A decision asserts INTENT, not effect, so it needs a rationale rather than
// evidence. This is the entry a successor reads to understand not just what happened
// on this host, but what the previous steward had concluded and was steering toward —
// which no amount of effect-replay would ever recover.
func (s *Store) Decide(actor principal.Ref, epoch uint64, workstream, summary, rationale string, evidence []Evidence, now time.Time) (Entry, error) {
	return s.Record(Entry{
		Actor:      actor,
		Kind:       KindDecision,
		Workstream: workstream,
		Summary:    summary,
		Rationale:  rationale,
		Evidence:   evidence,
	}, epoch, now)
}

// Attest records a VERIFICATION: somebody went and checked an earlier entry's claim,
// and this is the durable attestation of what they found.
//
// This is the ONLY thing that can promote a claim to "verified" on the board. Evidence on
// the original entry is a POINTER — "go look at commit de6485c", "I ran go test" — and a
// pointer is not a check. An agent can attach a plausible reference to a claim it never
// made true, and the reference will look exactly like one attached to a claim that is.
//
// BUT RECORDING A VERIFICATION IS NOT THE SAME AS EARNING ONE, and separating those two is
// what this revision fixes. Two previous attempts both let the CALLER supply the thing that
// promoted its own claim:
//
//	--method "re-ran the suite"      prose. An agent that would claim success it did not
//	                                 earn will write a sentence it did not run.
//	-e file:x#sha256:…               a digest. It proves bytes did not CHANGE; it says
//	                                 nothing about whether a check RAN. Nothing rehashes it
//	                                 at promotion time, so any 32 bytes did.
//	Verification.Adapter             a public struct with Approved and Grade fields. The
//	                                 caller filled in its own credential.
//
// So promotion is rooted where authority already is: in a VerificationVerifier the HOST
// injects (WithVerificationVerifier), which this method ASKS, and whose Seal the board
// later RE-CHECKS with that same verifier. A caller cannot implement it, name it in a
// config file, or write it to disk. See verification.go.
//
// What Attest does with the answer:
//
//	no verifier injected     records the check, UNSEALED. The board says asserted, which
//	                         is what an unverified claim is. Fail-closed by default.
//	verifier cannot say      records the check, UNSEALED. Same as above — this is the floor,
//	                         not a new hole.
//	verifier REFUTES it      REFUSES to record it as a success (ErrRefuted). A claim the one
//	                         trusted party actively refuted must not enter the log wearing a
//	                         success label. Record it as --result failed instead.
//	verifier vouches         records the check WITH the Seal. This, and only this, promotes.
//
// A REFUTING or INCONCLUSIVE verification needs no credential at all, and the asymmetry is
// the whole ethic of this package: degradation travels one way. We demand credentials to
// become more confident and never to become less, because the cost of a false "verified" is
// unbounded and the cost of a false "refuted" is a second look.
func (s *Store) Attest(ctx context.Context, actor principal.Ref, epoch uint64, v Verification, evidence []Evidence, now time.Time) (Entry, error) {
	now = mustUTC(now)
	var out Entry
	err := s.withLock(func() error {
		rep, err := s.Replay()
		if err != nil {
			return err
		}
		// AUTHORITY FIRST. The gate runs again at append time — it is the real one — but a
		// caller who may not write at all gets told THAT, rather than being walked through
		// the evidentiary rules for a record it was never going to be allowed to make. A
		// fenced zombie needs to hear "your tenure ended", not "your verification is
		// unpersuasive".
		if _, err := authorize(rep, actor, epoch); err != nil {
			return err
		}

		// A SEAL IS MINTED HERE, NEVER ACCEPTED. A caller arriving with one already set is
		// writing its own credential for its own claim — the exact move this file exists to
		// refuse — so it is rejected loudly rather than quietly overwritten.
		if v.Seal != nil {
			return &ErrSealSupplied{TargetSeq: v.TargetSeq}
		}

		target, ok := entryBySeq(rep.Entries, v.TargetSeq)
		if !ok {
			return fmt.Errorf("steward: cannot verify seq %d — no such entry in the journal", v.TargetSeq)
		}
		switch {
		case v.TargetHash == "":
			v.TargetHash = target.Hash
		case v.TargetHash != target.Hash:
			return fmt.Errorf("steward: refusing to verify seq %d — you attested to entry %s but seq %d is %s. "+
				"An attestation names the exact bytes it vouched for; if the journal moved under you, re-read it",
				v.TargetSeq, short8(v.TargetHash), v.TargetSeq, short8(target.Hash))
		}
		switch v.Result {
		case OutcomeSuccess, OutcomeFailed, OutcomeUnknown:
		default:
			return fmt.Errorf("steward: a verification result is success, failed, or unknown — not %q", v.Result)
		}
		if strings.TrimSpace(v.Method) == "" {
			return fmt.Errorf("steward: a verification must say HOW it checked (--method). " +
				"It is prose, and prose promotes nothing — but a check nobody can even describe is not one")
		}

		e := Entry{
			Actor:      actor,
			Kind:       KindVerification,
			Workstream: target.Workstream,
			Ref:        target.ID,
			Summary: fmt.Sprintf("verified seq %d (%s): %s", v.TargetSeq, v.Result,
				truncate(target.Summary, 60)),
			Outcome:  v.Result,
			Evidence: evidence,
			Verifies: &v,
		}

		// THE PROMOTION GATE. Only a success can promote, so only a success is put in front
		// of the trusted verifier; failure and unknown pass straight through, because they
		// take confidence away rather than granting it.
		if v.Result == OutcomeSuccess {
			seal, err := s.mintSeal(ctx, e, epoch, target)
			if err != nil {
				return err // the verifier went and looked, and refuted it — see ErrRefuted
			}
			v.Seal = seal // nil when nothing trusted could vouch: recorded, not promoted
		}

		stored, err := s.appendAuthorized(rep, e, epoch, now)
		if err != nil {
			return err
		}
		out = stored
		return committed("verify", stored.Seq, stored.Epoch, s.writeSeat(deriveAuthority(rep), "", now))
	})
	return out, err
}

func entryBySeq(entries []Entry, seq uint64) (Entry, bool) {
	for _, e := range entries {
		if e.Seq == seq {
			return e, true
		}
	}
	return Entry{}, false
}

// entriesBySeq indexes the journal so a projection can find the entry a verification
// vouched for without rescanning per verification. The zero Entry for an unknown seq is
// deliberate and safe: a seal re-checked against an empty target cannot match the claim it
// was minted for, so a dangling verification promotes nothing.
func entriesBySeq(entries []Entry) map[uint64]Entry {
	m := make(map[uint64]Entry, len(entries))
	for _, e := range entries {
		m[e.Seq] = e
	}
	return m
}

// Transcript stores an OPTIONAL, non-authoritative conversation artifact and records
// a hash-linked pointer to it.
//
// The order of operations here is the whole point, and an earlier revision had it
// backwards. It:
//
//  1. CHECKS AUTHORITY FIRST. A bystander does not get to write megabytes into the
//     steward's store and only then be told they may not journal it. The pre-check is
//     advisory (the real gate runs under the lock at append time) but it is what keeps
//     an unauthorized caller from touching the disk at all.
//  2. BOUNDS THE READ. The content comes from an agent-supplied stream. An unbounded
//     io.ReadAll on it is a way to fill the human's disk with bytes no projection will
//     ever look at.
//  3. CLEANS UP IF THE JOURNAL REFUSES. An artifact with no entry pointing at it is
//     litter — and worse, it is litter that looks like evidence. If we created the file
//     and the append then fails, the file goes.
//
// Nothing in any projection reads the artifact: delete the whole transcripts
// directory and every board, status, history, and checkpoint on the host is
// bit-identical (TestTranscriptDeletionDoesNotAffectProjections). A decision record
// says what was decided; a transcript lets a human go back and see how the room got
// there. Useful, and never load-bearing.
func (s *Store) Transcript(actor principal.Ref, epoch uint64, workstream, summary string, content io.Reader, now time.Time) (Entry, error) {
	now = mustUTC(now)

	// (1) Authority before bytes.
	rep, err := s.Replay()
	if err != nil {
		return Entry{}, err
	}
	if _, err := authorize(rep, actor, epoch); err != nil {
		return Entry{}, err
	}

	// (2) Bounded read. +1 byte so "exactly at the limit" is distinguishable from
	// "over it" without reading the whole stream to find out.
	limit := s.maxTranscript
	b, err := io.ReadAll(io.LimitReader(content, limit+1))
	if err != nil {
		return Entry{}, err
	}
	if int64(len(b)) > limit {
		return Entry{}, fmt.Errorf("steward: transcript exceeds the %d-byte limit. A transcript is a courtesy artifact "+
			"that no projection reads — it does not get to fill the disk. Trim it, or store it where it belongs and "+
			"record a `file:` reference instead", limit)
	}
	if len(b) == 0 {
		return Entry{}, fmt.Errorf("steward: refusing to record an empty transcript")
	}

	digest := digestOf(b)
	name := strings.TrimPrefix(digest, "sha256:") + ".txt"
	path := filepath.Join(s.transcriptDir(), name)

	// (3) Only clean up what we created. The store is content-addressed, so an
	// identical transcript may already be referenced by an earlier entry — removing
	// that on our failure would break someone else's record.
	_, statErr := os.Stat(path)
	preexisting := statErr == nil

	if err := writeBytesAtomic(path, b); err != nil {
		return Entry{}, err
	}

	e, err := s.Record(Entry{
		Actor:      actor,
		Kind:       KindTranscript,
		Workstream: workstream,
		Summary:    summary,
		Artifact: &Artifact{
			Path:   filepath.ToSlash(filepath.Join("transcripts", name)),
			Digest: digest,
			Bytes:  int64(len(b)),
			Media:  "text/plain",
		},
	}, epoch, now)
	if err != nil {
		if !preexisting {
			_ = os.Remove(path)
		}
		return Entry{}, err
	}
	return e, nil
}

// OpenWorkstream records the start of a strand of work.
func (s *Store) OpenWorkstream(actor principal.Ref, epoch uint64, name, title string, now time.Time) (Entry, error) {
	if strings.TrimSpace(name) == "" {
		return Entry{}, fmt.Errorf("steward: a workstream needs a name")
	}
	return s.Record(Entry{
		Actor:      actor,
		Kind:       KindWorkstreamOpen,
		Workstream: name,
		Summary:    title,
	}, epoch, now)
}

// UpdateWorkstream records a change to a strand's Kanban fields: lane, priority,
// owner, agents, blockers, next action, links.
//
// It is an ENTRY, not an edit. Nothing is mutated in place — the board folds the
// latest recorded value for each field, so the Kanban stays a pure projection of the
// journal, and "who moved this to p0, and when" is answerable forever.
func (s *Store) UpdateWorkstream(actor principal.Ref, epoch uint64, name string, u WorkstreamUpdate, now time.Time) (Entry, error) {
	if strings.TrimSpace(name) == "" {
		return Entry{}, fmt.Errorf("steward: a workstream update needs a name")
	}
	if u.Lane != "" && !u.Lane.Valid() {
		return Entry{}, fmt.Errorf("steward: %q is not a lane (%s)", u.Lane, strings.Join(laneNames(), ", "))
	}
	if u.Priority != "" && !u.Priority.Valid() {
		return Entry{}, fmt.Errorf("steward: %q is not a priority (p0, p1, p2, p3)", u.Priority)
	}
	if u.NextAt != "" {
		if _, err := time.Parse(time.RFC3339, u.NextAt); err != nil {
			return Entry{}, fmt.Errorf("steward: --next-at %q is not an RFC3339 time", u.NextAt)
		}
	}
	return s.Record(Entry{
		Actor:      actor,
		Kind:       KindWorkstreamUpdate,
		Workstream: name,
		Summary:    describeUpdate(name, u),
		Update:     &u,
	}, epoch, now)
}

func describeUpdate(name string, u WorkstreamUpdate) string {
	var parts []string
	if u.Lane != "" {
		parts = append(parts, "lane="+string(u.Lane))
	}
	if u.Priority != "" {
		parts = append(parts, "priority="+string(u.Priority))
	}
	if u.Owner != "" {
		parts = append(parts, "owner="+u.Owner)
	}
	if len(u.Agents) > 0 {
		parts = append(parts, "agents="+strings.Join(u.Agents, "+"))
	}
	if len(u.Blockers) > 0 {
		parts = append(parts, fmt.Sprintf("blockers=%d", len(u.Blockers)))
	}
	if u.NextAction != "" {
		parts = append(parts, "next="+truncate(u.NextAction, 40))
	}
	if u.NextAt != "" {
		parts = append(parts, "next-at="+u.NextAt)
	}
	if len(u.Links) > 0 {
		parts = append(parts, fmt.Sprintf("links=%d", len(u.Links)))
	}
	if len(u.Clear) > 0 {
		parts = append(parts, "cleared="+strings.Join(u.Clear, "+"))
	}
	if len(parts) == 0 {
		return "updated " + name
	}
	return "updated " + name + ": " + strings.Join(parts, ", ")
}

// CloseWorkstream records the end of a strand of work, with its outcome.
//
// Note what this does NOT do: it does not force the outcome to success. A closing
// entry that claims success with no evidence still projects as UNKNOWN, and one whose
// evidence nobody attested to still projects as ASSERTED rather than verified — so
// "closed", "claimed done", and "verified done" remain three different facts. That is
// the entire difference between a status board and a wish list.
func (s *Store) CloseWorkstream(actor principal.Ref, epoch uint64, name, summary string, outcome Outcome, evidence []Evidence, now time.Time) (Entry, error) {
	if strings.TrimSpace(name) == "" {
		return Entry{}, fmt.Errorf("steward: a workstream needs a name")
	}
	return s.Record(Entry{
		Actor:      actor,
		Kind:       KindWorkstreamClose,
		Workstream: name,
		Summary:    summary,
		Outcome:    outcome,
		Evidence:   evidence,
	}, epoch, now)
}
