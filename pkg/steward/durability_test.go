// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/principal"
)

// These tests kill the process at the worst possible instant.
//
// Every durability claim in this package is a claim about a CRASH — "the repair is
// atomic", "a commit is either visible or it isn't" — and a crash is exactly what an
// ordinary test cannot produce. So the code names its dangerous instants (see failpoint)
// and these tests die at each of them, then look at what an observer would see. A
// durability property that is only asserted in a comment is a durability property nobody
// has ever checked.

var errCrash = errors.New("simulated crash")

// tornJournal builds a store with two good entries and a torn final append — what a
// crash mid-write actually leaves behind — and returns the holder's epoch and the exact
// bytes on disk before any repair.
func tornJournal(t *testing.T) (*Store, principal.Ref, uint64, []byte) {
	t.Helper()
	s := newStore(t)
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))
	mustRecord(t, s, evidenced(a, "api", "work that must survive"), ep, at(time.Minute))

	appendRaw(t, s, `{"schema":"bashy-steward-v1","seq":3,"prev_hash":"sha256:ab`)
	return s, a, ep, journalBytes(t, s)
}

// THE REPAIR IS ATOMIC. There is no observable state in which the journal is SHORTER with
// nothing in it saying why.
//
// This is the correction of a bug that survived one adversarial pass because it hid
// behind a fix. The first revision swallowed the receipt error (`_, _ = s.Record(...)`),
// which was obviously wrong and was obviously fixed. The second revision reported the
// error loudly — and kept the SHAPE: truncate the file, then append the receipt. Two
// separate durable writes, and a crash in the window between them leaves precisely the
// state the receipt exists to prevent. Loudly reporting an error you only reach if you
// did NOT crash is no protection at all: in the crash case there is nobody to report to.
//
// A journal that quietly healed itself is bit-for-bit indistinguishable from a journal
// somebody edited to remove a record they did not like. So: quarantine durably, build the
// receipt against the valid prefix, and swap prefix‖receipt in with ONE atomic rename.
func TestRepairIsAtomicAtEveryCrashPoint(t *testing.T) {
	for _, stage := range []string{"repair.after-quarantine", "repair.before-replace"} {
		t.Run("crash at "+stage, func(t *testing.T) {
			s, a, ep, before := tornJournal(t)
			setFailpoint(t, stage, errCrash)

			_, err := s.Repair(a, ep, at(2*time.Minute))
			if !errors.Is(err, errCrash) {
				t.Fatalf("the failpoint must fire: %v", err)
			}

			// THE OBSERVABLE JOURNAL IS UNCHANGED. Not shortened, not partially receipted.
			if got := journalBytes(t, s); string(got) != string(before) {
				t.Fatalf("a repair that died before its atomic swap must change NOTHING.\n got: %q\nwant: %q", got, before)
			}
			// And it is still recognizably the same damage, so the next repair can do its job.
			rep := mustReplay(t, s)
			if !rep.Corrupt || len(rep.Entries) != 2 {
				t.Fatalf("the damage and the valid prefix must both survive: corrupt=%v entries=%d", rep.Corrupt, len(rep.Entries))
			}
		})
	}

	// A crash AFTER the rename lands: the journal is already repaired AND receipted,
	// because they arrived as one write. This is the case that would have been fatal under
	// the truncate-then-append shape — there, this instant is the shortened-with-no-receipt
	// state.
	t.Run("crash after the atomic swap", func(t *testing.T) {
		s, a, ep, before := tornJournal(t)
		setFailpoint(t, "repair.after-replace", errCrash)

		_, err := s.Repair(a, ep, at(2*time.Minute))
		if !errors.Is(err, errCrash) {
			t.Fatalf("the failpoint must fire: %v", err)
		}

		got := journalBytes(t, s)
		if string(got) == string(before) {
			t.Fatal("the swap had already landed; the journal must show the repair")
		}
		assertRepairedAndReceipted(t, s, got)
	})

	// And the ordinary path produces the same end state.
	t.Run("no crash", func(t *testing.T) {
		s, a, ep, _ := tornJournal(t)
		res, err := s.Repair(a, ep, at(2*time.Minute))
		if err != nil {
			t.Fatalf("Repair: %v", err)
		}
		assertRepairedAndReceipted(t, s, journalBytes(t, s))
		if res.Receipt.Seq != 3 {
			t.Fatalf("the receipt chains onto the valid prefix, got seq %d", res.Receipt.Seq)
		}
	})
}

// assertRepairedAndReceipted is the invariant: the journal replays clean, keeps every
// entry that was ever completed, and ENDS with a repair receipt. Never shortened in
// silence.
func assertRepairedAndReceipted(t *testing.T, s *Store, raw []byte) {
	t.Helper()
	rep := mustReplay(t, s)
	if rep.Corrupt {
		t.Fatalf("the repaired journal must replay clean, got corrupt at line %d: %s", rep.CorruptLine, rep.CorruptReason)
	}
	if len(rep.Entries) != 3 {
		t.Fatalf("the two completed entries survive and the receipt is added: got %d entries", len(rep.Entries))
	}
	last := rep.Entries[len(rep.Entries)-1]
	if last.Kind != KindRepair {
		t.Fatalf("a shortened journal MUST carry a receipt saying so; last entry is %s", last.Kind)
	}
	if last.Outcome != OutcomeDegraded {
		t.Fatalf("a repair discarded bytes — it is never a clean success, got %s", last.Outcome)
	}
	if !strings.Contains(string(raw), "quarantine") {
		t.Fatal("the receipt must name where the discarded bytes went")
	}
	// The quarantined bytes are there to be audited.
	var qpath string
	for _, ev := range last.Evidence {
		if ev.Kind == "quarantine" {
			qpath = ev.Ref
		}
	}
	if qpath == "" {
		t.Fatal("the receipt must carry a quarantine reference")
	}
	if _, err := os.ReadFile(filepath.Join(s.dir, filepath.FromSlash(qpath))); err != nil {
		t.Fatalf("the discarded bytes must be on disk: %v", err)
	}
}

// A COMMITTED OPERATION REPORTS THAT IT COMMITTED.
//
// The journal is the authority; seat.json is a derived cache. When the append lands and
// the cache write then fails, returning a bare error tells the caller "your claim
// failed" — and a caller that believes that RETRIES. The retry replays against a journal
// that already holds the claim, and appends a SECOND seat event, minting a second epoch
// that fences the tenure the first call successfully acquired. The operation that
// "failed" thereby destroys the thing it supposedly did not do.
//
// So the error carries the commit: seq, epoch, and an instruction not to retry.
func TestJournalCommittedButCacheFailedReportsTheCommit(t *testing.T) {
	t.Run("claim", func(t *testing.T) {
		s := newStore(t)
		g := mustGrant(t, s, agent("a"), ActionClaim, at(0))
		setFailpoint(t, "seat.write", errCrash)

		v, err := s.Claim(context.Background(), agent("a"), SeatRequest{GrantID: g.ID, Attended: true}, at(0))

		var committed *ErrCommitted
		if !errors.As(err, &committed) {
			t.Fatalf("a claim whose journal append LANDED must not report a plain failure — a caller that "+
				"retries it will mint a second epoch and fence the tenure it just won. got %v", err)
		}
		if committed.Seq != 1 || committed.Epoch != 1 {
			t.Fatalf("the error must carry what was committed, got seq=%d epoch=%d", committed.Seq, committed.Epoch)
		}
		if !strings.Contains(committed.Error(), "DO NOT RETRY") {
			t.Fatalf("the error must say what NOT to do: %v", committed)
		}
		// The View is usable: the caller really is the holder now.
		if v.Authority.Epoch != 1 || !SameHolder(v.Authority.Holder, agent("a")) {
			t.Fatalf("the committed claim's view must be returned, got %+v", v.Authority)
		}

		// THE JOURNAL AGREES. The seat is held, once.
		rep := mustReplay(t, s)
		claims := 0
		for _, e := range rep.Entries {
			if e.Kind == KindSeatClaimed {
				claims++
			}
		}
		if claims != 1 {
			t.Fatalf("the claim is in the journal exactly once, got %d", claims)
		}
		if a := deriveAuthority(rep); a.Vacant || a.Epoch != 1 {
			t.Fatalf("authority survives a failed cache write — it lives in the journal: %+v", a)
		}
	})

	t.Run("record", func(t *testing.T) {
		s := newStore(t)
		a := agent("a")
		ep := mustClaim(t, s, a, at(0))
		setFailpoint(t, "seat.write", errCrash)

		e, err := s.Record(evidenced(a, "api", "did a thing"), ep, at(time.Minute))
		var committed *ErrCommitted
		if !errors.As(err, &committed) {
			t.Fatalf("a record whose append landed must report the commit, got %v", err)
		}
		if committed.Seq != 2 || committed.Epoch != ep {
			t.Fatalf("the error must carry the committed seq/epoch, got seq=%d epoch=%d", committed.Seq, committed.Epoch)
		}
		if e.Seq != 2 {
			t.Fatalf("the stored entry must be returned alongside the warning, got %+v", e)
		}
		if n := len(mustReplay(t, s).Entries); n != 2 {
			t.Fatalf("the entry is in the journal exactly once, got %d entries", n)
		}
	})

	t.Run("takeover", func(t *testing.T) {
		s := newStore(t)
		mustClaim(t, s, agent("a"), at(0))
		g := mustGrant(t, s, agent("b"), ActionTakeover, at(time.Minute))
		setFailpoint(t, "seat.write", errCrash)

		_, err := s.Takeover(context.Background(), agent("b"), SeatRequest{GrantID: g.ID, Attended: true}, at(time.Minute))
		var committed *ErrCommitted
		if !errors.As(err, &committed) {
			t.Fatalf("a takeover whose append landed must report the commit, got %v", err)
		}
		if committed.Epoch != 2 {
			t.Fatalf("the seizure committed at epoch 2, got %d", committed.Epoch)
		}
		if a := deriveAuthority(mustReplay(t, s)); !SameHolder(a.Holder, agent("b")) {
			t.Fatal("the seat really did change hands — the journal says so")
		}
	})

	t.Run("release", func(t *testing.T) {
		s := newStore(t)
		a := agent("a")
		ep := mustClaim(t, s, a, at(0))
		setFailpoint(t, "seat.remove", errCrash)

		err := s.Release(a, ep, "done", at(time.Minute))
		var committed *ErrCommitted
		if !errors.As(err, &committed) {
			t.Fatalf("a release whose append landed must report the commit — a retry would append a second "+
				"release. got %v", err)
		}
		// The seat IS vacant: the journal says so, and a stale seat.json says nothing, because
		// no projection reads it once authority is vacant.
		v, _ := s.Status(at(2 * time.Minute))
		if !v.Authority.Vacant || v.Liveness != LivenessVacant {
			t.Fatalf("the release committed; the seat is vacant regardless of the leftover cache: %+v", v)
		}
	})
}

// The recovery is IDEMPOTENT and it is the ordinary one. A stale liveness cache is
// exactly the "unknown liveness" state the package already knows how to survive, and the
// holder's way out of it is a heartbeat — which presents the epoch, rebuilds the cache
// from the journal, and is safe to repeat.
func TestCommittedWithStaleCacheRecoversByHeartbeat(t *testing.T) {
	s := newStore(t)
	g := mustGrant(t, s, agent("a"), ActionClaim, at(0))

	setFailpoint(t, "seat.write", errCrash)
	_, err := s.Claim(context.Background(), agent("a"), SeatRequest{GrantID: g.ID, Attended: true}, at(0))
	var committed *ErrCommitted
	if !errors.As(err, &committed) {
		t.Fatalf("expected a committed-with-warning, got %v", err)
	}

	// With no cache, liveness is UNKNOWN — and unknown is deliberately NOT claimable, so
	// nobody else can walk in while the holder is recovering.
	failpoint = func(string) error { return nil }
	v, _ := s.Status(at(time.Minute))
	if v.Liveness != LivenessUnknown || v.Claimable {
		t.Fatalf("a missing cache is unknown liveness, and unknown is not claimable: %+v", v)
	}

	// The holder heartbeats with the epoch the error handed it. That is the whole recovery.
	if err := s.Heartbeat(agent("a"), committed.Epoch, at(2*time.Minute)); err != nil {
		t.Fatalf("Heartbeat with the committed epoch must rebuild the cache: %v", err)
	}
	v, _ = s.Status(at(2 * time.Minute))
	if v.Liveness != LivenessLive || v.Authority.Epoch != committed.Epoch {
		t.Fatalf("the seat is live again at the committed epoch: %+v", v)
	}
	// Idempotent: doing it twice is not a second anything.
	if err := s.Heartbeat(agent("a"), committed.Epoch, at(3*time.Minute)); err != nil {
		t.Fatalf("heartbeat is idempotent: %v", err)
	}
	if n := len(mustReplay(t, s).Entries); n != 1 {
		t.Fatalf("recovery writes no history: %d entries, want 1 (the claim)", n)
	}
}

// ─── A COMMITTED REPAIR SAYS SO ───────────────────────────────────────────────
//
// The rename is the commit. Once writeBytesAtomic returns, the repaired-and-receipted
// journal IS the journal, for every reader, at every instant, including one arriving after
// a power cut. Nothing that happens afterwards can un-commit it.
//
// The previous revision reported the failures that come AFTER that moment as bare errors,
// with an empty RepairResult. That is not a confusing report; it is a FALSE one, and the
// falsehood compounds: a caller handed `RepairResult{}, err` reasonably concludes the
// repair did not happen, and a caller that concludes that RETRIES. The retry replays
// against a journal that is already repaired, finds it intact, and cheerfully reports
// "nothing to repair" — so the operator is told, in sequence, that the repair failed and
// that there was never anything wrong with the journal. Both statements are false, and the
// second one is the kind that ends an investigation.
func TestRepairCommittedThenFailedReportsTheCommit(t *testing.T) {
	// Every way the work AFTER the commit can fail. The crash is the failpoint; the corrupt
	// read-back is the "this should never happen" branch that, when it does happen, must not
	// masquerade as a repair that never ran.
	t.Run("crash after the atomic swap", func(t *testing.T) {
		s, a, ep, _ := tornJournal(t)
		setFailpoint(t, "repair.after-replace", errCrash)

		res, err := s.Repair(a, ep, at(2*time.Minute))

		var c *ErrCommitted
		if !errors.As(err, &c) {
			t.Fatalf("a repair that COMMITTED and then failed must say so — a bare error is one a caller retries, "+
				"and the retry finds an intact journal and reports there was never anything wrong. Got %T: %v", err, err)
		}
		if c.Op != "repair" {
			t.Fatalf("the report must name the operation that committed, got %q", c.Op)
		}
		seq, epoch := c.Committed()
		if seq != 3 || epoch != ep {
			t.Fatalf("it must carry WHAT committed, so the caller can go and look: got seq %d epoch %d, want seq 3 epoch %d",
				seq, epoch, ep)
		}
		if !errors.Is(err, errCrash) {
			t.Fatal("…and it must still unwrap to the underlying cause")
		}

		// THE RESULT IS POPULATED. The repair happened; a caller that ignores the error type
		// and reads the result must not see an empty struct describing a repair that did.
		if res.Receipt.Seq != 3 {
			t.Fatalf("the result must describe the repair that COMMITTED, got receipt seq %d", res.Receipt.Seq)
		}
		if res.Discarded == 0 || res.SuffixDigest == "" || res.QuarantinePath == "" {
			t.Fatalf("a committed repair's result must be whole: %+v", res)
		}
		if res.ValidEntries != 3 {
			t.Fatalf("two good entries plus the receipt, got %d", res.ValidEntries)
		}

		// The remedy must be the RIGHT one. A repair is not recovered by a heartbeat — it is
		// already whole — so telling the operator to run one would be confident, specific, and
		// wrong, which is what the previous revision's fixed sentence did.
		if strings.Contains(c.Remedy, "heartbeat") {
			t.Fatalf("a repair is not rebuilt by a heartbeat; the advice must fit the operation: %q", c.Remedy)
		}
		if !strings.Contains(c.Remedy, "reconcile") {
			t.Fatalf("the remedy must tell the operator how to SEE the state: %q", c.Remedy)
		}
		if !strings.Contains(err.Error(), "DO NOT RETRY") {
			t.Fatalf("and the error must say the one thing that matters: %v", err)
		}

		// And the journal really is repaired — which is the whole reason the error had to say so.
		assertRepairedAndReceipted(t, s, journalBytes(t, s))
	})

	// NO RETRY AMBIGUITY. This is the failure the typed error exists to prevent, played out:
	// a caller that treats the error as "it did not happen" and retries gets told the journal
	// is intact — so the two reports together say the repair failed AND there was nothing to
	// repair, which is how an operator ends up believing a damaged store was fine.
	t.Run("a retry after a committed repair finds nothing to do", func(t *testing.T) {
		s, a, ep, _ := tornJournal(t)
		setFailpoint(t, "repair.after-replace", errCrash)
		if _, err := s.Repair(a, ep, at(2*time.Minute)); err == nil {
			t.Fatal("the failpoint must fire")
		}
		failpoint = func(string) error { return nil }

		res, err := s.Repair(a, ep, at(3*time.Minute))
		if err != nil {
			t.Fatalf("the second call sees an intact journal: %v", err)
		}
		if res.Discarded != 0 || res.Receipt.Seq != 0 {
			t.Fatalf("…and repairs nothing, because there is nothing left to repair: %+v", res)
		}
		// Exactly ONE receipt. A caller that retried on a bare error would have wanted a second
		// one, and the journal must never grow a duplicate record of the same repair.
		rep := mustReplay(t, s)
		var receipts int
		for _, e := range rep.Entries {
			if e.Kind == KindRepair {
				receipts++
			}
		}
		if receipts != 1 {
			t.Fatalf("a repair is recorded exactly once, got %d receipts", receipts)
		}
	})

	// The read-back is the other way the post-commit work can fail, and it is the more
	// alarming one: the repair landed and the result is WRONG. That is a different and much
	// worse fact than "the repair failed", and the operator must be told which one they have.
	t.Run("the readback finds the repaired journal corrupt", func(t *testing.T) {
		s, a, ep, _ := tornJournal(t)

		// Corrupt the journal at the instant AFTER the atomic swap lands — the exact state a
		// bug (or a hostile filesystem) would leave, and the branch the code calls
		// "this should not happen".
		orig := failpoint
		failpoint = func(stage string) error {
			if stage == "repair.after-replace" {
				appendRaw(t, s, `{"schema":"bashy-steward-v1","seq":9,"prev_hash":"sha`)
			}
			return nil
		}
		t.Cleanup(func() { failpoint = orig })

		res, err := s.Repair(a, ep, at(2*time.Minute))

		var c *ErrCommitted
		if !errors.As(err, &c) {
			t.Fatalf("the repair COMMITTED and then read back dirty — reporting that as a plain failure would send "+
				"the operator looking for a repair that did not run, when what they have is one that did. Got %T: %v", err, err)
		}
		if seq, _ := c.Committed(); seq != 3 {
			t.Fatalf("it must still carry what committed, got seq %d", seq)
		}
		if res.Receipt.Seq != 3 {
			t.Fatalf("and the result must still describe it, got %+v", res)
		}
		if !strings.Contains(err.Error(), "STILL unreadable") {
			t.Fatalf("the cause must name what was found, got %v", err)
		}
	})
}
