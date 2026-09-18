package weave

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// `weave doctor` — run the reaper on demand and report the lifecycle audit.
//
// The reaper also runs on `weave list` and each heartbeat tick, so doctor is
// rarely the thing that FIXES a queue. Its job is to make the invariant
// inspectable: what did the machine just close, and for everything still open,
// what is the named next step. A queue in which some item's answer is "nothing"
// is a queue with a limbo in it, and doctor is where that shows up.

func newWeaveDoctorCmd() *cobra.Command {
	var flags weaveOutputFlags
	thresholds := defaultWeaveDoctorThresholds()
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Reap limbo states and audit the lifecycle of every open item",
		Long: `doctor runs the lifecycle REAPER over this repo's queue and then audits
what is left.

The reaper gives every stuck state a determinate exit:

  allocated whose launcher died      -> failed
  finalizing whose conductor died    -> working (wrapper alive) or failed
  working whose wrapper pid is dead  -> failed (wrapper-died)
  submitted already merged into base -> done
  submitted past the threshold       -> flagged needs-steward
  failed/killed sitting on commits   -> flagged salvageable

It NEVER destroys committed work: it writes state fields and flags only.
Removing a workspace stays an explicit guarded step (` + "`weave prune`" + `,
` + "`weave abandon`" + `). It never promotes a crash to success either — a dead
wrapper becomes failed, and any work behind it is surfaced, not merged.

The audit then lists every item that is not closed (done/abandoned) with the
next step that will close it. Every state has one; see
docs/weave-lifecycle-state-machine.md for the whole machine.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if thresholds.Todo < 0 || thresholds.Submitted < 0 || thresholds.Allocated < 0 {
				return fmt.Errorf("age thresholds must be non-negative (0 disables a check)")
			}
			return runWeaveDoctorWithOptions(cmd, &flags, thresholds, time.Now)
		},
	}
	flags.attach(cmd)
	cmd.Flags().DurationVar(&thresholds.Todo, "todo-after", thresholds.Todo, "Flag todo/queued work untouched longer than this (0 disables)")
	cmd.Flags().DurationVar(&thresholds.Submitted, "submitted-after", thresholds.Submitted, "Flag submitted work unmerged longer than this (0 disables)")
	cmd.Flags().DurationVar(&thresholds.Allocated, "allocated-after", thresholds.Allocated, "Flag allocated work unlaunched longer than this (0 disables)")
	return cmd
}

const (
	weaveDoctorDefaultTodoAfter      = 168 * time.Hour
	weaveDoctorDefaultSubmittedAfter = 4 * time.Hour
	weaveDoctorDefaultAllocatedAfter = 30 * time.Minute
)

type weaveDoctorThresholds struct {
	Todo      time.Duration
	Submitted time.Duration
	Allocated time.Duration
}

func defaultWeaveDoctorThresholds() weaveDoctorThresholds {
	return weaveDoctorThresholds{
		Todo: weaveDoctorDefaultTodoAfter, Submitted: weaveDoctorDefaultSubmittedAfter,
		Allocated: weaveDoctorDefaultAllocatedAfter,
	}
}

// weaveOpenItem is one not-yet-closed item plus the named step that closes it.
type weaveOpenItem struct {
	Issue      int64    `json:"issue"`
	State      string   `json:"state"`
	Title      string   `json:"title,omitempty"`
	AgeSeconds int64    `json:"age_seconds"`
	NextSteps  string   `json:"next_steps"`
	Flags      []string `json:"flags,omitempty"`
}

func runWeaveDoctor(cmd *cobra.Command, flags *weaveOutputFlags) error {
	return runWeaveDoctorWithOptions(cmd, flags, defaultWeaveDoctorThresholds(), time.Now)
}

func runWeaveDoctorWithOptions(cmd *cobra.Command, flags *weaveOutputFlags, thresholds weaveDoctorThresholds, now func() time.Time) error {
	mode := flags.mode()
	const op = "weave doctor"

	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitPrecondFail, err))
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitGenericFail, err))
	}
	base := weaveBaseBranch(root)
	// doctor's whole job is the reap + the report, so unlike list/show it
	// waits for the lock rather than skipping the pass.
	actions, err := weaveReapQueueBlocking(dir, root, base)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitGenericFail, err))
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitGenericFail, err))
	}

	open := weaveDoctorOpenItems(q, thresholds, now().UTC())

	if mode == weavecli.OutputJSON {
		reaped := make([]map[string]any, 0, len(actions))
		for _, a := range actions {
			reaped = append(reaped, map[string]any{
				"issue": a.Issue, "from": a.From, "to": a.To, "reason": a.Reason, "flag": a.Flag,
			})
		}
		return ec(emitOK(cmd.OutOrStdout(), mode, op, map[string]any{
			"reaped": reaped,
			"open":   open,
		}))
	}

	w := cmd.OutOrStdout()
	if len(actions) == 0 {
		fmt.Fprintln(w, "reaped: nothing (no item was in a stuck state)")
	} else {
		fmt.Fprintf(w, "reaped (%d):\n", len(actions))
		for _, a := range actions {
			fmt.Fprintf(w, "  %s\n", a)
		}
	}
	if len(open) == 0 {
		fmt.Fprintln(w, "open: none — every item is closed (done/abandoned)")
		return nil
	}
	fmt.Fprintf(w, "open (%d) — each with the step that closes it:\n", len(open))
	fmt.Fprintf(w, "  %-5s %-11s %-9s %s\n", "ISSUE", "STATE", "AGE", "NEXT STEP")
	for _, o := range open {
		flag := ""
		if len(o.Flags) > 0 {
			flag = " [" + strings.Join(o.Flags, ", ") + "]"
		}
		fmt.Fprintf(w, "  #%-4d %-11s %-9s%s %s\n", o.Issue, o.State, weaveDoctorRoundedAge(o.AgeSeconds), flag, o.NextSteps)
	}
	return nil
}

func weaveDoctorOpenItems(q *weaveQueue, thresholds weaveDoctorThresholds, now time.Time) []weaveOpenItem {
	open := make([]weaveOpenItem, 0, len(q.Items))
	for _, it := range weaveLimboItems(q) {
		age := weaveDoctorItemAge(it, now)
		row := weaveOpenItem{Issue: it.ID, State: it.State, Title: it.Title, AgeSeconds: int64(age / time.Second), NextSteps: weaveNextSteps(it)}
		if it.NeedsSteward {
			row.Flags = append(row.Flags, "needs-steward")
		}
		if it.Salvageable {
			row.Flags = append(row.Flags, "salvageable")
		}
		if weaveDoctorItemStale(it, age, thresholds) {
			row.Flags = append(row.Flags, "stale")
		}
		open = append(open, row)
	}
	return open
}

func weaveDoctorItemAge(it *weaveItem, now time.Time) time.Duration {
	since := it.Created
	if it.State == "submitted" && !it.FinishedAt.IsZero() {
		since = it.FinishedAt
	}
	if since.IsZero() || now.Before(since) {
		return 0
	}
	return now.Sub(since)
}

func weaveDoctorItemStale(it *weaveItem, age time.Duration, thresholds weaveDoctorThresholds) bool {
	var threshold time.Duration
	switch it.State {
	case "todo", "queued":
		threshold = thresholds.Todo
	case "submitted":
		threshold = thresholds.Submitted
	case "allocated":
		threshold = thresholds.Allocated
	}
	return threshold > 0 && age > threshold
}

func weaveDoctorStaleCount(q *weaveQueue, thresholds weaveDoctorThresholds, now time.Time) int {
	n := 0
	for _, it := range weaveLimboItems(q) {
		if weaveDoctorItemStale(it, weaveDoctorItemAge(it, now), thresholds) {
			n++
		}
	}
	return n
}

func weaveDoctorRoundedAge(seconds int64) string {
	return weaveRoundDuration(time.Duration(seconds) * time.Second).String()
}

// weaveNextSteps names what will close an item from its current state. It is
// derived from the declared transition table, not hand-maintained prose, so a
// state added without a way out cannot quietly acquire a plausible-sounding
// answer here — it gets the empty set, and TestLifecycleHasNoLimbo fails.
func weaveNextSteps(it *weaveItem) string {
	if it.NeedsSteward && it.StewardReason != "" {
		return it.StewardReason
	}
	if it.Salvageable {
		return fmt.Sprintf("committed work survives this %s run — inspect it, then `weave salvage %d`, or `weave abandon %d`", it.State, it.ID, it.ID)
	}
	// The declared first edge out of failed/killed is `weave start --resume`,
	// which is only real while the workspace is. When it is gone the run is
	// still recoverable — but by re-provisioning, not reattaching — so derive
	// the step from the ACTUAL on-disk state rather than the table's default.
	if (it.State == "failed" || it.State == "killed" || it.State == "no-op") && !weaveWorkspaceLive(it) {
		return fmt.Sprintf("%s -> allocated (weave start --run %d -- <agent>) — no workspace remains to resume", it.State, it.ID)
	}
	var steps []string
	for _, t := range weaveLifecycleTransitionsFrom(it.State) {
		steps = append(steps, fmt.Sprintf("%s -> %s (%s)", t.From, t.To, t.By))
	}
	if len(steps) == 0 {
		// Unreachable while the lifecycle test passes; loud rather than blank
		// if a state ever escapes the table.
		return "NO DECLARED TRANSITION — this is a limbo; see docs/weave-lifecycle-state-machine.md"
	}
	return steps[0]
}
