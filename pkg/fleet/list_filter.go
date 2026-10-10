package fleet

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/assetring"
)

// ListFilter is the common view contract of every registry kind's `list`. A
// ring describes the selected definition after overlay resolution, not its
// origin before an override was applied. Kinds outside this package (skills)
// bind the same flags, so one view means one thing everywhere.
type ListFilter struct {
	All, Custom, Builtin, Active bool
	Retired                      bool
	Ring                         string
}

func (f *ListFilter) Flags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.Retired, "retired", false, "show only retired entries")
	cmd.Flags().BoolVar(&f.Custom, "custom", false, "show only local custom definitions")
	cmd.Flags().BoolVar(&f.Builtin, "builtin", false, "show only embedded definitions")
	cmd.Flags().BoolVar(&f.Active, "active", false, "show only entries usable on this host now")
	cmd.Flags().BoolVar(&f.All, "all", false, "show every ring, including otherwise hidden entries")
	cmd.Flags().StringVar(&f.Ring, "ring", "", "show exactly one ring: all, embedded, shared, cloud, or local")
}

func (f ListFilter) Selected() (string, error) {
	n := 0
	for _, set := range []bool{f.Retired, f.All, f.Custom, f.Builtin, f.Active, f.Ring != ""} {
		if set {
			n++
		}
	}
	if n > 1 {
		return "", fmt.Errorf("fleet: --retired, --all, --custom, --builtin, --active, and --ring are alternatives; give one")
	}
	if f.Retired || f.All {
		return "all", nil
	}
	if f.Custom {
		return "local", nil
	}
	if f.Builtin {
		return "embedded", nil
	}
	if f.Active {
		return "active", nil
	}
	switch f.Ring {
	case "", "all", "embedded", "shared", "cloud", "local":
		return f.Ring, nil
	default:
		return "", fmt.Errorf("fleet: invalid --ring %q (want all, embedded, shared, cloud, or local)", f.Ring)
	}
}

func (f ListFilter) Match(r assetring.Ring, selected string) bool {
	switch selected {
	case "all", "active", "":
		return true
	}
	return r.String() == selected
}

// Keep applies the view to one entry: its retirement, its ring, and — under
// --active — whether it is usable here now. active is only called for that.
func (f ListFilter) Keep(selected string, ring assetring.Ring, life RecordLifecycle, active func() bool) bool {
	if !MatchRetirement(life, f.Retired) || !f.Match(ring, selected) {
		return false
	}
	return !f.Active || active()
}

// View names the view for the list envelope. A --ring value with a flag of
// its own reports that flag's name; shared and cloud have none, so they
// report as ring:<name>.
func (f ListFilter) View() string {
	switch {
	case f.Retired:
		return "retired"
	case f.All:
		return "all"
	case f.Custom:
		return "custom"
	case f.Builtin:
		return "builtin"
	case f.Active:
		return "active"
	}
	switch f.Ring {
	case "":
		return "default"
	case "all":
		return "all"
	case "embedded":
		return "builtin"
	case "local":
		return "custom"
	}
	return "ring:" + f.Ring
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
