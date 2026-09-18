// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// clockSkew is how far a timestamp may run ahead of our clock before we stop
// believing it. Two minutes: enough for NTP drift between processes on one host,
// far too little to hide a heartbeat forged into the future.
const clockSkew = 2 * time.Minute

// MaxTranscriptBytes bounds a transcript artifact. A transcript is a courtesy
// artifact that nothing derives from (see Store.Transcript), so it gets a hard
// ceiling: an unbounded read from an agent-supplied stream is a way to fill the
// human's disk with something no projection will ever look at.
const MaxTranscriptBytes int64 = 8 << 20

// MaxReceiptBytes bounds an external authorization receipt artifact.
const MaxReceiptBytes int64 = 1 << 20

// Store is the on-disk steward state: one journal (the authority), one seat file
// (liveness only), and directories of derived or optional artifacts.
type Store struct {
	dir string
	// scope is the machine/account this store speaks for. It is stamped into grants so
	// a capability minted for one seat cannot be replayed against another, and it is
	// BOUND into the store (scope.json) so a store that travels to another machine is
	// refused rather than adopted. See scope.go.
	scope Scope
	// scopeProvider resolves that identity. Injectable so the isolation can be tested.
	scopeProvider ScopeProvider
	// verifier is the injected root of trust for AUTHORITY. Nil means every authority
	// transition fails closed — see verifier.go, which is the argument for why that is right.
	verifier Verifier
	// vverifier is the injected root of trust for PROMOTION. Nil means no verification
	// ever promotes a claim to verified: the check is recorded, the board says asserted.
	// See verification.go.
	vverifier VerificationVerifier
	// registryRoot is where the canonical scope→store registry lives. It is INDEPENDENT
	// of dir on purpose: a registry kept inside the store it governs could be escaped by
	// pointing --dir somewhere else, which is the entire hole it exists to close. See
	// registry.go. Open resolves it once — WithRegistryRoot if a host injected one, else
	// the OS ACCOUNT's own home (never $HOME, see defaultRegistryRoot) — and refuses to
	// open if it cannot, so it is non-empty for the life of the Store.
	registryRoot string
	// maxTranscript bounds transcript artifacts; overridable for tests.
	maxTranscript int64
}

// Option configures a Store.
type Option func(*Store)

// WithMaxTranscriptBytes overrides the transcript ceiling.
func WithMaxTranscriptBytes(n int64) Option {
	return func(s *Store) {
		if n > 0 {
			s.maxTranscript = n
		}
	}
}

// WithVerifier injects the root of trust for authority transitions (claim, takeover).
//
// This is the integration hook. A host with a channel the agent cannot write into —
// bashy meet, a desktop confirmation, an approval service, a signature it can check —
// implements Verifier and passes it here. WITHOUT IT, NO AUTHORITY TRANSITION IS
// POSSIBLE: the store fails closed, because a capability that rests only on store state
// is one the agent can mint by writing a file. See verifier.go.
func WithVerifier(v Verifier) Option {
	return func(s *Store) { s.verifier = v }
}

// WithVerificationVerifier injects the root of trust for PROMOTION — the thing that
// decides whether a verification may turn a strand VERIFIED on the board.
//
// Same argument as WithVerifier, applied to the other half of the package. A host that can
// actually establish whether work came true — a CI adapter, a git adapter, a signing
// service the agent holds no key for — implements VerificationVerifier and passes it here.
// WITHOUT IT, NOTHING IS EVER PROMOTED: checks are recorded in full, and the board reports
// them as asserted, which is what an unverified claim is. See verification.go.
func WithVerificationVerifier(v VerificationVerifier) Option {
	return func(s *Store) { s.vverifier = v }
}

// WithRegistryRoot overrides where the canonical scope→store registry lives.
//
// The registry is what enforces ONE STORE PER OS SCOPE no matter what directory was asked
// for (see registry.go), so its location must not be reachable from the same knobs the
// data dir is — there is deliberately no env var and no flag for it, and the default is
// rooted in the OS account's home rather than in $HOME/%USERPROFILE% for the same reason: a
// root the governed process can move is a root it can always find EMPTY, and an empty
// registry hands out a second seat. This hook is IN-PROCESS and therefore trusted: reaching
// it means being the host, not being an agent the host runs.
//
// It exists for two callers: TESTS, which need it hermetic rather than rooted in the
// developer's real home, and an EMBEDDER — one migrating a host's stores, or one whose state
// lives somewhere the OS cannot name (a container with no account record, where
// defaultRegistryRoot fails closed with ErrNoAccountHome) — which needs to say where the
// registry actually is.
func WithRegistryRoot(dir string) Option {
	return func(s *Store) {
		if dir != "" {
			s.registryRoot = dir
		}
	}
}

// WithScopeProvider overrides how the seat's identity is resolved.
//
// The default reads the OS: a stable machine id and the process's real OS account. This
// hook exists so those isolation properties can be TESTED — two machines sharing a
// hostname, one machine under two accounts — none of which is reachable by setting an
// environment variable any more, which was the entire point of removing them.
func WithScopeProvider(p ScopeProvider) Option {
	return func(s *Store) {
		if p != nil {
			s.scopeProvider = p
		}
	}
}

// WithScope pins the seat identity to a fixed id. Tests use it to prove that a grant
// minted for one seat is refused by another.
func WithScope(id string) Option {
	return WithScopeProvider(StaticScope(Scope{
		ID: id, Machine: "static:" + id, Account: "static:" + id, Host: id, Source: "static",
	}))
}

// Open prepares the store directory. The journal records what an agent did across
// every project on the host, so it is owner-only (0700/0600) like the audit log.
//
// It resolves the seat's identity and then enforces the singleton TWICE, because one
// check was not enough and the gap between them was a hole:
//
//	REGISTRY (scope → dir)  a canonical, per-OS-scope record, kept OUTSIDE any store, of
//	                        the ONE directory this seat lives in. It is what stops --dir,
//	                        $BASHY_STEWARD_DIR, or Open("/tmp/elsewhere") from minting a
//	                        SECOND steward for a scope that already has one. See registry.go.
//	BINDING (dir → scope)   the store's own record of whose seat it is, so a store carried
//	                        onto another machine in a synced home or a restored backup is
//	                        refused rather than adopted. See scope.go.
//
// The previous revision had only the second. It made every store say which seat it
// belonged to, and then let an agent open as many of them as it liked: `--dir /tmp/mine`
// bound a fresh store to the very same scope, and the host now had two journals, two
// epoch ladders, and two stewards that could not see each other — the exact failure the
// seat exists to prevent, reached by a flag.
//
// Both binds happen under ONE canonical lock, so two processes racing to first-bind a
// scope cannot both win.
func Open(dir string, opts ...Option) (*Store, error) {
	s := &Store{maxTranscript: MaxTranscriptBytes, scopeProvider: OSScope{}}
	for _, o := range opts {
		o(s)
	}

	sc, err := s.scopeProvider.Scope()
	if err != nil {
		return nil, err
	}
	s.scope = sc

	// The canonical registry root, resolved ONCE and never from the environment. A host may
	// inject it (WithRegistryRoot); otherwise it comes from the OS account's own home — not
	// $HOME, which the process being governed can set, and which paired with
	// $BASHY_STEWARD_DIR was an escape from the registry itself. See defaultRegistryRoot.
	if s.registryRoot == "" {
		if s.registryRoot, err = defaultRegistryRoot(); err != nil {
			return nil, err
		}
	}

	if dir == "" {
		if v := os.Getenv("BASHY_STEWARD_DIR"); v != "" {
			dir = v
		} else if dir, err = defaultDirFor(sc); err != nil {
			return nil, err
		}
	}
	// Create the directory and canonicalize it IN THAT ORDER, and take the canonical form
	// from the directory that now exists. See makeCanonicalDir: resolving a path before
	// creating it is what bound a brand-new seat to a spelling it would refuse one write later.
	if s.dir, err = makeCanonicalDir(dir); err != nil {
		return nil, err
	}
	if err := s.withRegistryLockOpen(s.revalidateBindings); err != nil {
		return nil, err
	}
	return s, nil
}

// makeCanonicalDir creates the store directory and returns the ONE string everything else
// compares it by. Creation comes first, and the canonical form is read back off the created
// directory, because the two orders do not agree and only this one is stable.
//
// THE HOLE THIS CLOSES — the first store a seat ever had could not perform its first write.
// filepath.EvalSymlinks fails on a path that does not exist, so canonicalizing BEFORE the
// mkdir left the unresolved spelling: on macOS, where a temp dir hands you /var/… and /var
// is a symlink to /private/var, a brand-new store bound its seat to
//
//	/var/folders/…/store         (the leaf did not exist, so nothing resolved)
//
// and then the mkdir made it exist. The very next mutation revalidates the binding
// (revalidateBindings), canonicalizes the recorded dir — which now resolves, because the
// directory is there — and compares /private/var/…/store against the /var/…/store this Store
// still held. Different strings, so ErrScopeDirConflict: the seat's one and only store,
// refused against its OWN binding, telling the operator to go and use the store it was
// already using. Every host whose steward dir sits under a symlinked parent (macOS $TMPDIR,
// a /home → /system/home layout, a bind-mounted state dir) hit it on the FIRST claim it ever
// made — which is exactly the path with no prior store to fall back to.
//
// Canonicalizing after the mkdir makes the answer independent of when it is asked: the
// directory exists, EvalSymlinks resolves the whole path, and the string bound into the
// registry at Open is the string every later revalidation derives. A leaf that is itself a
// symlink resolves the same way, which is the point — one directory, one spelling, however
// it was reached.
//
// It also FAILS rather than falling back to the unresolved path. A fallback is what produced
// the defect: it does not avoid the mismatch, it merely postpones it to the first write, and
// a store that cannot say which directory it is cannot be held to living in exactly one.
func makeCanonicalDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("steward: store dir %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", fmt.Errorf("steward: store dir: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("steward: cannot resolve the store dir %q to a canonical path: %w — "+
			"refusing to open, because the seat is one-per-directory and a store that cannot say WHICH "+
			"directory it is cannot be held to that", abs, err)
	}
	return resolved, nil
}

// canonicalDir is makeCanonicalDir's read-only half: the canonical form of a path this
// process does not own and must not create — the directory RECORDED in a registry entry,
// which may since have been deleted, or may never have existed.
//
// It must agree with makeCanonicalDir on a path that exists, and it must not change its
// answer when one stops existing. So it resolves the deepest ancestor that DOES exist and
// re-attaches the components below it: /var/…/gone/store canonicalizes to
// /private/var/…/gone/store whether or not the leaf is still there. A plain Abs fallback
// would answer /var/…/store for a deleted store and /private/var/…/store for a live one —
// two answers for one directory, which in a registry is a spurious conflict (a seat refused
// against its own binding because its store was removed) or, worse, a missed one.
//
// It carries the compatibility case too: a binding written by the version that had the
// defect records the unresolved spelling, and this resolves it to the same canonical string
// the store now holds, so those seats keep working with no migration.
func canonicalDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("steward: store dir %q: %w", dir, err)
	}
	// Walk up to the deepest existing ancestor, resolving it, then re-attach the tail. The
	// loop terminates at the volume root ("/" on unix, `C:\` on windows), where Dir(p) == p.
	var tail []string
	for p := abs; ; {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return abs, nil // nothing along the path resolves; the spelling is all we have
		}
		tail = append(tail, filepath.Base(p))
		p = parent
	}
}

func (s *Store) Dir() string       { return s.dir }
func (s *Store) Scope() string     { return s.scope.ID }
func (s *Store) ScopeInfo() Scope  { return s.scope }
func (s *Store) HasVerifier() bool { return s.verifier != nil }

// ─── committed-but-incomplete ─────────────────────────────────────────────────

// ErrCommitted reports an operation whose JOURNAL APPEND SUCCEEDED and whose follow-up
// housekeeping did not.
//
// This exists because the alternative is silent corruption of the caller's beliefs. The
// journal is the authority; everything else this package writes — the seat.json liveness
// cache, the spent-grant marker, the removal of a released seat file — is derived. So
// when the append lands and the cache write then fails, returning a bare error tells the
// caller "your claim failed", and a caller that believes that RETRIES. The retry replays
// against a journal that already contains the claim: at best it is refused confusingly,
// at worst it appends a second seat event and mints a second epoch, fencing the very
// tenure the first call successfully acquired.
//
// So the operation reports what actually happened: IT COMMITTED. The seq and epoch are
// carried so the caller can proceed as the holder it now is, and the message says which
// derived artifact is stale and how to rebuild it — which, for the seat cache, is a
// plain `steward heartbeat --epoch N`, an idempotent operation that reconstructs it from
// the journal. That is the whole recovery.
//
// Callers that only want to know "did this work" can keep using errors.Is/As; callers
// that must not retry check for this type. The journal is fine either way — which is the
// point of putting the authority in exactly one place.
type ErrCommitted struct {
	Op    string // "claim", "takeover", "record", "release", "repair", …
	Seq   uint64 // the journal seq that WAS committed (0 if the op appends nothing)
	Epoch uint64 // the epoch it was committed under
	Cause error  // what failed AFTER the commit

	// Remedy says how to rebuild whatever derived state is stale, in the caller's own
	// terms. It is a FIELD rather than a fixed sentence because the operations that can
	// reach this state fail in different places and are recovered differently: a stale
	// liveness cache is rebuilt by a heartbeat, whereas a repair that committed and then
	// could not be read back needs a human to look at the journal, not a heartbeat. The
	// previous revision printed the heartbeat advice unconditionally, which for a repair
	// was confident, specific, and wrong.
	Remedy string
}

// remedyHeartbeat is the recovery for the common case: the append landed, the derived
// seat.json did not. A heartbeat reconstructs it from the journal and is idempotent.
func remedyHeartbeat(epoch uint64) string {
	return fmt.Sprintf("What is stale is the liveness cache, which is derived and rebuildable: run "+
		"`steward heartbeat --epoch %d`, which reconstructs it from the journal and is safe to repeat.", epoch)
}

func (e *ErrCommitted) Error() string {
	s := fmt.Sprintf("steward: %s WAS COMMITTED to the journal (seq %d, epoch %d) — but the work that follows it did "+
		"not complete: %v.\n"+
		"DO NOT RETRY IT. The journal is the authority and it already holds this operation; retrying would append it a "+
		"second time.", e.Op, e.Seq, e.Epoch, e.Cause)
	if e.Remedy != "" {
		s += "\n" + e.Remedy
	}
	return s
}

func (e *ErrCommitted) Unwrap() error { return e.Cause }

// Committed reports the seq and epoch that reached the journal.
func (e *ErrCommitted) Committed() (seq, epoch uint64) { return e.Seq, e.Epoch }

// committed wraps a post-append failure, or returns nil if there was none.
func committed(op string, seq, epoch uint64, err error) error {
	return committedWith(op, seq, epoch, err, remedyHeartbeat(epoch))
}

// committedWith is committed with an operation-specific recovery.
func committedWith(op string, seq, epoch uint64, err error, remedy string) error {
	if err == nil {
		return nil
	}
	return &ErrCommitted{Op: op, Seq: seq, Epoch: epoch, Cause: err, Remedy: remedy}
}

// failpoint is a test hook for crash simulation, and it is a no-op in production.
//
// The durability arguments in this package — a repair is atomic, a commit is either
// visible or not — are claims about what happens when the process dies at the worst
// possible instant. Those instants are unreachable from an ordinary test, so the code
// names them, and the tests kill the process there. A named failpoint that nothing can
// trigger in production is a cheap price for a durability property that is actually
// exercised rather than merely asserted in a comment.
var failpoint = func(string) error { return nil }

func (s *Store) journalPath() string   { return filepath.Join(s.dir, "journal.jsonl") }
func (s *Store) seatPath() string      { return filepath.Join(s.dir, "seat.json") }
func (s *Store) lockPath() string      { return filepath.Join(s.dir, "steward.lock") }
func (s *Store) checkpointDir() string { return filepath.Join(s.dir, "checkpoints") }
func (s *Store) transcriptDir() string { return filepath.Join(s.dir, "transcripts") }
func (s *Store) grantDir() string      { return filepath.Join(s.dir, "grants") }
func (s *Store) receiptDir() string    { return filepath.Join(s.dir, "receipts") }
func (s *Store) quarantineDir() string { return filepath.Join(s.dir, "quarantine") }

// withLock serializes an entire read/decide/write cycle.
//
// The lock is essential exactly here: Claim must READ the journal, decide the seat
// is free, and WRITE its claim — and if two agents interleave those three steps,
// both conclude the seat is vacant and both take it. That is the race the singleton
// contract exists to forbid, reproduced inside the mechanism meant to enforce it.
//
// There is no no-op fallback. On a platform with no file locking, lockFile returns
// ErrLockUnsupported and every MUTATION fails closed with it; reads, which never
// take the lock, keep working. A lock that silently does nothing is worse than no
// lock, because the caller believes it is protected: it converts "this platform
// cannot host a steward" into "this platform hosts two stewards and neither knows".
// lockAcquire is the lock withLock takes. It is a var so a test can simulate a
// platform with no file locking and prove that every mutation there fails CLOSED
// rather than proceeding unserialized (TestUnsupportedLockFailsEveryMutationClosed).
var lockAcquire = lockFile

// withLock serializes a mutation, and REVALIDATES the seat's bindings first.
//
// Two locks, always in this order — canonical registry, then store — so two mutations can
// never deadlock by taking them in opposite orders.
//
// The revalidation is the part that is easy to leave out and expensive to omit. Open()
// checks the bindings once, and a Store is a long-lived object: an agent that opened
// legitimately and then rewrote the registry (or the store's scope.json, or moved the
// directory out from under itself) would keep writing through a handle whose authority was
// checked minutes ago against a world that no longer exists. So every mutation re-reads
// both bindings UNDER THE CANONICAL LOCK, where nothing can change them while it looks. A
// check that is not re-taken at the moment of the write is a check that races.
func (s *Store) withLock(fn func() error) error {
	return s.withRegistryLock(func() error {
		if err := s.revalidateBindings(); err != nil {
			return err
		}
		f, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return fmt.Errorf("steward: lock: %w", err)
		}
		defer f.Close()
		unlock, err := lockAcquire(f)
		if err != nil {
			return fmt.Errorf("steward: lock: %w", err)
		}
		defer unlock()
		return fn()
	})
}

// Replay walks the journal and returns the valid prefix plus an honest account of
// any unreadable tail. This is the ONLY way state is ever derived: every view in
// this package is a pure function of what Replay returns.
func (s *Store) Replay() (*Replay, error) { return readJournal(s.journalPath()) }

// writeJSONAtomic writes v to path atomically and durably: a reader — possibly a
// successor mid-recovery — sees either the whole previous file or the whole new
// one, never a half-written blend, and a rename the OS acknowledged survives a
// crash.
func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeBytesAtomic(path, append(b, '\n'))
}

// writeBytesAtomic is writeJSONAtomic for raw content.
func writeBytesAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	// fsync the DATA before the rename: a rename that lands while the contents are
	// still in the page cache can leave a correctly-named, empty file after a crash.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// fsync the DIRECTORY so the rename itself is durable, not just the bytes.
	// Best-effort: some filesystems refuse to open a directory for sync, and a
	// missing directory fsync is a weaker guarantee, not a wrong one.
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func readJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

// mustUTC normalizes a caller-supplied clock. Every timestamp this package writes
// is UTC: a journal that outlives the session that wrote it must not be readable
// only in the timezone it happened to be born in.
func mustUTC(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t.UTC()
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// jsonUnmarshalStrict rejects trailing garbage as well as bad syntax, so "is this a
// complete record?" cannot be answered yes by a fragment that merely starts like one.
func jsonUnmarshalStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data after the JSON value")
	}
	return nil
}
