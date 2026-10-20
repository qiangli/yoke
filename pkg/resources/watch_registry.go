package resources

import (
	"fmt"

	"github.com/qiangli/yoke/pkg/fleet"
)

// AutoRegisterHost registers the local host's cpu, memory (including swap),
// each mounted disk, network interfaces and GPUs as resources in the fleet
// registry — the same inventory `resource add/list/set` reads — unless a
// record with that name already exists. The host record itself carries the
// per-host watch overrides (e.g. the free-space floor). It returns the
// names it added; running it twice adds nothing the second time.
//
// The way to stop watching one auto-registered resource is
// `resource set NAME --watch enabled=false`: a plain `resource rm` is undone
// by the next registration pass, exactly like any other desired-state sweep.
func AutoRegisterHost(cat *fleet.Catalog, sys *System) ([]string, error) {
	prefix := WatchNamePrefix(sys.Host)
	if prefix == "" {
		return nil, fmt.Errorf("resources: watch: host has no usable name")
	}
	note := "auto-registered by resource watch"
	mk := func(name, title, member string) fleet.Resource {
		return fleet.Resource{
			Name: name, Kind: "name", Title: title, Notes: note,
			Members: []string{member},
		}
	}
	want := []fleet.Resource{
		mk(prefix, "host "+sys.Host+" (watcher defaults)", prefix),
		mk(prefix+".cpu", sys.Host+" cpu", "cpu"),
		mk(prefix+".mem", sys.Host+" memory and swap", "memory"),
	}
	for _, d := range sys.Disks {
		want = append(want, mk(prefix+".disk-"+watchSlug(d.Mount), sys.Host+" disk "+d.Mount, d.Mount))
	}
	for _, n := range sys.Network {
		want = append(want, mk(prefix+".net-"+watchSlug(n.Name), sys.Host+" interface "+n.Name, n.Name))
	}
	for _, g := range sys.GPUs {
		want = append(want, mk(prefix+".gpu-"+watchSlug(g.Name), sys.Host+" gpu "+g.Name, g.Name))
	}
	var added []string
	for _, rec := range want {
		if _, ok := cat.Resource(rec.Name); ok {
			continue
		}
		if err := cat.SaveResource(rec); err != nil {
			return added, fmt.Errorf("resources: watch: registering %s: %w", rec.Name, err)
		}
		added = append(added, rec.Name)
	}
	return added, nil
}

// ResolveWatchConfigs layers the registry's watch overrides: the defaults,
// then the host record's overrides, then each resource's own. The returned
// base config carries the host-level values (including notify-to); the map
// holds per-resource configs only for resources that carry overrides.
func ResolveWatchConfigs(cat *fleet.Catalog, host string) (WatchConfig, map[string]WatchConfig) {
	base := DefaultWatchConfig()
	prefix := WatchNamePrefix(host)
	if prefix != "" {
		if rec, ok := cat.Resource(prefix); ok {
			base = OverrideOnly(base, rec.Watch)
		}
	}
	byName := map[string]WatchConfig{}
	all, _ := cat.Resources()
	for _, rec := range all {
		if len(rec.Watch) == 0 || rec.Name == prefix {
			continue
		}
		if !watchOwnedName(rec.Name, prefix) {
			continue
		}
		byName[rec.Name] = OverrideOnly(base, rec.Watch)
	}
	return base, byName
}

// watchOwnedName reports whether a resource record belongs to this host's
// auto-registered set ("<prefix>." names), so a foreign resource's watch map
// never configures this host's watcher.
func watchOwnedName(name, prefix string) bool {
	return len(name) > len(prefix)+1 && name[:len(prefix)+1] == prefix+"."
}
