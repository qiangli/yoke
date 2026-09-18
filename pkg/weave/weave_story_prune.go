package weave

// `sprint prune` — ONE command for "is this sprint clean, and make it so".
//
// It has two modes and the SAFE one is the default:
//
//	sprint prune <id>            REPORT. Reads everything, changes nothing.
//	sprint prune <id> --apply    RECLAIM. Removes only what is provably redundant.
//
// Report-by-default is what makes it usable as the standing consistency
// question. Another agent about to rebuild a shared repo, a reviewer sizing up
// a branch, a second sprint touching the same checkout — all of them need to
// ask "what state is this in" without the risk of changing it, and without
// needing the conductor's permission or a second verb to learn.
//
// # What it will and will not remove
//
// Reclaim only ever removes a POINTER to bytes that already live somewhere
// else. Concretely:
//
//	branch      only when every patch is already upstream — ancestor of base, or
//	            `git cherry` reports no '+' lines (the rebase/squash case)
//	worktree    only when it is clean, or its directory is already gone
//	workspace   only for a terminal run whose commits are merged
//	socket/log  only for a run that has reached a terminal state
//
// A branch with one unique commit, a dirty worktree, an unmerged run: left
// alone and REPORTED. The umbrella's cleanup policy has said exactly this in
// prose for a long time while nothing implemented it — no `git cherry` and no
// worktree handling existed anywhere in the tree. This is that policy, executed.
//
// # It never merges
//
// Merging inside a cleanup verb is how an unreviewed merge lands: the operator
// typing `prune` is thinking about disk, which is the worst frame of mind for
// approving code. Unmerged work is reported with the gated verb that settles it
// (`weave pull`, `weave salvage`) and prune declines to touch it.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// sprintPruneAction is one thing prune did, or would do.
type sprintPruneAction struct {
	Kind          string `json:"kind"` // branch | worktree | workspace | socket | log
	Repo          string `json:"repo,omitempty"`
	Target        string `json:"target"`
	Done          bool   `json:"done"`
	Err           string `json:"error,omitempty"`
	ExpectedBytes uint64 `json:"expected_bytes"`
	ActualBytes   uint64 `json:"actual_bytes"`
	BytesComplete bool   `json:"bytes_complete"`
	ByteKind      string `json:"byte_kind,omitempty"`
}

func newWeaveStoryPruneCmd() *cobra.Command {
	var flags weaveOutputFlags
	var apply bool
	cmd := &cobra.Command{
		Use:   "prune <sprint>",
		Short: "Report a sprint's clean state across its repos and this host — and reclaim what is redundant",
		Long: `prune answers "is this sprint clean" and, with --apply, makes it so.

Without --apply it CHANGES NOTHING. That is the default on purpose: the
question is asked constantly by people who are not the conductor — another
agent about to rebuild a shared repo, a reviewer deciding whether a branch is
safe, a second sprint in the same checkout — and a consistency check nobody
can run safely is a consistency check nobody runs.

It reports three things:

  UNCLEAN       uncommitted files, unpushed commits, runs still working,
                unmerged commits, decisions owed. These BLOCK a clean close.
  reclaimable   workspaces, worktrees, branches, sockets and logs that are
                redundant. Untidy, never unsafe.
  UNCOVERED     open stories the sprint's plan does not mention, and any
                reachability problem (no owner, no room, an owner that
                resolves to no agent).

--apply removes ONLY what is provably redundant: a branch whose every patch is
already upstream (ancestor of base, or git cherry shows no unique patch), a
worktree that is clean or already gone, a workspace whose run is terminal and
merged. A branch with one unique commit is left alone and reported.

prune NEVER merges. Unmerged work is reported with the gated verb that settles
it — weave pull, weave salvage — because approving code and reclaiming disk are
different decisions and should not share a keystroke.`,
		Args: cobra.MaximumNArgs(1),
		Example: "  bashy sprint prune               # every open sprint, report only\n" +
			"  bashy sprint prune 99            # one sprint, report only\n" +
			"  bashy sprint prune 99 --apply    # reclaim what is redundant\n" +
			"  bashy sprint prune --json        # for an agent with no context",
		RunE: func(cmd *cobra.Command, args []string) error {
			// NO ID IS A VALID CALL. The agent most likely to need this is the
			// one with the least context: a worker that just cloned a repo, a
			// reviewer who wandered in, anything that wants to know the state
			// before it builds. Making it name a sprint number first would put
			// the check behind exactly the knowledge it exists to supply.
			if len(args) == 0 {
				return runSprintPruneAll(cmd, apply, &flags)
			}
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("sprint must be an integer: %q", args[0])
			}
			return runSprintPrune(cmd, id, apply, &flags)
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "actually reclaim (default: report only)")
	flags.attach(cmd)
	return cmd
}

func runSprintPrune(cmd *cobra.Command, id int64, apply bool, flags *weaveOutputFlags) error {
	mode := flags.mode()
	op := "sprint prune"
	dir, err := weaveStoryDir(cmd, mode, op)
	if err != nil {
		return err
	}
	q, lerr := loadWeaveQueue(dir)
	if lerr != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitGenericFail, lerr))
	}
	s := findWeaveStory(q, id)
	if s == nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitInvalidArg,
			fmt.Errorf("sprint #%d not found", id)))
	}

	hy := sprintCheckHygiene(s)
	reach := sprintCheckReachability(s)
	coverage := sprintCoverageProblems(s)

	var actions []sprintPruneAction
	if apply {
		actions = sprintApplyPrune(s, hy)
	}

	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, op, map[string]any{
			"sprint":       id,
			"applied":      apply,
			"clean":        hy.Clean(),
			"repos":        hy.Repos,
			"problems":     hy.Problems,
			"reclaimable":  hy.Reclaimable,
			"uncovered":    coverage,
			"reachability": reach,
			"actions":      actions,
			"reclaim_plan": sprintPlanRunArtifacts(s),
		}))
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "sprint #%d — %s\n", id, s.Title)
	if len(hy.Repos) > 0 {
		fmt.Fprintf(out, "  repos: %s\n", strings.Join(hy.Repos, ", "))
	}
	renderSprintHygiene(out, hy)
	if !apply {
		for _, a := range sprintPlanRunArtifacts(s) {
			fmt.Fprintf(out, "  estimate %s %s: %d apparent bytes (complete=%t)\n", a.Kind, a.Target, a.ExpectedBytes, a.BytesComplete)
		}
	}
	renderSprintReachability(out, s)
	for _, c := range coverage {
		fmt.Fprintf(out, "  UNCOVERED: %s\n", c)
	}
	for _, a := range actions {
		switch {
		case a.Err != "":
			fmt.Fprintf(out, "  failed %s %s: %s\n", a.Kind, a.Target, a.Err)
		default:
			fmt.Fprintf(out, "  removed %s %s\n", a.Kind, a.Target)
		}
	}
	switch {
	case !apply && len(hy.Reclaimable) > 0:
		fmt.Fprintf(out, "  %d item(s) reclaimable — `bashy sprint prune %d --apply`\n", len(hy.Reclaimable), id)
	case hy.Clean() && len(coverage) == 0 && len(reach.Problems) == 0:
		fmt.Fprintln(out, "  clean")
	}
	return nil
}

// sprintApplyPrune removes what the hygiene pass proved redundant.
//
// It RE-CHECKS each target at the moment of removal rather than trusting the
// report it was handed. The report was taken without a lock and a repo is a
// shared thing: a branch that was integrated when we looked may have gained a
// commit since, and acting on a stale reading is how cleanup deletes work.
func sprintApplyPrune(s *weaveStory, hy sprintHygiene) []sprintPruneAction {
	// Repository-wide stale worktrees and integrated branches are observations,
	// not proof of this sprint's ownership. Only explicit linked run artifacts
	// are eligible for mutation; a competitor's checkout is never swept.
	return sprintPruneRunArtifacts(s)
}

func sprintPruneRunArtifacts(s *weaveStory) []sprintPruneAction {
	var acts []sprintPruneAction
	if s == nil {
		return acts
	}
	for _, run := range s.Runs {
		dir, err := weaveQueueDirForSprintRun(run)
		if err != nil {
			continue
		}
		acts = append(acts, weavePruneOwnedRun(dir, run.ID, run.Repo, run.Born)...)
	}
	return acts
}

// runSprintPruneAll reports every OPEN sprint on the board.
//
// This is the no-argument entry point, and it is the one most callers should
// use. It needs no sprint id, no conductor lease and no prior context: an agent
// that just arrived can ask what state the machine is in, and get an answer
// naming the sprint, the repos, and the verb that fixes each finding.
//
// Only open sprints are walked. A done sprint's leftovers are still reported by
// naming it explicitly; sweeping them by default would bury today's actionable
// findings under months of closed work.
func runSprintPruneAll(cmd *cobra.Command, apply bool, flags *weaveOutputFlags) error {
	mode := flags.mode()
	op := "sprint prune"
	dir, err := weaveStoryDir(cmd, mode, op)
	if err != nil {
		return err
	}
	q, lerr := loadWeaveQueue(dir)
	if lerr != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitGenericFail, lerr))
	}
	var open []*weaveStory
	for _, s := range q.Stories {
		if sprintColumnOpen(s.Column) {
			open = append(open, s)
		}
	}
	if len(open) == 0 {
		if mode == weavecli.OutputJSON {
			return ec(emitOK(cmd.OutOrStdout(), mode, op, map[string]any{"sprints": []any{}, "clean": true}))
		}
		fmt.Fprintln(cmd.OutOrStdout(), "no open sprints")
		return nil
	}
	var reports []map[string]any
	out := cmd.OutOrStdout()
	allClean := true
	for _, s := range open {
		hy := sprintCheckHygiene(s)
		reach := sprintCheckReachability(s)
		coverage := sprintCoverageProblems(s)
		var actions []sprintPruneAction
		if apply {
			actions = sprintApplyPrune(s, hy)
		}
		clean := hy.Clean() && len(coverage) == 0 && len(reach.Problems) == 0
		if !clean {
			allClean = false
		}
		if mode == weavecli.OutputJSON {
			reports = append(reports, map[string]any{
				"sprint": s.ID, "title": s.Title, "clean": clean,
				"repos": hy.Repos, "problems": hy.Problems,
				"reclaimable": hy.Reclaimable, "uncovered": coverage,
				"reachability": reach, "actions": actions, "reclaim_plan": sprintPlanRunArtifacts(s),
			})
			continue
		}
		fmt.Fprintf(out, "sprint #%d — %s\n", s.ID, s.Title)
		renderSprintHygiene(out, hy)
		renderSprintReachability(out, s)
		for _, c := range coverage {
			fmt.Fprintf(out, "  UNCOVERED: %s\n", c)
		}
		for _, a := range actions {
			if a.Err != "" {
				fmt.Fprintf(out, "  failed %s %s: %s\n", a.Kind, a.Target, a.Err)
			} else {
				fmt.Fprintf(out, "  removed %s %s\n", a.Kind, a.Target)
			}
		}
		if clean && len(hy.Reclaimable) == 0 {
			fmt.Fprintln(out, "  clean")
		}
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, op, map[string]any{"sprints": reports, "clean": allClean}))
	}
	if !apply {
		fmt.Fprintln(out, "report only — `bashy sprint prune --apply` to reclaim, or name a sprint for detail")
	}
	return nil
}
