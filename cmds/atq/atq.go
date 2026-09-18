// Package atqcmd implements atq(1): list pending one-shot `at` jobs
// from the bashy schedule store.
package atqcmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/schedule"
	"github.com/qiangli/coreutils/tool"
)

var cmd = &tool.Tool{
	Name:     "atq",
	Synopsis: "List pending at jobs.",
	Usage:    "atq",
}

func init() { cmd.Run = run; tool.Register(cmd) }

func run(rc *tool.RunContext, args []string) int {
	fs := tool.NewFlags(cmd.Name)
	operands, code := tool.Parse(rc, cmd, fs, args)
	if code >= 0 {
		return code
	}
	if len(operands) != 0 {
		return tool.UsageError(rc, cmd, "extra operand %q", operands[0])
	}
	identity, err := schedule.AuthenticatedIdentity()
	if err != nil {
		fmt.Fprintf(rc.Err, "%s: %v\n", cmd.Name, err)
		return 1
	}

	jobs, err := schedule.StoreFor(rc.Dir, rc.Env).LoadJobs()
	if err != nil {
		fmt.Fprintf(rc.Err, "%s: cannot load schedule: %v\n", cmd.Name, err)
		return 1
	}

	found := false
	for _, j := range jobs {
		if j.Kind != "at" || !j.Enabled || j.OwnerUID == "" || j.OwnerUID != identity.UID {
			continue
		}
		found = true
		if _, err := fmt.Fprintf(rc.Out, "%s\t%s\t%s\n", j.ID, j.NextRun.Format(time.RFC3339), strings.Join(j.Command, " ")); err != nil {
			return 1
		}
	}
	if !found {
		if _, err := fmt.Fprintln(rc.Out, "no pending at jobs"); err != nil {
			return 1
		}
	}
	return 0
}
