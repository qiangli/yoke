package webconsole

import (
	"fmt"
	"github.com/qiangli/yoke/pkg/fleet"
	"io"
	"text/tabwriter"
)

// Retired records remain inspectable without probing or mounting their panels.
func listRetiredApps(out io.Writer, asJSON bool) error {
	apps, errs := fleet.New().Apps()
	if len(errs) > 0 {
		return errs[0]
	}
	rows := make([]fleet.App, 0)
	for _, a := range apps {
		if fleet.MatchRetirement(a, true) {
			rows = append(rows, a)
		}
	}
	if asJSON {
		return fleet.Emit(out, rows, true)
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tRETIRED\tREPLACED-BY\tREASON")
	for _, a := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", a.Name, a.Retired.At, a.Retired.ReplacedBy, a.Retired.Reason)
	}
	return w.Flush()
}
