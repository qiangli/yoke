package spacetime

import (
	"testing"
	"time"
)

// netResolver stands in for the real net.* namespace: a fact that flips
// while the process runs (a laptop roams off the LAN).
type netResolver struct {
	sameLAN *bool
	calls   *int
}

func (netResolver) Namespace() string { return "net" }
func (netResolver) Volatile() bool    { return true }
func (r netResolver) Eval(key string) (string, error) {
	*r.calls++
	if key != "same_lan" {
		return "", ErrNotApplicable
	}
	if *r.sameLAN {
		return "true", nil
	}
	return "false", nil
}

// A volatile namespace must never be served from the persistent cache.
// Serving net.same_lan from a 24h TTL is worse than not probing it.
func TestVolatileNamespaceBypassesPersistentCache(t *testing.T) {
	dir := t.TempDir()
	lan, calls := true, 0

	ps := DefaultProbes(NewFileCache(dir, time.Hour))
	ps.Register(netResolver{&lan, &calls})
	if v, _ := ps.Value("net.same_lan"); v != "true" {
		t.Fatalf("net.same_lan = %q, want true", v)
	}

	// The host roams. A fresh process must observe the change, even
	// though a static probe cached under the same dir would not.
	lan = false
	ps2 := DefaultProbes(NewFileCache(dir, time.Hour))
	ps2.Register(netResolver{&lan, &calls})
	if v, _ := ps2.Value("net.same_lan"); v != "false" {
		t.Fatal("net.same_lan was served from the persistent cache across a roam")
	}
}

// Within one process the volatile value is memoized once, so a single
// command sees one consistent coordinate rather than a value that
// changes between two clauses of the same predicate.
func TestVolatileNamespaceMemoizedWithinProcess(t *testing.T) {
	lan, calls := true, 0
	ps := DefaultProbes(NopCache())
	ps.Register(netResolver{&lan, &calls})

	ps.Value("net.same_lan")
	ps.Value("net.same_lan")
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (memoized within the process)", calls)
	}

	// Forget re-opens the question for the next read.
	ps.Forget("net.same_lan")
	lan = false
	if v, _ := ps.Value("net.same_lan"); v != "false" {
		t.Fatalf("after Forget, net.same_lan = %q, want false", v)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (Forget forces re-eval)", calls)
	}
}

// A non-volatile resolver keeps the old caching behavior exactly.
func TestNonVolatileNamespaceStillCached(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	ps := DefaultProbes(NewFileCache(dir, time.Hour))
	ps.Register(countingResolver{&calls})
	ps.Value("fake.thing")

	ps2 := DefaultProbes(NewFileCache(dir, time.Hour))
	ps2.Register(countingResolver{&calls})
	ps2.Value("fake.thing")
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (static namespace persists across processes)", calls)
	}
}

// A volatile core Probe is re-evaluated on every read.
func TestVolatileCoreProbe(t *testing.T) {
	n := 0
	ps := DefaultProbes(NopCache())
	ps.SetProbe(Probe{Name: "peer.online", Volatile: true, Eval: func() (string, error) {
		n++
		if n == 1 {
			return "true", nil
		}
		return "false", nil
	}})
	if v, _ := ps.Value("peer.online"); v != "true" {
		t.Fatal("first read")
	}
	if v, _ := ps.Value("peer.online"); v != "false" {
		t.Fatal("a volatile core probe must re-evaluate on every read")
	}
}

func TestStaticCoreProbeMemoized(t *testing.T) {
	n := 0
	ps := DefaultProbes(NopCache())
	ps.SetProbe(Probe{Name: "steady", Eval: func() (string, error) {
		n++
		return "yes", nil
	}})
	ps.Value("steady")
	ps.Value("steady")
	if n != 1 {
		t.Fatalf("static core probe evaluated %d times, want 1", n)
	}
}

// The requires grammar must accept the dotted, underscored probe names
// contact methods gate on (net.same_lan, mesh.paired, peer.online).
func TestRequiresAcceptsVolatileProbeNames(t *testing.T) {
	r, err := ParseRequires("net.same_lan mesh.paired")
	if err != nil {
		t.Fatalf("ParseRequires: %v", err)
	}
	refs := r.ProbeRefs()
	if len(refs) != 2 || refs[0] != "net.same_lan" || refs[1] != "mesh.paired" {
		t.Fatalf("ProbeRefs = %v", refs)
	}

	lan, calls := true, 0
	ps := DefaultProbes(NopCache())
	ps.Register(netResolver{&lan, &calls})
	ps.SetStatic("mesh.paired", "true")

	if v := r.Eval(ps); !v.Applicable {
		t.Fatalf("expected applicable on a paired same-LAN host, got %+v", v)
	}

	// Roam: the same predicate must now fail, naming the failing clause.
	lan = false
	ps.Forget("net.same_lan")
	v := r.Eval(ps)
	if v.Applicable || v.Failing == "" {
		t.Fatalf("expected inapplicable after roaming off the LAN, got %+v", v)
	}
}

// ContextKey covers only the probes a predicate references, so an entry
// that never mentions net.* does not re-key when the host roams.
func TestContextKeyDoesNotFragmentOnUnreferencedProbes(t *testing.T) {
	quiet, err := ParseRequires("os=darwin")
	if err != nil {
		t.Fatal(err)
	}
	roamer, err := ParseRequires("os=darwin net.same_lan")
	if err != nil {
		t.Fatal(err)
	}
	if got := quiet.ProbeRefs(); len(got) != 1 || got[0] != "os" {
		t.Fatalf("quiet.ProbeRefs = %v, want [os]", got)
	}
	if got := roamer.ProbeRefs(); len(got) != 2 {
		t.Fatalf("roamer.ProbeRefs = %v, want os + net.same_lan", got)
	}

	// Same coordinate, two network states: the quiet entry's key is
	// stable; only the roamer's moves.
	onLAN := map[string]string{"os": "darwin", "net.same_lan": "true"}
	offLAN := map[string]string{"os": "darwin", "net.same_lan": "false"}
	quietKey := func(m map[string]string) string {
		return ContextKey(map[string]string{"os": m["os"]})
	}
	if quietKey(onLAN) != quietKey(offLAN) {
		t.Fatal("an entry that never references net.* must not re-key on a roam")
	}
	if ContextKey(onLAN) == ContextKey(offLAN) {
		t.Fatal("an entry that references net.* must re-key on a roam")
	}
}
