//go:build !windows

package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

const pollInterval = 250 * time.Millisecond

// ListedJob is one rendered row returned by ListJobs.
type ListedJob struct {
	Record  JobRecord
	Started string
	Elapsed string
}

// ListResult is the structured result for jobs.
type ListResult struct {
	Jobs []ListedJob
}

// JobResult is the structured result for fg, bg, and kill.
type JobResult struct {
	PID     int
	Record  JobRecord
	Signal  string
	Deleted bool
	Message string
}

// ListJobs reads the registry and prepares display-oriented fields.
func ListJobs(reg *JobRegistry, now time.Time) (ListResult, error) {
	rows, err := reg.List()
	if err != nil {
		return ListResult{}, fmt.Errorf("read registry: %w", err)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	res := ListResult{Jobs: make([]ListedJob, 0, len(rows))}
	for _, r := range rows {
		res.Jobs = append(res.Jobs, ListedJob{
			Record:  r,
			Started: r.StartedAt.Local().Format("2006-01-02 15:04:05"),
			Elapsed: formatElapsed(now.Sub(r.StartedAt)),
		})
	}
	return res, nil
}

// RenderList writes jobs in the traditional tabular form.
func RenderList(w io.Writer, res ListResult) error {
	if len(res.Jobs) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PID\tUSER\tSTARTED\tELAPSED\tCMD")
	for _, r := range res.Jobs {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
			r.Record.PID, r.Record.User, r.Started, r.Elapsed, r.Record.Cmd)
	}
	return tw.Flush()
}

// ForegroundJob sends SIGCONT to a recorded job and waits until it exits.
func ForegroundJob(ctx context.Context, reg *JobRegistry, pid int) (JobResult, error) {
	rec, err := getRegisteredJob(reg, pid)
	if err != nil {
		return JobResult{}, err
	}
	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil && !errors.Is(err, syscall.ESRCH) {
		return JobResult{}, fmt.Errorf("continue pid %d: %w", pid, err)
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return JobResult{}, ctx.Err()
		case <-ticker.C:
			if err := syscall.Kill(pid, 0); err != nil {
				if errors.Is(err, syscall.ESRCH) {
					_ = reg.Delete(pid)
					return JobResult{PID: pid, Record: rec, Signal: "CONT", Deleted: true}, nil
				}
				return JobResult{}, fmt.Errorf("poll pid %d: %w", pid, err)
			}
		}
	}
}

// BackgroundJob sends SIGCONT to a recorded job and returns immediately.
func BackgroundJob(reg *JobRegistry, pid int) (JobResult, error) {
	rec, err := getRegisteredJob(reg, pid)
	if err != nil {
		return JobResult{}, err
	}
	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			_ = reg.Delete(pid)
			return JobResult{PID: pid, Record: rec, Signal: "CONT", Deleted: true}, nil
		}
		return JobResult{}, fmt.Errorf("continue pid %d: %w", pid, err)
	}
	return JobResult{PID: pid, Record: rec, Signal: "CONT"}, nil
}

// KillJob sends a signal to a recorded job and deletes the registry entry.
func KillJob(reg *JobRegistry, pid int, signal string) (JobResult, error) {
	rec, err := getRegisteredJob(reg, pid)
	if err != nil {
		return JobResult{}, err
	}
	sig := syscall.SIGTERM
	sigName := "TERM"
	if signal != "" {
		sig, err = parseSignal(signal)
		if err != nil {
			return JobResult{}, err
		}
		sigName = signalName(signal)
	}
	if err := syscall.Kill(pid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			_ = reg.Delete(pid)
			return JobResult{PID: pid, Record: rec, Signal: sigName, Deleted: true}, nil
		}
		return JobResult{}, fmt.Errorf("kill pid %d: %w", pid, err)
	}
	if err := reg.Delete(pid); err != nil {
		return JobResult{
			PID:     pid,
			Record:  rec,
			Signal:  sigName,
			Deleted: false,
			Message: fmt.Sprintf("signal sent but registry cleanup failed: %v", err),
		}, nil
	}
	return JobResult{PID: pid, Record: rec, Signal: sigName, Deleted: true}, nil
}

// Command returns the jobs command group with jobs, fg, bg, and kill subcommands.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "Manage detached background jobs",
	}
	cmd.AddCommand(Commands()...)
	return cmd
}

// Commands returns jobs, fg, bg, and kill as sibling subcommands.
func Commands() []*cobra.Command {
	return []*cobra.Command{JobsCommand(), FgCommand(), BgCommand(), KillCommand()}
}

// JobsCommand returns the Cobra adapter for ListJobs.
func JobsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "jobs",
		Short: "List detached background jobs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := ListJobs(DefaultRegistry(), time.Now().UTC())
			if err != nil {
				return err
			}
			return RenderList(cmd.OutOrStdout(), res)
		},
	}
}

// FgCommand returns the Cobra adapter for ForegroundJob.
func FgCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "fg <pid>",
		Short: "Continue a recorded background job and wait for it to exit",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pid, err := parsePID(args[0])
			if err != nil {
				return err
			}
			_, err = ForegroundJob(cmd.Context(), DefaultRegistry(), pid)
			return err
		},
	}
}

// BgCommand returns the Cobra adapter for BackgroundJob.
func BgCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "bg <pid>",
		Short: "Continue a recorded background job",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			pid, err := parsePID(args[0])
			if err != nil {
				return err
			}
			_, err = BackgroundJob(DefaultRegistry(), pid)
			return err
		},
	}
}

// KillCommand returns the Cobra adapter for KillJob.
func KillCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "kill <pid> [SIGNAL]",
		Short: "Send a signal to a recorded background job and forget it",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			pid, err := parsePID(args[0])
			if err != nil {
				return err
			}
			sig := ""
			if len(args) == 2 {
				sig = args[1]
			}
			_, err = KillJob(DefaultRegistry(), pid, sig)
			if err != nil {
				return err
			}
			return nil
		},
	}
}

func getRegisteredJob(reg *JobRegistry, pid int) (JobRecord, error) {
	if pid <= 0 {
		return JobRecord{}, fmt.Errorf("invalid pid %d", pid)
	}
	rec, err := reg.Get(pid)
	if err != nil {
		return JobRecord{}, fmt.Errorf("no recorded job for pid %d", pid)
	}
	return rec, nil
}

func parsePID(s string) (int, error) {
	pid, err := strconv.Atoi(s)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid pid %q", s)
	}
	return pid, nil
}

func parseSignal(s string) (syscall.Signal, error) {
	if n, err := strconv.Atoi(s); err == nil {
		if n <= 0 {
			return 0, fmt.Errorf("invalid signal number %d", n)
		}
		return syscall.Signal(n), nil
	}
	switch signalName(s) {
	case "HUP":
		return syscall.SIGHUP, nil
	case "INT":
		return syscall.SIGINT, nil
	case "QUIT":
		return syscall.SIGQUIT, nil
	case "TERM":
		return syscall.SIGTERM, nil
	case "KILL":
		return syscall.SIGKILL, nil
	case "USR1":
		return syscall.SIGUSR1, nil
	case "USR2":
		return syscall.SIGUSR2, nil
	case "STOP":
		return syscall.SIGSTOP, nil
	case "CONT":
		return syscall.SIGCONT, nil
	}
	return 0, fmt.Errorf("unknown signal %q (try HUP, INT, TERM, KILL, USR1, USR2, STOP, CONT, or a number)", s)
}

func signalName(s string) string {
	name := strings.ToUpper(strings.TrimSpace(s))
	return strings.TrimPrefix(name, "SIG")
}

func formatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
