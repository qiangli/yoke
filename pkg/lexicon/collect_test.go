package lexicon

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func addrs(cidrs ...string) []net.Addr {
	out := make([]net.Addr, 0, len(cidrs))
	for _, c := range cidrs {
		ip, ipnet, err := net.ParseCIDR(c)
		if err != nil {
			continue
		}
		ipnet.IP = ip
		out = append(out, ipnet)
	}
	return out
}

// One interface legitimately holds both an IPv4 and an IPv6, and they are
// different facts. A shared key would make the second supersede the first and
// silently lose an address — which is what happened the first time this ran
// against a real machine: four addresses collected, one stored.
func TestDiscover_AddressFamilyIsPartOfTheKey(t *testing.T) {
	_, found := Discover(CollectOptions{
		Hostname:   "workshop",
		Interfaces: []net.Interface{{Name: "en0"}},
		Addrs: func(net.Interface) ([]net.Addr, error) {
			return addrs("10.0.0.41/24", "2001:db8::1/64"), nil
		},
		MountRoots: []string{},
	})
	if len(found) != 2 {
		t.Fatalf("got %d discoveries, want 2 (v4 and v6 are different facts): %+v", len(found), found)
	}
	if found[0].Key == found[1].Key {
		t.Errorf("both addresses share key %q; one would supersede the other", found[0].Key)
	}
}

// THE SPLIT, at the source. An interface NAME is vocabulary — `utun3` means the
// same kind of thing anywhere. Its ADDRESS is identity: it says which machine
// this is, and belongs in a host-local fact store, not a glossary.
func TestDiscover_SeparatesNamesFromAddresses(t *testing.T) {
	inv, found := Discover(CollectOptions{
		Hostname: "workshop",
		Interfaces: []net.Interface{
			{Name: "en0"}, {Name: "utun3"}, {Name: "lo0"},
		},
		Addrs: func(i net.Interface) ([]net.Addr, error) {
			switch i.Name {
			case "en0":
				return addrs("10.0.0.41/24"), nil
			case "lo0":
				return addrs("127.0.0.1/8"), nil
			}
			return nil, nil
		},
		MountRoots: []string{},
	})

	if !slices.Equal(inv.Interfaces, []string{"en0", "lo0", "utun3"}) {
		t.Errorf("interfaces = %v", inv.Interfaces)
	}
	// The addresses are NOT in the term set.
	for _, name := range inv.Interfaces {
		if name == "10.0.0.41" {
			t.Fatal("an address was collected as a term")
		}
	}

	if len(found) != 1 {
		t.Fatalf("got %d discoveries, want 1 (loopback says nothing about which machine this is): %+v", len(found), found)
	}
	d := found[0]
	if d.EntityKind != "host" || d.EntityName != "workshop" {
		t.Errorf("discovery not bound to the host: %+v", d)
	}
	if d.Key != "address.en0.v4" || d.Value != "10.0.0.41" {
		t.Errorf("discovery = %+v", d)
	}
}

// Loopback and link-local identify nobody; recording them would add noise to
// the fact store without adding knowledge.
func TestDiscover_SkipsNonIdentifyingAddresses(t *testing.T) {
	_, found := Discover(CollectOptions{
		Hostname:   "workshop",
		Interfaces: []net.Interface{{Name: "lo0"}, {Name: "en1"}},
		Addrs: func(i net.Interface) ([]net.Addr, error) {
			if i.Name == "lo0" {
				return addrs("127.0.0.1/8", "::1/128"), nil
			}
			return addrs("169.254.1.1/16"), nil // link-local
		},
		MountRoots: []string{},
	})
	if len(found) != 0 {
		t.Errorf("got %+v, want nothing", found)
	}
}

// An interface that will not answer is normal, not fatal.
func TestDiscover_ToleratesInterfaceErrors(t *testing.T) {
	inv, found := Discover(CollectOptions{
		Hostname:   "workshop",
		Interfaces: []net.Interface{{Name: "en0"}, {Name: "broken"}},
		Addrs: func(i net.Interface) ([]net.Addr, error) {
			if i.Name == "broken" {
				return nil, errors.New("device not configured")
			}
			return addrs("10.0.0.41/24"), nil
		},
		MountRoots: []string{},
	})
	// The NAME still lands even though the address lookup failed — the name is
	// what the glossary wanted.
	if !slices.Contains(inv.Interfaces, "broken") {
		t.Errorf("a failing interface's name was dropped: %v", inv.Interfaces)
	}
	if len(found) != 1 {
		t.Errorf("got %d discoveries, want 1", len(found))
	}
}

// With no hostname there is nothing to bind an address to, so it is not a fact.
func TestDiscover_NoHostnameMeansNoFacts(t *testing.T) {
	inv, found := Discover(CollectOptions{
		Interfaces: []net.Interface{{Name: "en0"}},
		Addrs:      func(net.Interface) ([]net.Addr, error) { return addrs("10.0.0.41/24"), nil },
		MountRoots: []string{},
	})
	if len(found) != 0 {
		t.Errorf("an address was recorded with nothing to bind it to: %+v", found)
	}
	if !slices.Contains(inv.Interfaces, "en0") {
		t.Error("the interface name should still be collected")
	}
}

func TestDiscover_Mounts(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"scratch", "backup", ".hidden"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "afile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	inv, _ := Discover(CollectOptions{Interfaces: []net.Interface{}, MountRoots: []string{root}})
	if !slices.Equal(inv.Mounts, []string{"backup", "scratch"}) {
		t.Errorf("mounts = %v, want the two real dirs (no dotfiles, no files)", inv.Mounts)
	}
}

// A mount root that does not exist costs nothing — listing every platform's
// conventions must not fail on the platforms that use none of them.
func TestDiscover_MissingMountRootsAreSkipped(t *testing.T) {
	inv, _ := Discover(CollectOptions{
		Interfaces: []net.Interface{},
		MountRoots: []string{"/definitely/not/here", "/nor/here"},
	})
	if len(inv.Mounts) != 0 {
		t.Errorf("mounts = %v", inv.Mounts)
	}
}

func TestAddCollected_ProjectsAndResolves(t *testing.T) {
	s := &Store{byTerm: map[string]int{}, byTermAll: map[string][]int{}}
	s.AddCollected(SystemInventory{
		Interfaces: []string{"utun3"},
		Mounts:     []string{"scratch"},
	}, Overlay{})

	for _, tc := range []struct {
		term string
		kind Kind
	}{
		{"utun3", KindInterface},
		{"scratch", KindMount},
	} {
		got, ok := s.Resolve(tc.term)
		if !ok {
			t.Errorf("%q did not resolve", tc.term)
			continue
		}
		if got.Kind != tc.kind {
			t.Errorf("%q kind = %s, want %s", tc.term, got.Kind, tc.kind)
		}
		if got.ScopeNote == "" {
			t.Errorf("%q has no scope note", tc.term)
		}
	}
}

// Collection is a pure function of its inputs.
func TestDiscover_Deterministic(t *testing.T) {
	opts := CollectOptions{
		Hostname:   "workshop",
		Interfaces: []net.Interface{{Name: "utun3"}, {Name: "en0"}},
		Addrs:      func(net.Interface) ([]net.Addr, error) { return addrs("10.0.0.41/24"), nil },
		MountRoots: []string{},
	}
	inv1, f1 := Discover(opts)
	inv2, f2 := Discover(opts)

	if !slices.Equal(inv1.Interfaces, inv2.Interfaces) {
		t.Error("interface order is not deterministic")
	}
	if len(f1) != len(f2) || (len(f1) > 0 && f1[0] != f2[0]) {
		t.Error("discoveries are not deterministic")
	}
	if !slices.IsSorted(inv1.Interfaces) {
		t.Errorf("interfaces not sorted: %v", inv1.Interfaces)
	}
}
