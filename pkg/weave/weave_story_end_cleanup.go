package weave

// `sprint end` residue (Sprint 224 S4).
//
// Ending a sprint is the last moment anyone holds the context to say what each
// run's work became. So end refuses a run nobody has decided about, reclaims
// every artifact the settled runs still own through the ONE guarded teardown,
// looks again, and closes only when nothing sprint-owned is left. The reclaim
// happens BEFORE the story mutation takes the queue lock: the runs may live in
// the very queue the mutation locks, and the teardown needs that lock itself.

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// sprintUndisposedRuns names every linked run whose work has no decision yet,
// each with the verb that makes one. A run that is terminal and provably
// settled (merged, empty, or salvaged) needs no explicit word — the teardown
// records it — so old rows from before the field existed do not block a close.
func sprintUndisposedRuns(s *weaveStory) []string {
	var out []string
	for _, run := range s.Runs {
		dir, err := weaveQueueDirForSprintRun(run)
		if err != nil {
			continue // hygiene already reports an unreadable queue
		}
		q, err := loadWeaveQueue(dir)
		if err != nil {
			continue
		}
		it := findWeaveItem(q, run.ID)
		if it == nil {
			continue
		}
		root, ok := weaveRepoRootForQueue(dir)
		if !ok {
			continue
		}
		if !isTerminalState(it.State) {
			out = append(out, fmt.Sprintf("run %s#%d is %s — let it finish, `bashy weave kill %d`, or `bashy weave abandon %d --disposition superseded`",
				run.Repo, run.ID, it.State, run.ID, run.ID))
			continue
		}
		if _, settled := weaveItemSettled(root, weaveBaseBranch(root), it); settled {
			continue
		}
		switch it.State {
		case "submitted":
			out = append(out, fmt.Sprintf("run %s#%d is submitted and undecided — accept: `bashy weave pull %d`; decline: `bashy weave abandon %d --disposition rejected|superseded --reason '<why>'`",
				run.Repo, run.ID, run.ID, run.ID))
		default:
			out = append(out, fmt.Sprintf("run %s#%d (%s) holds unmerged work — keep: `bashy weave salvage %d`; decline: `bashy weave abandon %d --disposition rejected|superseded --reason '<why>'`",
				run.Repo, run.ID, it.State, run.ID, run.ID))
		}
	}
	sort.Strings(out)
	return out
}

// sprintResidual is what would still be sprint-owned after the reclaim: an
// artifact the plan can still see, a branch name a run still owns, or a
// recorded cleanup failure. Empty means residual = 0.
func sprintResidual(s *weaveStory) []string {
	var out []string
	for _, a := range sprintPlanRunArtifacts(s) {
		out = append(out, fmt.Sprintf("%s %s", a.Kind, a.Target))
	}
	for _, run := range s.Runs {
		dir, err := weaveQueueDirForSprintRun(run)
		if err != nil {
			continue
		}
		q, err := loadWeaveQueue(dir)
		if err != nil {
			continue
		}
		it := findWeaveItem(q, run.ID)
		if it == nil {
			continue
		}
		if it.CleanupError != "" {
			out = append(out, fmt.Sprintf("run %s#%d cleanup failed: %s", run.Repo, run.ID, it.CleanupError))
		}
		root, ok := weaveRepoRootForQueue(dir)
		if !ok {
			continue
		}
		for _, name := range weaveRunBranchNames(it) {
			if _, exists := gitBranchTip(root, name); exists {
				out = append(out, fmt.Sprintf("branch %s in %s", name, root))
			}
		}
		for _, kind := range []string{"cache", "agent-data"} {
			if p := weaveRunArtifactPath(dir, it, kind); p != "" {
				if _, err := os.Lstat(p); err == nil {
					out = append(out, fmt.Sprintf("%s %s", kind, p))
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// sprintReclaimSummary renders what the reclaim did, by artifact kind, with
// apparent bytes — the number the operator asked for and the one that must
// never be read as physical free space.
func sprintReclaimSummary(actions []sprintPruneAction) (string, []string) {
	type tally struct {
		n     int
		bytes uint64
	}
	kinds := map[string]*tally{}
	var failures []string
	for _, a := range actions {
		if a.Err != "" {
			failures = append(failures, fmt.Sprintf("%s %s: %s", a.Kind, a.Target, a.Err))
			continue
		}
		if !a.Done {
			continue
		}
		t := kinds[a.Kind]
		if t == nil {
			t = &tally{}
			kinds[a.Kind] = t
		}
		t.n++
		t.bytes += a.ActualBytes
	}
	var parts []string
	for _, k := range []string{"workspace", "cache", "agent-data", "log", "socket", "lock", "branch"} {
		if t := kinds[k]; t != nil {
			if t.bytes > 0 {
				parts = append(parts, fmt.Sprintf("%d %s (%d apparent bytes)", t.n, k, t.bytes))
			} else {
				parts = append(parts, fmt.Sprintf("%d %s", t.n, k))
			}
		}
	}
	sort.Strings(failures)
	if len(parts) == 0 {
		return "nothing to reclaim", failures
	}
	return "reclaimed " + strings.Join(parts, ", "), failures
}

// weaveStoryRead is the lock-free read counterpart of runWeaveStoryMutate for
// work that must happen BEFORE the mutation takes the queue lock.
func weaveStoryRead(cmd *cobra.Command, flags *weaveOutputFlags, op string, id int64, fn func(*weaveStory)) error {
	mode := flags.mode()
	dir, err := weaveStoryDir(cmd, mode, op)
	if err != nil {
		return err
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitGenericFail, err))
	}
	s := findWeaveStory(q, id)
	if s == nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitInvalidArg, fmt.Errorf("sprint #%d not found", id)))
	}
	fn(s)
	return nil
}
