// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package steward is the host/user-scoped seat of authority and continuity: exactly
// one steward per machine-and-account, holding an append-only, hash-chained,
// evidence-carrying journal that outlives whoever holds the seat.
//
// # The problem
//
// An agentic host accumulates two kinds of loss, and they are different things.
//
// LOST WORK is a session dying with an uncommitted diff in the tree. That is
// pkg/handoff's problem: capture the working tree, restore it into a successor's
// checkout.
//
// LOST AUTHORITY AND LOST TRUTH is the agent that has been answering for this machine
// vanishing, and with it the only account of what was actually done, what was decided,
// and what was merely CLAIMED. Nobody can say who is in charge, and nobody can
// distinguish "we verified that" from "an agent said so". That is this package, and it
// is the harder one, because the incumbent NEVER GETS TO SAY GOODBYE. A steward that
// crashed, hit a rate limit, or was killed leaves no handoff note. Continuity has to
// work anyway.
//
// # The design, and why each part is load-bearing
//
// ONE JOURNAL, MANY VIEWS. journal.jsonl is the only authority. Board, status, log,
// conversation, history and checkpoints are read-only PROJECTIONS derived by replay. A
// view has no state of its own, so it structurally cannot drift into a competing
// writable truth.
//
// A REFERENCE IS NOT A VERIFICATION — AND NEITHER IS ANYTHING ELSE THE CALLER CAN WRITE.
// This is the spine. An entry claiming success with NOTHING to point at projects as UNKNOWN
// (Entry.EffectiveOutcome). An entry claiming success WITH references projects as ASSERTED
// — still not verified. Only a KindVerification entry reaches ConfidenceVerified, and only
// when a VerificationVerifier the HOST injected (WithVerificationVerifier) went and
// established the claim and SEALED it — a seal the board then asks that same verifier to
// re-check when it projects. See verification.go, which is the argument in full.
//
// That clause is not a detail; it is where three revisions in a row left the hole in the
// middle of their own thesis, each time by letting the agent supply BOTH the claim and the
// credential that vouched for it:
//
//   - a --method string, and the board promoted on the strength of the sentence;
//   - DIGEST-BOUND evidence, and the board promoted on the strength of a hash — but a
//     digest is INTEGRITY, not a check: it proves some bytes did not change and says nothing
//     about whether anybody looked, and since nothing rehashes it at promotion time, thirty-
//     two arbitrary bytes did just as well;
//   - a public Verification.Adapter *Attestation with Approved and Grade fields, which the
//     caller simply filled in itself.
//
// Each move eliminated nothing; it relocated the trust-me claim one field sideways and
// promoted it there. What an agent cannot do is implement an interface it does not hold, so
// that is where promotion now lives. WITH NO VERIFIER INJECTED, NOTHING IS EVER PROMOTED:
// checks are recorded in full, and the board says asserted — which is the honest state of a
// claim on a host that cannot check anything.
//
// Degradation travels one way. Refuting a claim needs no credential at all: we demand
// evidence to become more confident and never to become less, because the cost of a false
// "verified" is unbounded and the cost of a false "refuted" is a second look.
//
// AUTHORITY vs LIVENESS. Authority (holder, epoch) replays from the journal; liveness
// comes from seat.json, which is a CACHE that is validated against the journal before
// it is believed at all — wrong schema, wrong holder, wrong epoch, or a timestamp from
// the future and it is discarded, not merely discounted.
//
// THE SEAT IS KEYED TO THE MACHINE AND THE OS ACCOUNT — NOT TO THE ENVIRONMENT. The
// account is the kernel's (the UID; the SID on Windows), never $USER/$LOGNAME/$USERNAME,
// which are strings a process inherits and can overwrite: `USER=someone-else steward
// claim` used to be a DIFFERENT SEAT, so an agent could sidestep the singleton by
// exporting a variable rather than taking anything. The machine is the OS's stable id
// (/etc/machine-id, kern.uuid, MachineGuid), never the hostname, which is a label a
// machine wears — two boxes both called "localhost" sharing a synced home would otherwise
// merge their journals and their stewards. Where the OS offers none — and ONLY there —
// $BASHY_HOST_ID names it; where the OS answers, the variable is IGNORED, because a machine
// identity a process can choose is not one (it was consulted first once, and
// `BASHY_HOST_ID=elsewhere` was a fresh seat for the asking). With neither, the seat FAILS
// CLOSED (ErrNoStableIdentity), because every guessable fallback is one two machines can
// share. See Scope.
//
// AND THE SEAT LIVES IN EXACTLY ONE DIRECTORY. The identity above is only half the
// singleton, and the missing half was an open door. Each store binds itself to its seat and
// refuses to be adopted by another (ErrScopeMismatch) — that is dir → scope. Nothing said
// scope → dir, so `--dir /tmp/mine` (or $BASHY_STEWARD_DIR, or a plain Open elsewhere) got
// a fresh store, which bound ITSELF to the same seat, minted its own epoch ladder from an
// empty journal, and handed over the seat: two stewards on one host, each at epoch 1,
// neither able to fence the other, because fencing compares epochs WITHIN one journal and
// there were now two.
//
// So a CANONICAL REGISTRY, kept outside every store and keyed by the scope digest, maps each
// seat to its ONE directory and refuses any other (ErrScopeDirConflict). It is taken under a
// per-scope lock, so first-bind cannot race, and it is REVALIDATED before every mutation, so
// a handle whose seat was rebound behind its back is refused at its next write rather than
// journaling into an orphan. In a shared home the entries are keyed by machine, so two boxes
// still get two seats. See registry.go — including what it is NOT worth: an agent that can
// delete the registry can delete the journal, and no filesystem check survives that.
//
// AND THE REGISTRY'S OWN ROOT COMES FROM THE OS ACCOUNT, NOT FROM $HOME. It was rooted at
// os.UserHomeDir once, which is $HOME (%USERPROFILE% on Windows) — a string the governed
// process can set — so the escape survived with one extra variable: `HOME=/tmp/other
// BASHY_STEWARD_DIR=/tmp/other/store` reached a PRISTINE registry, found no binding in it,
// and bound a second seat. A root the agent can relocate is a root it can always find empty,
// and an empty registry is a licence to mint. The root is now the OS account's own home (the
// passwd record for the real uid; the access token's profile directory on Windows) — as
// unspoofable as the uid and the SID the seat is already keyed on — and with no account
// record it FAILS CLOSED (ErrNoAccountHome) rather than falling back to $HOME or to a temp
// dir, os.TempDir being $TMPDIR and therefore the same hole. The store DIRECTORY is still
// movable by --dir/$BASHY_STEWARD_DIR/$HOME, deliberately: saying where a seat keeps its bytes
// is not permission to have two of it. WithRegistryRoot is the trusted in-process hook.
//
// A STALE HEARTBEAT PROVES ONLY A LAPSE. It never proves death. So Claim takes a seat
// only when it is VACANT or when a TRUSTWORTHY heartbeat says the holder is LAPSED, and
// the claim bumps a monotonic fencing epoch so the returning incumbent is rejected
// (ErrFenced) rather than silently interleaving its writes.
//
// CLAIMING DOES NOT RENEW A HELD SEAT. Re-claiming a seat you already hold and are live
// in was once quietly treated as a heartbeat — a way to refresh a tenure WITHOUT
// presenting the epoch, which is the one thing that can tell a steward its tenure ended
// while it was away. A live holder renews through Heartbeat, which presents the token.
// Being yourself is not a credential.
//
// AN UNREADABLE LIVENESS RECORD PROVES LESS THAN A LAPSE, SO IT IS NOT CLAIMABLE. "I
// looked and the incumbent is late" is a fact about the incumbent; "I cannot find or
// trust the record" is a fact about the RECORD, and every way of producing it is also a
// way of producing it deliberately. Deleting one file must not be enough to take a
// healthy steward's seat. Recovering from unknown is a takeover.
//
// EVERY AUTHORITATIVE WRITE PRESENTS A FENCING EPOCH, AND ZERO IS NOT A VALUE. There is
// no "use whatever epoch is current" shortcut — that convenience was a hole, because an
// agent that does not know its tenure ended is precisely the agent that would use it.
// The epoch is checked BEFORE identity, so the same logical principal returning with a
// stale token is fenced exactly like a stranger.
//
// EVERY ACQUISITION OF AUTHORITY IS AUTHORIZED — CLAIM AND TAKEOVER BOTH. Takeover
// obviously. Claim too, and that is the less obvious half: a LAPSED seat has an incumbent,
// and "lapsed" proves a heartbeat gap and nothing more, so an unattended agent that could
// claim a lapsed seat could simply wait out the TTL and depose a working steward — the
// takeover it was forbidden to perform, spelled differently. A vacant seat is still the
// seat of authority for the whole machine; "whoever gets there first" is a race, not a
// policy. Both consume a Grant: single-use (its nonce is recorded in the journal by the
// seat event that spends it), expiring, bound to one grantee, one action, one seat, and
// one epoch.
//
// AND THE CAPABILITY IS NOT THE AUTHORITY. A grant is a file in a store the agent can
// write to, so a capability rooted in store state is one the agent can mint for itself.
// The root of trust is an injected Verifier (WithVerifier) — code the HOST supplies, which
// the transition asks at mint time AND AGAIN when the capability is spent. With no
// verifier, every authority transition FAILS CLOSED (ErrNoVerifier); reads keep working.
//
// The CLI ships a typed-terminal confirmation, and labels it honestly: GradeAudit —
// deliberate, attended, permanently recorded, and NOT proof a human was present, because
// a pty an agent allocated produces the same bytes a keyboard does. An UNATTENDED
// transition cannot be authorized by an audit-grade attestation at all; it requires
// GradeVerified from a verifier rooted outside this store. There is deliberately no --yes.
//
// An external receipt pins bytes by digest. A DIGEST IS INTEGRITY, NOT AUTHENTICITY: it
// proves the artifact did not change and says nothing about who wrote it — an agent can
// write a file and hash it. It is evidence for a human or a verifier to weigh, never an
// authorization on its own, and an earlier revision's rule that a receipt sufficed for an
// unattended seizure reduced the control to "the agent must write a file first".
//
// TRANSCRIPTS ARE OPTIONAL BY CONTRACT. Delete every artifact and every projection is
// bit-identical (TestTranscriptDeletionDoesNotAffectProjections).
//
// TORN TAILS ARE REPAIRABLE; TAMPERING IS NOT; AND THE REPAIR IS ATOMIC. Replay always
// returns the valid PREFIX, so a crash that tore the last append never hides the history
// before it. Repair truncates ONLY a torn final append — mid-log damage, or a complete
// record that does not chain, fails closed, because a tool that silently truncated those
// would delete the evidence of tampering and call it a repair.
//
// It quarantines the exact discarded bytes first, durably, then swaps in the valid prefix
// PLUS an already-authorized degraded receipt as ONE atomic rename. The earlier
// truncate-then-append shape had two durable writes, and a crash in the window between
// them left the journal SHORTER with nothing in it saying why — bit-for-bit
// indistinguishable from one somebody edited. Reporting that loudly does not help: in the
// crash case there is nobody to report to. The observable journal is now either the old
// corrupt bytes or the repaired-and-receipted bytes, at every instant, for every observer.
//
// AN OPERATION THAT COMMITTED SAYS SO. The journal is the authority; everything else this
// package writes is derived. When the append lands and the work AFTER it fails, the
// operation returns ErrCommitted — carrying the committed seq and epoch, and the remedy —
// rather than a bare error a caller would reasonably RETRY, appending the same seat event
// twice and fencing the tenure it just won.
//
// This covers the REPAIR too, which is where the previous revision still had it wrong. The
// atomic rename IS the commit: once it returns, the repaired-and-receipted journal is the
// journal, and nothing afterwards can un-commit it. But the failpoint and the read-back
// that follow it returned bare errors with an EMPTY RepairResult — so a caller was told the
// repair had failed, retried, found an intact journal, and was told there had never been
// anything wrong. Two false statements in sequence, the second of which ends an
// investigation. Now the result is populated at the moment of commit and every later failure
// reports ErrCommitted with the remedy that actually fits: a stale liveness cache is rebuilt
// by an idempotent Heartbeat; a repair that committed and could not be read back is not
// rebuilt by anything — go and LOOK (`steward reconcile`), which is what the error says.
//
// RECONCILE DOES NOT CLAIM TO HAVE CHECKED REALITY. Comparing a claim against the world
// needs an adapter that knows how (Observer). With none supplied, the report says
// plainly that nothing was compared — a re-read of the journal is a spellcheck, not a
// reality check.
//
// # Durability and concurrency
//
// Atomic temp+fsync+rename for every file; journal appends are fsynced; the whole
// read/decide/write cycle is serialized under a real file lock (flock on the unixes that
// are actually tested, LockFileEx on Windows). There is no no-op lock fallback: a platform
// with no locking fails every mutation closed (ErrLockUnsupported), because a lock that
// silently does nothing is worse than none — the caller believes it is protected while two
// agents interleave and both claim the seat, and the fencing epoch does not save that case
// either (both claims mint from the same replayed head, so they COLLIDE rather than
// supersede). A platform earns its place in the locking build tag by being tested, not by
// being a unix: anything unlisted fails closed rather than being assumed to work.
//
// # What this is NOT
//
// It is not pkg/handoff. Claiming the seat restores no working tree, captures no diff,
// and touches no repository (TestSeatLifecycleTouchesNoRepository). WORK is a diff; a
// SEAT is a mandate.
package steward
