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
	all, custom bool
	retired     bool
	ring        string
}

func (f *listFilter) flags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.retired, "retired", false, "show only retired entries")
	cmd.Flags().BoolVar(&f.custom, "custom", false, "show only local custom definitions")
	cmd.Flags().BoolVar(&f.all, "all", false, "show every ring, including otherwise hidden entries")
	cmd.Flags().StringVar(&f.ring, "ring", "", "show exactly one ring: all, embedded, shared, cloud, or local")
}

func (f listFilter) selected() (string, error) {
	n := 0
	if f.retired {
		n++
	}
	if f.all {
		n++
	}
	if f.custom {
		n++
	}
	if f.ring != "" {
		n++
	}
	if n > 1 {
		return "", fmt.Errorf("fleet: --all, --custom, and --ring are alternatives; give one")
	}
	if f.retired {
		return "all", nil
	}
	if f.all {
		return "all", nil
	}
	if f.custom {
		return "local", nil
	}
	switch f.ring {
	case "", "all", "embedded", "shared", "cloud", "local":
		return f.ring, nil
	default:
		return "", fmt.Errorf("fleet: invalid --ring %q (want all, embedded, shared, cloud, or local)", f.ring)
	}
}

func (f listFilter) match(r assetring.Ring, selected string) bool {
	if selected == "all" {
		return true
	}
	if selected == "" {
		return r != assetring.RingLocal
	}
	return r.String() == selected
}

const listFilterHelp = `By default, list shows selected definitions from the embedded, shared, and
cloud rings and hides local custom definitions. --custom shows only local;
--all shows every ring; --ring all|embedded|shared|cloud|local selects a precise
ring. These flags are alternatives. A local override of a seeded name has
RING local because local supplied the selected definition. Text and JSON use
the same view; only text prints a hint when custom definitions are hidden.`

func hiddenCustomHint(cmd *cobra.Command, count int, selected string) {
	if selected == "" && count > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "%d custom entries hidden — --custom to show\n", count)
	}
}
