// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ─── one store per seat, and the directory does not get a vote ────────────────
//
// THE HOLE THIS CLOSES. The previous revision bound each store to a scope: scope.json
// said WHOSE seat this store was, and a store carried onto another machine was refused.
// That is a real check and it stays (see bindScope). It is also only half of the
// invariant, and the missing half was the one an agent could walk through.
//
// The seat is ONE PER MACHINE-AND-ACCOUNT. scope.json enforces dir → scope: this
// directory belongs to that seat. Nothing enforced scope → dir: that seat lives in
// exactly one directory. So an agent that did not care for the seat it found simply asked
// for another one:
//
//	bashy steward --dir /tmp/mine status      # a fresh store…
//	BASHY_STEWARD_DIR=/tmp/mine bashy steward claim
//
// and got a brand-new store, which cheerfully bound ITSELF to the same scope, minted its
// own epoch ladder from an empty journal, and handed out the seat. Two stewards on one
// host, each holding epoch 1, neither able to see the other, neither fenced — because
// fencing compares epochs within one journal and there were now two. The singleton the
// whole package is built to guarantee was defeated by a flag.
//
// So the mapping is made canonical, and it is kept OUTSIDE the store it governs. A
// registry rooted inside the data directory would be exactly as escapable as the data
// directory, which is to say completely: point --dir elsewhere and you point at a
// different registry too. This one is keyed by the SCOPE DIGEST — the machine and the
// account, the things an agent cannot re-spell — and is found the same way no matter what
// directory was asked for.
//
// WHAT IT IS AND IS NOT WORTH, stated plainly, because the last revision's mistake was
// believing a filesystem check was stronger than it is. An agent with write access to the
// registry can delete the entry, just as it can delete the journal. Nothing rooted in the
// filesystem survives an attacker who owns the filesystem, and this package will not
// pretend otherwise. What the registry buys is that the singleton is now ENFORCED rather
// than merely intended: reaching a second store takes a deliberate, destructive,
// evidence-leaving act (removing the canonical binding) instead of an ordinary flag. And
// the loser of that act finds out — every mutation revalidates under the canonical lock,
// so a steward whose scope has been rebound elsewhere is refused at its next write rather
// than journaling into an orphan.

// registryEntry is the canonical record: THIS seat lives in THAT directory.
//
// Dir is stored canonicalized (absolute, symlinks resolved), because the comparison is the
// whole point and "~/.bashy/steward/x", "./x", and "/private/var/…" vs "/var/…" are all
// the same directory wearing different spellings.
type registryEntry struct {
	SchemaVersion string    `json:"schema_version"`
	Scope         string    `json:"scope"`  // the readable seat key
	Digest        string    `json:"digest"` // sha256 over (machine, account) — the real identity
	Dir           string    `json:"dir"`    // THE one store directory for this seat
	Host          string    `json:"host"`   // a label, for the error message
	Source        string    `json:"source"` // where the machine identity came from
	BoundAt       time.Time `json:"bound_at"`
}

// ErrScopeDirConflict is returned when a process asks to open a store for a seat that
// already has one, somewhere else.
//
// This is the flag-shaped escape closed. It fires for --dir, for $BASHY_STEWARD_DIR, and
// for a plain Open("/somewhere/else") alike, because all three arrive at the same place:
// a second store for a scope that is only allowed to have one.
type ErrScopeDirConflict struct {
	Scope     string
	Canonical string // where the seat actually lives
	Requested string // where the caller asked to put a second one
	Registry  string // the registry entry that says so
}

func (e *ErrScopeDirConflict) Error() string {
	return fmt.Sprintf("steward: this seat already has a store, and it is not the one you asked for.\n"+
		"  seat:       %s\n"+
		"  its store:  %s\n"+
		"  you asked:  %s\n"+
		"The seat is ONE per machine-and-account — one journal, one epoch ladder, one steward. Opening a second store "+
		"for it would not give you a second view of the same seat; it would give you a SECOND SEAT, with its own empty "+
		"journal and its own epoch 1, unable to see the first and unable to be fenced by it. That is the exact failure "+
		"the singleton exists to prevent, and --dir is not a licence to cause it.\n"+
		"To read or write this seat, use its store (drop --dir/$BASHY_STEWARD_DIR, or point them at %s).\n"+
		"If the seat's store genuinely MOVED, say so deliberately: the binding is at %s — remove it and the next open "+
		"rebinds the seat to wherever you point it. Do that only when you know the old store is gone, because that "+
		"judgement is the entire reason this check exists.",
		e.Scope, e.Canonical, e.Requested, e.Canonical, e.Registry)
}

// ErrRegistryUnreadable is returned when the canonical binding exists but cannot be read.
// It FAILS CLOSED: a store that cannot find out whether its seat already lives somewhere
// else must not assume it does not.
type ErrRegistryUnreadable struct {
	Path  string
	Cause error
}

func (e *ErrRegistryUnreadable) Error() string {
	return fmt.Sprintf("steward: the seat registry at %s is unreadable: %v.\n"+
		"Refusing to open. This file is what says whether this seat already has a store somewhere else, and a store that "+
		"cannot answer that question must not guess: guessing wrong mints a second steward on a host that already has one.",
		e.Path, e.Cause)
}

func (e *ErrRegistryUnreadable) Unwrap() error { return e.Cause }

// registryRootName is the registry's home under the steward dir. It sits ALONGSIDE the
// per-scope store directories, never inside one — a registry a store contains is a
// registry that store can escape.
const registryRootName = "registry"

// accountHomeFn resolves the OS account's home directory. It is a var for exactly one
// reason — the test suite pins it to a temp directory so a test that forgets to inject a
// registry root cannot write a binding into the developer's real home — and production
// never reassigns it. It is NOT a seam an agent can reach: setting a package variable
// requires already being inside this process's code, at which point the filesystem argument
// below applies and nothing here would have saved you anyway.
var accountHomeFn = accountHome

// ErrNoAccountHome is returned when the OS cannot say where this account's home is. It
// FAILS CLOSED, and the fallback it refuses is the whole point.
//
// The two tempting fallbacks are $HOME (the hole being closed) and a temp directory — and
// os.TempDir is $TMPDIR, which is the same hole wearing a different variable. Any root a
// process can move is a root that yields a FRESH, EMPTY registry on demand, and an empty
// registry is a licence to mint a second seat: no binding found, so bind mine, so epoch 1,
// so two stewards. A registry that can be relocated by the thing it governs is not a
// registry, so when the OS has no answer this refuses to invent one.
type ErrNoAccountHome struct{ Cause error }

func (e *ErrNoAccountHome) Error() string {
	return fmt.Sprintf("steward: cannot establish this OS account's home directory from the operating system: %v.\n"+
		"The canonical seat registry is rooted there, and it is rooted in the OS ACCOUNT rather than in $HOME/%%USERPROFILE%% "+
		"on purpose: an environment variable is a suggestion the process can rewrite, and a registry the agent can point "+
		"somewhere new is one it can always find empty — which is precisely how a machine that already has a steward acquires "+
		"a second one. Refusing to guess a root rather than guessing one an agent could have chosen.\n"+
		"A host that keeps its state somewhere the OS cannot name must say so in-process, with WithRegistryRoot — a trusted "+
		"hook, deliberately not an env var and not a flag.", e.Cause)
}

func (e *ErrNoAccountHome) Unwrap() error { return e.Cause }

// defaultRegistryRoot is <os-account-home>/.bashy/steward/registry.
//
// THE HOME COMES FROM THE OS, NOT FROM THE ENVIRONMENT — the passwd record for the real uid
// on unix, the access token's profile directory on windows (see accountHome). os.UserHomeDir
// would have been the obvious call and it is exactly wrong here: it returns $HOME (or
// %USERPROFILE%), which the process whose singleton this enforces can set. Paired with
// $BASHY_STEWARD_DIR, that was a two-variable escape from the registry itself —
//
//	HOME=/tmp/other BASHY_STEWARD_DIR=/tmp/other/store bashy steward claim
//
// — a pristine registry, no binding in it, a fresh store bound to the same scope, its own
// epoch 1, and a second steward on a host that already had one. The registry closed the
// --dir door while leaving its own root reachable through the same kind of knob.
//
// The account home may well be SHARED between machines (an NFS home), and that remains fine:
// entries are keyed by the scope digest, which includes the machine identity. Two machines
// mounting one home get two keys, two entries, two canonical stores. The shared-home
// isolation scope.go establishes is preserved here rather than undone by it.
func defaultRegistryRoot() (string, error) {
	home, _, err := accountHomeFn()
	if err != nil {
		return "", &ErrNoAccountHome{Cause: err}
	}
	return filepath.Join(home, ".bashy", "steward", registryRootName), nil
}

// regRoot is the resolved root. Open resolves it once — from WithRegistryRoot if the host
// injected one, else from the OS account — and fails if it cannot, so by the time any of
// this runs it is never empty.
func (s *Store) regRoot() string { return s.registryRoot }

// registryKey is the entry's filename, derived from the scope DIGEST — the machine and the
// account. Never the host label (a machine can be renamed), never the directory (that is
// the thing being governed).
func registryKey(sc Scope) string {
	h := strings.TrimPrefix(sc.Digest(), "sha256:")
	if len(h) > 32 {
		h = h[:32]
	}
	return h
}

func (s *Store) registryPath() string {
	return filepath.Join(s.regRoot(), registryKey(s.scope)+".json")
}

func (s *Store) registryLockPath() string {
	return filepath.Join(s.regRoot(), registryKey(s.scope)+".lock")
}

// withRegistryLock serializes everything that reads or writes this seat's canonical
// binding — across processes, and independently of any store directory.
//
// It is THE lock for the scope, and that is why first-bind cannot race: two processes
// starting simultaneously with different --dir values both arrive here, one wins the lock,
// writes the binding, and the other reads it and is refused. Without this, both would find
// no entry, both would write one, and the last writer would silently decide which of two
// live stewards was real.
func (s *Store) withRegistryLock(fn func() error) error {
	root := s.regRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("steward: seat registry dir: %w", err)
	}
	f, err := os.OpenFile(s.registryLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("steward: seat registry lock: %w", err)
	}
	defer f.Close()
	unlock, err := lockAcquire(f)
	if err != nil {
		return fmt.Errorf("steward: seat registry lock: %w", err)
	}
	defer unlock()
	return fn()
}

// withRegistryLockOpen is withRegistryLock for Open, which must still work on a platform
// that cannot lock at all.
//
// The seat fails closed on such a platform (ErrLockUnsupported, see lock.go) — but it
// fails closed for MUTATIONS. Reads keep working, and refusing to open the store would
// take those away too, which would turn "you cannot hold a seat here" into "you cannot
// read the record here" for no gain: an unserialized bind is harmless when nothing on the
// platform is permitted to write.
func (s *Store) withRegistryLockOpen(fn func() error) error {
	err := s.withRegistryLock(fn)
	if errors.Is(err, ErrLockUnsupported) {
		return fn()
	}
	return err
}

// bindRegistry establishes — or enforces — the ONE directory this seat lives in.
//
// Called under the canonical lock, both at Open and again before every mutation. It is
// idempotent: the seat's own store passes through it untouched, and only a SECOND store
// for the same seat is refused.
func (s *Store) bindRegistry() error {
	path := s.registryPath()

	var e registryEntry
	found, err := readJSON(path, &e)
	if err != nil {
		return &ErrRegistryUnreadable{Path: path, Cause: err}
	}
	if found && e.Digest == s.scope.Digest() {
		canon, cerr := canonicalDir(e.Dir)
		if cerr != nil {
			canon = e.Dir
		}
		if canon == s.dir {
			return nil // this IS the seat's store
		}
		return &ErrScopeDirConflict{
			Scope:     s.scope.ID,
			Canonical: canon,
			Requested: s.dir,
			Registry:  path,
		}
	}
	if found && e.Digest != s.scope.Digest() {
		// The key is the digest, so the only way to land here is a hand-edited file. Refuse:
		// a binding that does not describe the seat it is filed under cannot be reasoned about.
		return &ErrRegistryUnreadable{
			Path: path,
			Cause: fmt.Errorf("it is filed under this seat's key but describes another seat (%s) — "+
				"it has been edited by hand", e.Scope),
		}
	}

	// No binding yet: this store becomes the seat's one store. Under the canonical lock, so
	// a racing process finds this entry rather than writing a competing one.
	return writeJSONAtomic(path, registryEntry{
		SchemaVersion: SchemaVersion,
		Scope:         s.scope.ID,
		Digest:        s.scope.Digest(),
		Dir:           s.dir,
		Host:          s.scope.Host,
		Source:        s.scope.Source,
		BoundAt:       time.Now().UTC(),
	})
}

// revalidateBindings re-establishes BOTH halves of the seat's identity, under the
// canonical lock, immediately before a mutation.
//
//	registry (scope → dir)   this seat still lives here, and not somewhere else
//	scope.json (dir → scope) this store still belongs to this seat
//
// Open checks both once. A Store handle outlives that check — an agent that opened
// legitimately, then rebound the scope to a directory of its own, would otherwise keep
// writing through a handle whose authority was established against a world that has since
// changed. Re-taking both checks at the moment of the write is what makes them hold.
func (s *Store) revalidateBindings() error {
	if err := s.bindRegistry(); err != nil {
		return err
	}
	return s.bindScope()
}

// RegistryPath reports where this seat's canonical binding lives. For `steward scope` and
// for whoever has to go and look at it.
func (s *Store) RegistryPath() string { return s.registryPath() }
