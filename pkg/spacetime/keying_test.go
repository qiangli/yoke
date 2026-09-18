package spacetime

import (
	"slices"
	"testing"
)

// THE RATCHET. An unkeyable fact inside a context key fragments the evidence
// corpus to n=1: every run files under a fresh coordinate, no skill ever gathers
// enough attestations to be elected or retired, and nothing reports it because
// nothing is broken — only useless. These tests make that a build failure rather
// than a thing reviewers are asked to remember.

// The guard must actually guard something. If the excluded set empties, every
// other test here still passes while protecting nothing.
func TestUnkeyableCoreProbes_NonEmpty(t *testing.T) {
	got := UnkeyableCoreProbes()
	if len(got) == 0 {
		t.Fatal("no core probe is excluded from context keys; the ratchet now guards nothing")
	}
	for _, want := range []string{"time.attended", "time.hour", "time.weekday", "time.zone"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s is no longer excluded from context keys (have %v)", want, got)
		}
	}
}

// Stable host facts are the whole point of a coordinate and must pass through.
func TestIsUnkeyableProbe_StableFactsPassThrough(t *testing.T) {
	for _, name := range []string{"os", "arch", "os.release", "libc", "container", "tty", "elevated"} {
		if IsUnkeyableProbe(name) {
			t.Errorf("%q excluded from keys; it is a stable host fact and belongs in one", name)
		}
	}
	for _, name := range []string{"tool.git", "tool.claude", "engine.podman"} {
		if IsUnkeyableProbe(name) {
			t.Errorf("%q excluded from keys", name)
		}
	}
}

// The line is NOT volatility. Network locality is volatile AND keyable: whether
// a peer is reachable directly is a real capability difference. Filtering it
// would contradict TestContextKeyDoesNotFragmentOnUnreferencedProbes, which
// asserts an entry referencing net.* re-keys on a roam.
func TestIsUnkeyableProbe_NetworkLocalityStaysKeyable(t *testing.T) {
	for _, name := range []string{"net.same_lan", "mesh.paired", "peer.online", "place.id"} {
		if IsUnkeyableProbe(name) {
			t.Errorf("%q excluded from keys; reachability is a capability difference, not clock churn", name)
		}
	}
}

// Namespaces match by prefix so a future time.minute is covered on arrival.
func TestIsUnkeyableProbe_CoversFutureNamespaceMembers(t *testing.T) {
	for _, name := range []string{"time.minute", "time.second", "time.epoch"} {
		if !IsUnkeyableProbe(name) {
			t.Errorf("%q is keyable; the namespace prefix rule regressed", name)
		}
	}
}

func TestKeyableProbes(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "drops clock probes, keeps order",
			in:   []string{"os", "time.hour", "arch", "tool.git"},
			want: []string{"os", "arch", "tool.git"},
		},
		{
			name: "keeps network locality",
			in:   []string{"os", "net.same_lan", "place.id"},
			want: []string{"os", "net.same_lan", "place.id"},
		},
		{
			name: "dedupes",
			in:   []string{"os", "arch", "os", "arch"},
			want: []string{"os", "arch"},
		},
		{
			name: "all excluded yields nothing",
			in:   []string{"time.hour", "time.zone"},
			want: []string{},
		},
		{
			name: "empty in, empty out",
			in:   nil,
			want: []string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := KeyableProbes(tc.in); !slices.Equal(got, tc.want) {
				t.Errorf("KeyableProbes(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// The end-to-end property: two runs of one host at different times of day must
// land on ONE coordinate.
func TestContextKey_UnmovedByTheClock(t *testing.T) {
	morning := map[string]string{"os": "darwin", "arch": "arm64", "time.hour": "09", "time.weekday": "monday"}
	evening := map[string]string{"os": "darwin", "arch": "arm64", "time.hour": "21", "time.weekday": "friday"}

	// Sanity: unfiltered, the clock DOES move the key. Without this the
	// assertion below could pass because ContextKey ignored its input.
	if ContextKey(morning) == ContextKey(evening) {
		t.Fatal("clock probes did not move ContextKey; the fixture proves nothing")
	}

	filter := func(m map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			if !IsUnkeyableProbe(k) {
				out[k] = v
			}
		}
		return out
	}
	if ContextKey(filter(morning)) != ContextKey(filter(evening)) {
		t.Error("one host at two times of day produced two coordinates")
	}
	if ContextKey(filter(morning)) != ContextKey(map[string]string{"os": "darwin", "arch": "arm64"}) {
		t.Error("filtered snapshot did not match the stable-only snapshot")
	}
}

// The environment coordinate is where knowledge belonging to no particular
// skill gets filed, so it must be readable back tomorrow, from an agent session
// as well as a terminal, and after the walk to the office. Each excluded probe
// is named here so that adding one to the list is a deliberate act with a test
// to answer to.
func TestEnvironmentProbes_ExcludeWhatChurns(t *testing.T) {
	for _, banned := range []string{"time.hour", "time.weekday", "time.zone", "time.attended", "tty", "elevated", "place.id"} {
		if slices.Contains(EnvironmentProbes(), banned) {
			t.Errorf("%q is in the environment coordinate; it changes without the machine changing", banned)
		}
	}
	if !slices.Contains(EnvironmentProbes(), "os") || !slices.Contains(EnvironmentProbes(), "arch") {
		t.Error("the environment coordinate no longer carries os/arch, so it distinguishes nothing")
	}
}

// Two probe sets over one host must agree. This is the property the CLI depends
// on when it defaults a coordinate rather than asking for one.
func TestEnvironmentCoordinate_StableAcrossProbeSets(t *testing.T) {
	a := DefaultProbes(NopCache()).EnvironmentCoordinate()
	b := DefaultProbes(NopCache()).EnvironmentCoordinate()
	if a != b {
		t.Errorf("one host produced two environment coordinates: %s and %s", a, b)
	}
}

// coreProbeList is the single source both DefaultProbes and the keying rules
// read; this asserts they agree rather than trusting the arrangement.
func TestCoreProbeList_BackstopsDefaultProbes(t *testing.T) {
	ps := DefaultProbes(NopCache())
	for _, p := range coreProbeList() {
		if _, ok := ps.core[p.Name]; !ok {
			t.Errorf("core probe %q is in the table but absent from DefaultProbes", p.Name)
		}
	}
	if len(ps.core) != len(coreProbeList()) {
		t.Errorf("DefaultProbes has %d core probes, table defines %d", len(ps.core), len(coreProbeList()))
	}
}
