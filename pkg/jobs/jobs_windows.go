//go:build windows

package jobs

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
)

type ListedJob struct {
	Record  JobRecord
	Started string
	Elapsed string
}

type ListResult struct {
	Jobs []ListedJob
}

type JobResult struct {
	PID     int
	Record  JobRecord
	Signal  string
	Deleted bool
	Message string
}

func ListJobs(*JobRegistry, time.Time) (ListResult, error) {
	return ListResult{}, fmt.Errorf("jobs relies on signal-based job control and is not available on Windows")
}

func RenderList(io.Writer, ListResult) error { return nil }

func ForegroundJob(context.Context, *JobRegistry, int) (JobResult, error) {
	return JobResult{}, fmt.Errorf("fg relies on signal-based job control and is not available on Windows")
}

func BackgroundJob(*JobRegistry, int) (JobResult, error) {
	return JobResult{}, fmt.Errorf("bg relies on signal-based job control and is not available on Windows")
}

func KillJob(*JobRegistry, int, string) (JobResult, error) {
	return JobResult{}, fmt.Errorf("kill relies on signal-based job control and is not available on Windows")
}

func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "(Unix-only) not supported on Windows",
	}
	cmd.AddCommand(Commands()...)
	return cmd
}

func Commands() []*cobra.Command {
	return []*cobra.Command{JobsCommand(), FgCommand(), BgCommand(), KillCommand()}
}

func JobsCommand() *cobra.Command { return notSupportedCmd("jobs") }
func FgCommand() *cobra.Command   { return notSupportedCmd("fg") }
func BgCommand() *cobra.Command   { return notSupportedCmd("bg") }
func KillCommand() *cobra.Command { return notSupportedCmd("kill") }

func notSupportedCmd(name string) *cobra.Command {
	return &cobra.Command{
		Use:   name,
		Short: "(Unix-only) not supported on Windows",
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("%s relies on signal-based job control and is not available on Windows", name)
		},
	}
}
