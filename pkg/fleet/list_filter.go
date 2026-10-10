package fleet

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/assetring"
)

// listFilter is the common view contract for the three fleet nouns. A ring
// describes the selected definition after overlay resolution, not its origin
// before an override was applied.
type listFilter struct {
	all, custom, builtin, active bool
	retired                      bool
	ring                         string
}

func (f *listFilter) flags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.retired, "retired", false, "show only retired entries")
	cmd.Flags().BoolVar(&f.custom, "custom", false, "show only local custom definitions")
	cmd.Flags().BoolVar(&f.builtin, "builtin", false, "show only embedded definitions")
	cmd.Flags().BoolVar(&f.active, "active", false, "show only entries usable on this host now")
	cmd.Flags().BoolVar(&f.all, "all", false, "show every ring, including otherwise hidden entries")
	cmd.Flags().StringVar(&f.ring, "ring", "", "show exactly one ring: all, embedded, shared, cloud, or local")
}

func (f listFilter) selected() (string, error) {
	n := 0
	for _, set := range []bool{f.retired, f.all, f.custom, f.builtin, f.active, f.ring != ""} {
		if set {
			n++
		}
	}
	if n > 1 {
		return "", fmt.Errorf("fleet: --retired, --all, --custom, --builtin, --active, and --ring are alternatives; give one")
	}
	if f.retired || f.all {
		return "all", nil
	}
	if f.custom {
		return "local", nil
	}
	if f.builtin {
		return "embedded", nil
	}
	if f.active {
		return "active", nil
	}
	switch f.ring {
	case "", "all", "embedded", "shared", "cloud", "local":
		return f.ring, nil
	default:
		return "", fmt.Errorf("fleet: invalid --ring %q (want all, embedded, shared, cloud, or local)", f.ring)
	}
}

func (f listFilter) match(r assetring.Ring, selected string) bool {
	switch selected {
	case "all", "active", "":
		return true
	}
	return r.String() == selected
}

// explicitView reports a ring-pinned or show-everything view. Those show
// every entry, including hidden definitions and non-CLI tool kinds. The
// default and --active views are the pickable roster: visible CLI entries,
// usable ones under --active.
func explicitView(selected string) bool {
	switch selected {
	case "", "active":
		return false
	}
	return true
}

const listFilterHelp = `By default, list shows the selected definition per name across ALL rings,
local custom definitions included. --custom shows only local; --builtin shows
only embedded (--ring embedded spells the same view); --active shows only
entries usable on this host now, with no network; --all shows every ring,
including otherwise hidden entries; --ring all|embedded|shared|cloud|local
selects a precise ring. These flags are alternatives. A local override of a
seeded name has RING local because local supplied the selected definition.
Text and JSON use the same view.`
