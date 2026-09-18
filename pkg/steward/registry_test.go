// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─── ONE STORE PER SEAT, AND THE DIRECTORY DOES NOT GET A VOTE ────────────────
//
// The seat is one per machine-and-account. scope.json enforced HALF of that — this
// directory belongs to that seat — and the previous revision stopped there, which left the
// other half open: nothing said that seat lives in exactly ONE directory.
//
// So an agent that did not care for the steward it found simply asked for another one, and
// the store it got cheerfully bound ITSELF to the same scope, minted its own epoch ladder
// from an empty journal, and handed over the seat. Two stewards on one host, each holding
// epoch 1, neither able to see the other, neither fenced — because fencing compares epochs
// WITHIN one journal and there were now two. The singleton the whole package exists to
// guarantee was defeated by a flag.
//
// These tests come at that from every direction an agent actually has: the API, the flag,
// the environment variable, a race, and a store handle that was legitimate a moment ago.

// The API route: a second Open, for the same seat, somewhere else.
func TestASecondStoreForTheSameSeatIsRefused(t *testing.T) {
	reg := t.TempDir()
	canonical, alternate := t.TempDir(), t.TempDir()

	if _, err := Open(canonical, WithScopeProvider(testScope("seat")), WithRegistryRoot(reg)); err != nil {
		t.Fatalf("the first store for a seat becomes its store: %v", err)
	}

	_, err := Open(alternate, WithScopeProvider(testScope("seat")), WithRegistryRoot(reg))
	var conflict *ErrScopeDirConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("a SECOND store for a seat that already has one must be refused — it would not be a second view "+
			"of the same seat, it would be a second SEAT, with its own empty journal and its own epoch 1. Got %v", err)
	}
	// Compare CANONICAL forms: macOS hands out temp dirs under /var, which is a symlink to
	// /private/var, and the store resolves that (see canonicalDir) precisely so two spellings
	// of one directory cannot become two seats.
	if want := mustCanonical(t, canonical); conflict.Canonical != want {
		t.Fatalf("the refusal must name where the seat actually lives, got %q want %q", conflict.Canonical, want)
	}
	if !strings.Contains(err.Error(), mustCanonical(t, alternate)) {
		t.Fatalf("…and what was asked for, so the operator can see the difference: %v", err)
	}

	// The seat's own store still opens, of course. The check is a singleton, not a lock-out.
	if _, err := Open(canonical, WithScopeProvider(testScope("seat")), WithRegistryRoot(reg)); err != nil {
		t.Fatalf("the seat's own store must keep opening: %v", err)
	}
}

// The environment route. $BASHY_STEWARD_DIR still SELECTS a directory — a host with a
// mounted volume needs that — but it does not get to mint a second seat with it.
func TestStewardDirEnvCannotMintASecondSeat(t *testing.T) {
	reg := t.TempDir()
	canonical, alternate := t.TempDir(), t.TempDir()

	if _, err := Open(canonical, WithScopeProvider(testScope("seat")), WithRegistryRoot(reg)); err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Setenv("BASHY_STEWARD_DIR", alternate)
	_, err := Open("", WithScopeProvider(testScope("seat")), WithRegistryRoot(reg))

	var conflict *ErrScopeDirConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("$BASHY_STEWARD_DIR is a way to SAY WHERE the seat lives, not a way to have a second one. Got %v", err)
	}

	// And pointed at the seat's real store, it is honored exactly as before.
	t.Setenv("BASHY_STEWARD_DIR", canonical)
	if _, err := Open("", WithScopeProvider(testScope("seat")), WithRegistryRoot(reg)); err != nil {
		t.Fatalf("pointing the variable AT the seat's store must still work: %v", err)
	}
}

// The flag route, through the CLI the agent actually types at.
func TestCLIDirFlagCannotMintASecondSeat(t *testing.T) {
	reg := t.TempDir()
	canonical, alternate := t.TempDir(), t.TempDir()

	run := func(dir string, args ...string) error {
		t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/tester")
		t.Setenv("BASHY_HOST_ID", "registry-test-machine")
		cmd := NewStewardCmd(WithRegistryRoot(reg))
		cmd.SetOut(new(strings.Builder))
		cmd.SetErr(new(strings.Builder))
		cmd.SetIn(strings.NewReader(""))
		cmd.SetArgs(append([]string{"--dir", dir}, args...))
		return cmd.Execute()
	}

	if err := run(canonical, "status"); err != nil {
		t.Fatalf("the first --dir binds the seat: %v", err)
	}
	err := run(alternate, "status")
	var conflict *ErrScopeDirConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("`steward --dir /tmp/mine` was the whole escape: a fresh store, bound to the same seat, with its "+
			"own epoch ladder. It must be refused. Got %v", err)
	}
}

// THE FIRST BIND IS A RACE, AND IT IS SERIALIZED.
//
// Without a lock, two processes starting together both find no binding, both write one, and
// the last writer silently decides which of two live stewards was real. The canonical lock
// is per-SCOPE and lives outside every store, so the loser reads the winner's binding rather
// than overwriting it.
func TestFirstBindIsSerializedUnderTheCanonicalLock(t *testing.T) {
	const racers = 8
	reg := t.TempDir()

	dirs := make([]string, racers)
	for i := range dirs {
		dirs[i] = t.TempDir()
	}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		won    []string
		failed int
	)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // pile up on the lock together
			s, err := Open(dirs[i], WithScopeProvider(testScope("contested")), WithRegistryRoot(reg))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				return
			}
			won = append(won, s.Dir())
		}()
	}
	close(start)
	wg.Wait()

	if len(won) != 1 {
		t.Fatalf("exactly ONE directory may become the seat's store — %d did (%v). Two winners is two journals, "+
			"two epoch ladders, and two stewards that cannot fence each other.", len(won), won)
	}
	if failed != racers-1 {
		t.Fatalf("every other racer must be refused, got %d refusals of %d", failed, racers-1)
	}
}

// A HANDLE IS NOT A PERMISSION. Open() checks the bindings once, and a Store outlives that
// check — so an agent that opened legitimately and then rebound the scope to a directory of
// its own would keep writing through a handle whose authority was established against a
// world that no longer exists.
//
// Every mutation re-reads both bindings under the canonical lock. A check that is not
// re-taken at the moment of the write is a check that races.
func TestMutationsRevalidateTheBindingUnderTheCanonicalLock(t *testing.T) {
	reg := t.TempDir()
	dir := t.TempDir()

	s, err := Open(dir, WithScopeProvider(testScope("seat")), WithVerifier(verified()), WithRegistryRoot(reg))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))

	// A write through this handle works — the handle is legitimate.
	if _, err := s.Record(evidenced(a, "api", "before"), ep, at(time.Minute)); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Now the seat is rebound elsewhere, behind this handle's back — an agent rewriting the
	// registry, or a racing process that won a rebind after the canonical dir went away.
	rebindRegistryTo(t, s, filepath.Join(t.TempDir(), "elsewhere"))

	_, err = s.Record(evidenced(a, "api", "after"), ep, at(2*time.Minute))
	var conflict *ErrScopeDirConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("a steward whose scope has been rebound elsewhere must be refused at its NEXT WRITE, rather than "+
			"journaling into an orphan nobody will ever read. Got %v", err)
	}

	// Every mutation, not just the convenient one. A revalidation that only guarded Record
	// would leave the seat itself — the thing actually worth stealing — unguarded.
	if err := s.Heartbeat(a, ep, at(3*time.Minute)); !errors.As(err, &conflict) {
		t.Fatalf("Heartbeat must revalidate too, got %v", err)
	}
	if _, err := s.Checkpoint(a, ep, "", at(3*time.Minute)); !errors.As(err, &conflict) {
		t.Fatalf("Checkpoint must revalidate too, got %v", err)
	}
	if err := s.Release(a, ep, "", at(3*time.Minute)); !errors.As(err, &conflict) {
		t.Fatalf("Release must revalidate too, got %v", err)
	}
	if _, err := s.Claim(context.Background(), a, SeatRequest{GrantID: "g", Attended: true}, at(3*time.Minute)); !errors.As(err, &conflict) {
		t.Fatalf("Claim must revalidate too, got %v", err)
	}
}

// The store's OWN binding is revalidated as well: a scope.json rewritten under a live
// handle is the same attack from the other side.
func TestMutationsRevalidateTheStoresScopeBinding(t *testing.T) {
	reg := t.TempDir()
	dir := t.TempDir()

	s, err := Open(dir, WithScopeProvider(testScope("seat")), WithVerifier(verified()), WithRegistryRoot(reg))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	a := agent("a")
	ep := mustClaim(t, s, a, at(0))

	// Rewrite the store's scope binding to somebody else's seat.
	b := scopeBinding{
		SchemaVersion: SchemaVersion,
		Scope:         "somebody-else",
		Digest:        "sha256:" + strings.Repeat("e", 64),
		Host:          "other-host",
		BoundAt:       time.Now().UTC(),
	}
	if err := writeJSONAtomic(filepath.Join(dir, "scope.json"), b); err != nil {
		t.Fatal(err)
	}

	_, err = s.Record(evidenced(a, "api", "after"), ep, at(time.Minute))
	var mismatch *ErrScopeMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("a store whose binding now names another seat must refuse the write, got %v", err)
	}
}

// SHARED-HOME MULTI-HOST ISOLATION SURVIVES ALL OF THIS.
//
// The registry lives under $HOME, which two machines may well share — and that is fine,
// because the entries are keyed by the SCOPE DIGEST, which includes the machine identity.
// Two machines mounting one home get two keys, two bindings, and two canonical stores. The
// isolation scope.go established is preserved by the registry rather than undone by it,
// and this is the test that says so.
func TestSharedHomeKeepsTwoMachinesIsolated(t *testing.T) {
	reg := t.TempDir() // ONE registry root — a shared home, exactly
	dirA, dirB := t.TempDir(), t.TempDir()

	sa, err := Open(dirA, WithScopeProvider(testScope("machine-a")), WithVerifier(verified()), WithRegistryRoot(reg))
	if err != nil {
		t.Fatalf("machine A: %v", err)
	}
	sb, err := Open(dirB, WithScopeProvider(testScope("machine-b")), WithVerifier(verified()), WithRegistryRoot(reg))
	if err != nil {
		t.Fatalf("machine B must get its OWN seat in a shared home — the registry keys on the machine, not the "+
			"home directory: %v", err)
	}

	if sa.RegistryPath() == sb.RegistryPath() {
		t.Fatal("two machines must not share one binding — that is the merged-journal failure this all exists to stop")
	}

	// And both hold their own seat, at their own epoch 1, without fencing each other.
	epA := mustClaim(t, sa, agent("a"), at(0))
	epB := mustClaim(t, sb, agent("b"), at(0))
	if epA != 1 || epB != 1 {
		t.Fatalf("each machine's seat has its own epoch ladder, got %d and %d", epA, epB)
	}
	if _, err := sa.Record(evidenced(agent("a"), "api", "A's work"), epA, at(time.Minute)); err != nil {
		t.Fatalf("machine A must keep working: %v", err)
	}
	if _, err := sb.Record(evidenced(agent("b"), "api", "B's work"), epB, at(time.Minute)); err != nil {
		t.Fatalf("machine B must keep working: %v", err)
	}
}

// The same directory, spelled differently, is the same directory. A registry that compared
// paths as strings would hand a second seat to whoever typed a trailing slash.
func TestTheSameDirSpelledDifferentlyIsTheSameStore(t *testing.T) {
	reg := t.TempDir()
	dir := t.TempDir()

	if _, err := Open(dir, WithScopeProvider(testScope("seat")), WithRegistryRoot(reg)); err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, spelling := range []string{
		dir + string(filepath.Separator),
		filepath.Join(dir, "."),
		filepath.Join(dir, "sub", ".."),
	} {
		if _, err := Open(spelling, WithScopeProvider(testScope("seat")), WithRegistryRoot(reg)); err != nil {
			t.Fatalf("%q is the same directory as %q — a path comparison that says otherwise is a second seat "+
				"waiting to happen: %v", spelling, dir, err)
		}
	}
}

// An unreadable binding FAILS CLOSED. A store that cannot find out whether its seat already
// lives somewhere else must not assume it does not: guessing wrong mints a second steward
// on a host that already has one.
func TestAnUnreadableRegistryFailsClosed(t *testing.T) {
	reg := t.TempDir()
	dir := t.TempDir()

	s, err := Open(dir, WithScopeProvider(testScope("seat")), WithRegistryRoot(reg))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := os.WriteFile(s.RegistryPath(), []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = Open(dir, WithScopeProvider(testScope("seat")), WithRegistryRoot(reg))
	var unreadable *ErrRegistryUnreadable
	if !errors.As(err, &unreadable) {
		t.Fatalf("an unreadable binding must refuse the open rather than silently rebinding, got %v", err)
	}
}

// rebindRegistryTo rewrites the seat's canonical binding to point somewhere else — what an
// agent with file access would do, and what a legitimate rebind after a move looks like from
// the inside.
func rebindRegistryTo(t *testing.T, s *Store, dir string) {
	t.Helper()
	var e registryEntry
	found, err := readJSON(s.RegistryPath(), &e)
	if err != nil || !found {
		t.Fatalf("reading the binding: found=%v err=%v", found, err)
	}
	e.Dir = dir
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.RegistryPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// mustCanonical is the path form the store actually compares. macOS temp dirs live under
// /var, which is a symlink to /private/var, so a test that asserted on the raw t.TempDir()
// string would be testing the symlink rather than the store.
func mustCanonical(t *testing.T, dir string) string {
	t.Helper()
	c, err := canonicalDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// ─── THE FIRST STORE A SEAT EVER HAS MUST BE ABLE TO WRITE ────────────────────
//
// The singleton is enforced by comparing a directory against the one the registry recorded,
// so the STRING those comparisons run on has to be the same string every time it is derived.
// It was not, and the seam was exactly where nobody looks: a store that does not exist yet.
//
// filepath.EvalSymlinks fails on a path with no directory at the end of it, and the old
// canonicalizer fell back to the unresolved spelling when it did. So a BRAND-NEW store under
// a symlinked parent — /var/… on macOS, where /var is a symlink to /private/var, which is
// where every temp dir and many a state dir lives — bound its seat to /var/…/store, and then
// created the directory. One mutation later, revalidateBindings canonicalized the recorded
// dir, which now resolved, compared /private/var/…/store against the /var/…/store the Store
// still held, and refused the write with ErrScopeDirConflict: the seat's only store, denied
// against its OWN binding, advising the operator to go and use the store it was already
// using. It fired on the first claim a host ever made — the one path with no earlier store to
// paper over it.
//
// The fix is an ordering: create, THEN canonicalize, and read the canonical form off the
// directory that now exists (makeCanonicalDir). These tests hold the property that ordering
// buys — one directory has one spelling, whenever you ask — from both sides: the new store
// works, and it is still exactly one store.

// The regression, through a genuinely symlinked parent, on the path that broke: a store
// directory that does not exist until Open makes it.
func TestAFirstStoreUnderASymlinkedParentCanTakeItsSeat(t *testing.T) {
	reg := t.TempDir()
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	symlinkOrSkip(t, real, link)

	// The store is BELOW the symlink and does not exist yet — a fresh seat on a host whose
	// state dir is reached through a symlinked parent.
	dir := filepath.Join(link, "store")

	s, err := Open(dir, WithScopeProvider(testScope("seat")), WithVerifier(verified()), WithRegistryRoot(reg))
	if err != nil {
		t.Fatalf("the first store for a seat must open: %v", err)
	}

	// It bound itself to the RESOLVED directory, not to the spelling it was asked for. This is
	// the assertion the whole defect reduces to: what goes into the registry has to be what
	// every later revalidation derives.
	want := mustCanonical(t, real) + string(filepath.Separator) + "store"
	if s.Dir() != want {
		t.Fatalf("the store must canonicalize to the directory it actually made, got %q want %q", s.Dir(), want)
	}
	var e registryEntry
	if found, err := readJSON(s.RegistryPath(), &e); err != nil || !found {
		t.Fatalf("reading the binding: found=%v err=%v", found, err)
	}
	if e.Dir != want {
		t.Fatalf("the binding recorded %q, but every mutation will canonicalize the store to %q and refuse itself "+
			"against its own entry. The seat's FIRST write is the one that fails.", e.Dir, want)
	}

	// And the write actually happens. mustClaim mints a capability (a mutation) and claims the
	// seat (another) — both go through withLock → revalidateBindings, which is where the seat
	// used to be refused against its own binding.
	ep := mustClaim(t, s, agent("a"), at(0))
	if _, err := s.Record(evidenced(agent("a"), "api", "the first thing this seat ever did"), ep, at(time.Minute)); err != nil {
		t.Fatalf("the seat's first authoritative write must land: %v", err)
	}

	// The OTHER spelling of the same directory is the same store, opened again — through the
	// symlink and through the resolved path alike.
	for _, spelling := range []string{dir, want} {
		again, err := Open(spelling, WithScopeProvider(testScope("seat")), WithVerifier(verified()), WithRegistryRoot(reg))
		if err != nil {
			t.Fatalf("%q is the seat's own store, reached by another name — it must open: %v", spelling, err)
		}
		if again.Dir() != want {
			t.Fatalf("both spellings must canonicalize to one directory, got %q want %q", again.Dir(), want)
		}
		if _, err := again.Record(evidenced(agent("a"), "api", "and again"), ep, at(2*time.Minute)); err != nil {
			t.Fatalf("…and it must be able to write: %v", err)
		}
	}

	// AND THE SINGLETON IS INTACT. The fix makes the seat's own store work; it does not hand
	// out a second one, and the not-yet-existing directory is where that would be easiest to
	// miss — the same missing-path branch, on a directory the seat does not own.
	for _, alternate := range []string{
		t.TempDir(),                         // an existing dir elsewhere
		filepath.Join(t.TempDir(), "fresh"), // one that does not exist yet
		filepath.Join(link, "store", "..", "..", "elsewhere"), // reached back out through the symlink
	} {
		_, err := Open(alternate, WithScopeProvider(testScope("seat")), WithVerifier(verified()), WithRegistryRoot(reg))
		var conflict *ErrScopeDirConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("a SECOND store for this seat must still be refused (%s), got %v", alternate, err)
		}
		if conflict.Canonical != want {
			t.Fatalf("the refusal must name the seat's real store %q, got %q", want, conflict.Canonical)
		}
	}
}

// The canonical form of a directory MUST NOT DEPEND ON WHETHER IT EXISTS. That is the
// property the defect violated, stated directly and without a store around it — and it is
// the half that runs on windows, where creating a symlink needs a privilege the test box may
// not have.
//
// It matters on both sides of the comparison. makeCanonicalDir answers for the store this
// process is opening (and creates it). canonicalDir answers for the directory the REGISTRY
// recorded, which this process must not create and which may since have been deleted — and a
// canonicalizer that gave one answer for a live directory and another for a deleted one would
// refuse a seat against its own binding the moment its store was removed.
func TestCanonicalDirIsTheSameAnswerWhetherOrNotThePathExists(t *testing.T) {
	base := t.TempDir()

	for _, tc := range []struct {
		name string
		path string
	}{
		{"a leaf that does not exist yet", filepath.Join(base, "store")},
		{"several levels that do not exist yet", filepath.Join(base, "a", "b", "c")},
		{"an unclean spelling of one", filepath.Join(base, "a", "..", "a", "b", ".")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := canonicalDir(tc.path)
			if err != nil {
				t.Fatalf("canonicalDir on a path that does not exist must still answer: %v", err)
			}
			if !filepath.IsAbs(before) {
				t.Fatalf("the canonical form is absolute, got %q", before)
			}

			made, err := makeCanonicalDir(tc.path)
			if err != nil {
				t.Fatalf("makeCanonicalDir: %v", err)
			}
			if made != before {
				t.Fatalf("creating the directory changed its canonical form (%q → %q). That is the defect: the "+
					"registry records the first answer at Open and compares the second at every write.", before, made)
			}

			after, err := canonicalDir(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if after != made {
				t.Fatalf("canonicalDir disagrees with makeCanonicalDir on a directory that EXISTS (%q vs %q) — the "+
					"two run on opposite sides of every singleton comparison", after, made)
			}

			// …and deleting it does not change the answer either, which is what keeps a seat whose
			// store was removed from being refused against its own binding.
			if err := os.RemoveAll(filepath.Join(base, "a")); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(base, "store")); err != nil {
				t.Fatal(err)
			}
			gone, err := canonicalDir(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if gone != made {
				t.Fatalf("the canonical form of a DELETED directory changed (%q → %q) — a seat whose store is gone "+
					"would be refused against its own entry", made, gone)
			}
		})
	}
}

// The same property, with a real symlink in the middle of it: the resolution has to happen
// through the parent even when the leaf below it is missing, which is the case a plain
// EvalSymlinks cannot answer at all.
func TestCanonicalDirResolvesASymlinkedParentOfAMissingDir(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	symlinkOrSkip(t, real, link)

	want := mustCanonical(t, real) + string(filepath.Separator) + "store"

	got, err := canonicalDir(filepath.Join(link, "store"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("a missing directory under a symlinked parent must resolve THROUGH the parent — the leaf is the only "+
			"part with nothing to resolve. Got %q want %q", got, want)
	}
}

// TWO PROCESSES, ONE NEW STORE, TWO SPELLINGS OF IT, AT THE SAME TIME.
//
// The first-bind race (TestFirstBindIsSerializedUnderTheCanonicalLock) proves that two
// racers wanting DIFFERENT directories cannot both win. This is its mirror, and it is the
// race the canonicalization defect actually created: racers wanting the SAME directory, by
// different names, before it exists. If the winner records a spelling the losers do not
// derive, every loser is refused from the store it correctly shares — a self-inflicted
// conflict on a seat with exactly one store.
func TestRacingOpensOfOneNewStoreAgreeOnItsSpelling(t *testing.T) {
	reg := t.TempDir()
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	symlinkOrSkip(t, real, link)

	// Half the racers say it through the symlink, half through the resolved path. Neither
	// creates it first: Open does, under the lock, whichever one gets there.
	spellings := []string{
		filepath.Join(link, "store"),
		filepath.Join(real, "store"),
		filepath.Join(link, "sub", "..", "store"),
		filepath.Join(real, "store") + string(filepath.Separator),
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		dirs []string
		errs []error
	)
	start := make(chan struct{})
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s, err := Open(spellings[i%len(spellings)], WithScopeProvider(testScope("contested")), WithRegistryRoot(reg))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			dirs = append(dirs, s.Dir())
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) != 0 {
		t.Fatalf("every racer asked for the SAME store, spelled differently — none may be refused. Got %d refusals, "+
			"first: %v", len(errs), errs[0])
	}
	want := mustCanonical(t, real) + string(filepath.Separator) + "store"
	for _, d := range dirs {
		if d != want {
			t.Fatalf("all racers must derive one canonical directory, got %q want %q", d, want)
		}
	}
}

// symlinkOrSkip creates a directory symlink, or skips: on windows it needs developer mode or
// SeCreateSymbolicLinkPrivilege, and a test box without either cannot exercise a symlink at
// all. The ordering property the symlink tests are here for is pinned WITHOUT one by
// TestCanonicalDirIsTheSameAnswerWhetherOrNotThePathExists, which runs everywhere.
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink on this box (%v) — on windows that needs developer mode or "+
			"SeCreateSymbolicLinkPrivilege", err)
	}
}

// ─── AND THE REGISTRY'S OWN ROOT DOES NOT GET A VOTE EITHER ───────────────────
//
// The registry closed the --dir door, and left its own front door reachable through the very
// same kind of knob. Its root was os.UserHomeDir — which is $HOME (%USERPROFILE% on windows),
// a string the process it governs can set. So the escape survived, at one remove and with one
// extra variable:
//
//	HOME=/tmp/other BASHY_STEWARD_DIR=/tmp/other/store bashy steward claim
//
// A registry that has never been written is a registry with no binding in it, and no binding
// means "this seat has no store yet — bind mine." Fresh store, fresh journal, epoch 1,
// second steward on a host that already had one. The registry did not fail; it was simply
// asked in a different building.
//
// So the root is taken from the OS ACCOUNT — the passwd record for the real uid, the access
// token's profile directory on windows — which is exactly as unspoofable as the uid and the
// SID the seat is already keyed on. These tests come at it from both sides: the resolver
// itself must ignore the environment, and the enforcement must hold across a process that
// changes its mind about where home is.

// The resolver, on the real OS, with the environment lying to it.
//
// This is the one test that deliberately un-hooks the suite's accountHomeFn and calls the
// PRODUCTION path, because "os.UserHomeDir was replaced by something that does not read
// $HOME" is precisely the claim, and a test that only exercised the injected fake would
// assert nothing about it.
func TestDefaultRegistryRootIgnoresAmbientHome(t *testing.T) {
	real := accountHomeFn
	accountHomeFn = accountHome
	t.Cleanup(func() { accountHomeFn = real })

	first, err := defaultRegistryRoot()
	if err != nil {
		t.Skipf("no OS account record on this box (%v) — which fails CLOSED by design, see ErrNoAccountHome", err)
	}

	// Now tell the process it lives somewhere else. Both spellings, because os.UserHomeDir
	// reads $HOME on unix and %USERPROFILE% on windows and a fix that only covered one would
	// be a fix on one platform.
	lie := t.TempDir()
	t.Setenv("HOME", lie)
	t.Setenv("USERPROFILE", lie)

	second, err := defaultRegistryRoot()
	if err != nil {
		t.Fatalf("the environment cannot break a root it does not supply: %v", err)
	}
	if second != first {
		t.Fatalf("the registry root moved when $HOME/%%USERPROFILE%% did (%q → %q). That is the hole: a root the agent "+
			"can relocate is a root it can always find EMPTY, and an empty registry hands out a second seat.", first, second)
	}
	if strings.HasPrefix(first, lie) {
		t.Fatalf("the root landed inside the directory the environment named (%q) — it must come from the OS account, "+
			"not from a variable the governed process sets", first)
	}
	if home, src, err := accountHome(); err != nil || home == lie || !strings.HasPrefix(src, "account:") {
		t.Fatalf("accountHome must answer from the OS and say so: home=%q source=%q err=%v", home, src, err)
	}
}

// The enforcement, end to end: one process, two Opens, a different $HOME in between — and
// NO WithRegistryRoot, because the default root is the thing under test.
//
// Both routes an agent has once it can rewrite its own home are here: pointing
// $BASHY_STEWARD_DIR at a store of its choosing, and simply letting the default store dir
// follow $HOME. Neither may reach a second registry, so both land on the same canonical
// binding and are refused.
func TestChangingHomeCannotMintASecondSeat(t *testing.T) {
	firstHome, secondHome := t.TempDir(), t.TempDir()
	t.Setenv("HOME", firstHome)
	t.Setenv("USERPROFILE", firstHome)
	t.Setenv("BASHY_STEWARD_DIR", "")

	scope := testScope("home-flip-seat")
	canonical := t.TempDir()

	s, err := Open(canonical, WithScopeProvider(scope))
	if err != nil {
		t.Fatalf("the first store for a seat becomes its store: %v", err)
	}
	// The binding lives under the OS account's home (the suite's accountHomeFn), NOT under the
	// $HOME this process was pretending to have when it wrote it.
	wantReg := filepath.Join(testHome, ".bashy", "steward", registryRootName)
	if !strings.HasPrefix(s.RegistryPath(), wantReg+string(filepath.Separator)) {
		t.Fatalf("the binding must be rooted in the OS account's home (%s), got %s", wantReg, s.RegistryPath())
	}

	// The agent moves home. Every ambient signal now says it is somebody else, somewhere else.
	t.Setenv("HOME", secondHome)
	t.Setenv("USERPROFILE", secondHome)

	// Route 1: $BASHY_STEWARD_DIR, the store of its choosing under the new home.
	alternate := filepath.Join(secondHome, "store")
	t.Setenv("BASHY_STEWARD_DIR", alternate)

	_, err = Open("", WithScopeProvider(scope))
	var conflict *ErrScopeDirConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("HOME=%s BASHY_STEWARD_DIR=%s must NOT reach a second registry, find it empty, and mint a second seat "+
			"for a machine-and-account that already has one. Got %v", secondHome, alternate, err)
	}
	if conflict.Canonical != mustCanonical(t, canonical) {
		t.Fatalf("the refusal must name the store the seat actually has, got %q", conflict.Canonical)
	}
	if conflict.Registry != s.RegistryPath() {
		t.Fatalf("both opens must consult the SAME canonical binding — a per-$HOME registry is no registry at all: "+
			"%q vs %q", conflict.Registry, s.RegistryPath())
	}

	// Route 2: no override at all. The default store dir does follow $HOME (it is a place to
	// keep bytes, not a licence to have two — see defaultDirFor), so this is a store the seat
	// has never had, and the registry says so.
	t.Setenv("BASHY_STEWARD_DIR", "")
	_, err = Open("", WithScopeProvider(scope))
	if !errors.As(err, &conflict) {
		t.Fatalf("moving $HOME alone must not mint a second seat either, got %v", err)
	}
	if conflict.Registry != s.RegistryPath() {
		t.Fatalf("same seat, same binding, whatever home says: %q vs %q", conflict.Registry, s.RegistryPath())
	}

	// And the seat's own store keeps opening, from under the moved home, exactly as it should:
	// the check is a singleton, not a lock-out.
	if _, err := Open(canonical, WithScopeProvider(scope)); err != nil {
		t.Fatalf("the seat's own store must keep opening: %v", err)
	}
}

// With no OS account record there is no root, and the store REFUSES TO OPEN rather than
// falling back to something an agent could have chosen.
//
// The fallbacks it declines are $HOME (the hole) and a temp dir (os.TempDir is $TMPDIR — the
// same hole, different variable). The way out is in-process and trusted: WithRegistryRoot.
func TestNoAccountHomeFailsClosed(t *testing.T) {
	real := accountHomeFn
	accountHomeFn = func() (string, string, error) { return "", "", errors.New("no account record for uid 65534") }
	t.Cleanup(func() { accountHomeFn = real })

	_, err := Open(t.TempDir(), WithScopeProvider(testScope("no-home-seat")))
	var noHome *ErrNoAccountHome
	if !errors.As(err, &noHome) {
		t.Fatalf("a store that cannot root its registry must not guess a root — guessing one an agent could have "+
			"chosen is how a second seat gets minted. Got %v", err)
	}
	if !strings.Contains(err.Error(), "WithRegistryRoot") {
		t.Fatalf("the refusal must name the trusted way out, got %q", err.Error())
	}

	// …and that way out works, which is what makes failing closed acceptable rather than fatal
	// for a host whose state lives somewhere the OS cannot name.
	if _, err := Open(t.TempDir(), WithScopeProvider(testScope("no-home-seat")), WithRegistryRoot(t.TempDir())); err != nil {
		t.Fatalf("an injected root must still open the store: %v", err)
	}
}
