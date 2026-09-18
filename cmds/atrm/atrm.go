// Package atrmcmd implements atrm(1): remove pending `at` jobs from
// the bashy schedule store.
package atrmcmd

import (
	"fmt"

	"github.com/qiangli/coreutils/pkg/schedule"
	"github.com/qiangli/coreutils/tool"
)

var cmd = &tool.Tool{
	Name:     "atrm",
	Synopsis: "Remove pending at jobs.",
	Usage:    "atrm JOBID...",
}

func init() { cmd.Run = run; tool.Register(cmd) }

func run(rc *tool.RunContext, args []string) int {
	fs := tool.NewFlags(cmd.Name)
	operands, code := tool.Parse(rc, cmd, fs, args)
	if code >= 0 {
		return code
	}
	if len(operands) == 0 {
		return tool.UsageError(rc, cmd, "missing job ID")
	}
	identity, err := schedule.AuthenticatedIdentity()
	if err != nil {
		fmt.Fprintf(rc.Err, "%s: %v\n", cmd.Name, err)
		return 1
	}

	missing := false
	if err := schedule.StoreFor(rc.Dir, rc.Env).UpdateJobs(func(jobs []*schedule.Job) ([]*schedule.Job, error) {
		// Validate the complete request first. Foreign, non-at, and legacy jobs
		// are deliberately indistinguishable from missing jobs and cannot cause
		// a partial mutation of the caller's own queue.
		for _, id := range operands {
			found := false
			for _, job := range jobs {
				if job.Kind == "at" && job.OwnerUID != "" && job.OwnerUID == identity.UID && (job.ID == id || job.Name == id) {
					found = true
					break
				}
			}
			if !found {
				fmt.Fprintf(rc.Err, "%s: no job %q\n", cmd.Name, id)
				missing = true
			}
		}
		if missing {
			return jobs, nil
		}
		for _, id := range operands {
			for i := len(jobs) - 1; i >= 0; i-- {
				job := jobs[i]
				if job.Kind == "at" && job.OwnerUID == identity.UID && (job.ID == id || job.Name == id) {
					jobs = append(jobs[:i], jobs[i+1:]...)
				}
			}
		}
		return jobs, nil
	}); err != nil {
		fmt.Fprintf(rc.Err, "%s: cannot save schedule: %v\n", cmd.Name, err)
		return 1
	}
	if missing {
		return 1
	}
	return 0
}
