package lexicon

// SYSTEM COLLECTION — prepopulating the glossary by asking the machine.
//
// Enumerate reads what the SHELL knows: env keys, PATH, the working directory.
// This goes one layer down and asks the OS: what interfaces exist, what volumes
// are mounted. Those names are dense local jargon — `en0`, `utun3`, `docker0`,
// `tailscale0`, a volume called `scratch` — and an agent that meets one in a log
// has no way to look it up.
//
// # Two outputs, because collection finds two KINDS of thing
//
// An interface NAME is vocabulary: `utun3` means the same kind of thing on any
// machine that has one, and telling an agent what it is helps everywhere.
//
// An interface ADDRESS is identity: it says which machine this is, and it
// belongs in the host-local fact store rather than a glossary that might be
// shared.
//
// So Discover returns both, separated at the source rather than filtered later.
// That separation is what lets the caller put each where it belongs without
// having to classify anything itself — and it makes the identity filter
// STRONGER as a side effect, because every address collected here is one more
// thing the fold gate can recognise.
//
// # What is deliberately NOT collected
//
// Numbers. CPU percentages, bytes free, load averages are telemetry, not
// vocabulary — pkg/resources already reports them and nothing is gained by
// calling them terms. The collectors worth mining are the ones that emit NAMES.
//
// Process names would qualify, and are absent because coreutils has no `ps`:
// claiming to collect them would mean shelling out, which this repo does not do.

import (
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Kinds contributed by system collection.
const (
	KindInterface Kind = "interface" // a network interface name
	KindMount     Kind = "mount"     // a mounted volume's name
)

const (
	interfaceScopeNote = "A network interface on THIS host. The name is vocabulary; its addresses " +
		"are identity and are not recorded here."
	mountScopeNote = "A volume mounted on this host. The name carries local meaning — it may be a " +
		"disk, a share, or a container mount."
)

// Discovery is one identity-bearing thing collection found.
//
// Deliberately NOT a lexicon Concept: these do not belong in a glossary. The
// struct is neutral data so the caller can hand it to a fact store without this
// package depending on one.
type Discovery struct {
	// EntityKind is the kind of thing this describes ("host", "endpoint").
	EntityKind string `json:"entity_kind"`
	// EntityName identifies it.
	EntityName string `json:"entity_name"`
	Key        string `json:"key"`
	Value      string `json:"value"`
}

// CollectOptions configures collection. Every input is injectable so tests stay
// hermetic — nothing here touches the machine unless asked.
type CollectOptions struct {
	// Interfaces supplies the interface list; nil means read the host's.
	Interfaces []net.Interface
	// Addrs resolves an interface's addresses; nil means ask the interface.
	Addrs func(net.Interface) ([]net.Addr, error)
	// MountRoots are directories whose entries are mounted volumes. Nil means
	// the platform defaults.
	MountRoots []string
	// Hostname is this machine's name; empty means read it.
	Hostname string
}

// Discover collects name-bearing terms and identity-bearing facts.
func Discover(o CollectOptions) (SystemInventory, []Discovery) {
	var inv SystemInventory
	var found []Discovery

	host := strings.TrimSpace(o.Hostname)
	if host == "" && o.Interfaces == nil {
		// Only read the real hostname when not running against injected input,
		// so a test never picks up the developer's machine.
		host, _ = os.Hostname()
	}

	ifaces := o.Interfaces
	if ifaces == nil {
		ifaces, _ = net.Interfaces()
	}
	addrsOf := o.Addrs
	if addrsOf == nil {
		addrsOf = func(i net.Interface) ([]net.Addr, error) { return i.Addrs() }
	}

	for _, ifc := range ifaces {
		if name := strings.TrimSpace(ifc.Name); name != "" {
			inv.Interfaces = appendUnique(inv.Interfaces, name)
		}
		if host == "" {
			continue // an address with nothing to bind it to is not a fact
		}
		addrs, err := addrsOf(ifc)
		if err != nil {
			continue // an interface that will not answer is normal, not fatal
		}
		for _, a := range addrs {
			ip := addrIP(a)
			// Loopback and link-local say nothing about WHICH machine this is,
			// so recording them would add noise to the fact store without
			// adding knowledge.
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
				continue
			}
			// The family is part of the key, because one interface legitimately
			// holds both an IPv4 and an IPv6 and they are different facts. With
			// a shared key the second supersedes the first and the address is
			// silently lost — which is exactly what happened the first time this
			// ran against a real machine: four addresses collected, one stored.
			family := "v4"
			if ip.To4() == nil {
				family = "v6"
			}
			found = append(found, Discovery{
				EntityKind: "host", EntityName: host,
				Key: "address." + ifc.Name + "." + family, Value: ip.String(),
			})
		}
	}

	roots := o.MountRoots
	if roots == nil {
		roots = defaultMountRoots()
	}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			inv.Mounts = appendUnique(inv.Mounts, e.Name())
		}
	}

	sort.Strings(inv.Interfaces)
	sort.Strings(inv.Mounts)
	sort.Slice(found, func(i, j int) bool {
		if found[i].Key != found[j].Key {
			return found[i].Key < found[j].Key
		}
		return found[i].Value < found[j].Value
	})
	return inv, found
}

func addrIP(a net.Addr) net.IP {
	switch v := a.(type) {
	case *net.IPNet:
		return v.IP
	case *net.IPAddr:
		return v.IP
	}
	ip, _, err := net.ParseCIDR(a.String())
	if err != nil {
		return net.ParseIP(a.String())
	}
	return ip
}

// defaultMountRoots are the directories each platform mounts removable and
// secondary volumes under. A path that does not exist is skipped, so listing
// all of them costs nothing on a platform that uses none.
func defaultMountRoots() []string {
	return []string{
		"/Volumes",   // darwin
		"/mnt",       // linux, WSL
		"/media",     // linux desktop
		"/run/media", // linux, systemd
		filepath.Clean("/srv"),
	}
}

// AddCollected projects collected system names into the store.
func (s *Store) AddCollected(inv SystemInventory, ov Overlay) {
	for _, name := range inv.Interfaces {
		s.add(Concept{
			ID: "iface:" + name, Kind: KindInterface, PrefLabel: name,
			Definition: "a network interface on this host",
			ScopeNote:  interfaceScopeNote,
			Source:     "system:net",
		}, ov)
	}
	for _, name := range inv.Mounts {
		s.add(Concept{
			ID: "mount:" + name, Kind: KindMount, PrefLabel: name,
			Definition: "a volume mounted on this host",
			ScopeNote:  mountScopeNote,
			Source:     "system:mounts",
		}, ov)
	}
	s.reindex()
}
