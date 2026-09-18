// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package weave

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/atlas"
)

// The issue GRAPH and the SDLC STAGE — the two things a flat queue cannot say.
//
// # Stage (D1)
//
// Every weave issue used to be, implicitly, a CODE issue: the item carried 30-odd
// fields — priority, points, tool, owner, branch, verify, suite-gate, dirty counts —
// and not one of them said what KIND of work it was. So "review the chunking design"
// and "triage the flaky fixture" and "promote to staging" had nowhere to live, and
// the board could only ever describe the middle of the lifecycle.
//
// The vocabulary is NOT re-declared here. It is atlas.Stages() — the same closed set
// every front-door VERB must already declare. A second list would drift from the
// first within a release, and the whole point of the axis is that one word means one
// thing everywhere.
//
// # Graph (D2)
//
// `weave add --points` has always told the user "8 = ~30m cap — SPLIT bigger work",
// and then offered no way to split: you hand-added children and the parent link was
// lost forever. Meanwhile the conductor's central rule — schedule by PARALLEL SAFETY —
// was carried in the conductor's head, because nothing on the item could say "#7 needs
// #3's code first". A resumed conductor, or a second agent, could not know it.
//
// Three fields fix both: Stage, Parent, DependsOn.

// weaveStages is the set a WORK ITEM may declare.
//
// It is atlas.Stages() minus "cross". That subtraction is deliberate: a VERB may serve
// every stage (`git` is used while planning, coding, testing and deploying), but a unit
// of WORK cannot BE every stage. "Cross" on an issue would mean "unclassified", which
// is exactly the silence this axis exists to break.
func weaveStages() []string {
	out := make([]string, 0, 4)
	for _, s := range atlas.Stages() {
		if s != atlas.StageCross {
			out = append(out, s)
		}
	}
	return out
}

// defaultStage: an empty Stage reads as "code".
//
// This is what makes the change migration-free. Every queue that exists today was, by
// construction, a queue of coding issues — so the zero value is not a guess, it is the
// truth about the existing data.
const defaultStage = atlas.StageCode

func itemStage(it *weaveItem) string {
	if it.Stage == "" {
		return defaultStage
	}
	return it.Stage
}

func validWeaveStage(s string) bool {
	for _, v := range weaveStages() {
		if s == v {
			return true
		}
	}
	return false
}

func normalizeStage(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", nil // recorded as empty; reads as defaultStage
	}
	if s == atlas.StageCross {
		return "", fmt.Errorf("stage %q is for VERBS, not work: a command can serve every stage, a unit of work cannot BE every stage (use one of: %s)",
			s, strings.Join(weaveStages(), ", "))
	}
	if !validWeaveStage(s) {
		return "", fmt.Errorf("unknown stage %q (want one of: %s)", s, strings.Join(weaveStages(), ", "))
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// The graph
// ---------------------------------------------------------------------------

func weaveFindItem(q *weaveQueue, id int64) *weaveItem {
	for _, it := range q.Items {
		if it.ID == id {
			return it
		}
	}
	return nil
}

func weaveChildren(q *weaveQueue, id int64) []*weaveItem {
	var out []*weaveItem
	for _, it := range q.Items {
		if it.Parent == id {
			out = append(out, it)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// weaveIsContainer reports whether an item was split into children.
//
// A container is NOT claimable — an agent works the children, never the parent. Note
// there is no "split" STATE: the parent keeps whatever state it had, and containerhood
// is derived from the existence of children. That is deliberate. A new stored state
// would have to be threaded through every transition (start, pull, salvage, prune,
// abandon, the reporter) and every one of those is a place to get it wrong. Derived
// facts cannot go stale.
func weaveIsContainer(q *weaveQueue, it *weaveItem) bool {
	return len(weaveChildren(q, it.ID)) > 0
}

// weaveDerivedState is what a container's state MEANS, computed from its children.
func weaveDerivedState(q *weaveQueue, it *weaveItem) string {
	kids := weaveChildren(q, it.ID)
	if len(kids) == 0 {
		return it.State
	}
	done := 0
	working := false
	for _, k := range kids {
		switch k.State {
		case "done":
			done++
		case "working", "allocated", "paused":
			working = true
		}
	}
	switch {
	case done == len(kids):
		return "done"
	case working:
		return "working"
	}
	return "todo"
}

// weaveBlockers returns the dependencies that are NOT yet satisfied.
//
// A dependency is satisfied ONLY when it reaches "done" — i.e. merged into the base
// branch. Not "submitted": submitted means the branch exists and is awaiting `weave
// pull`, so its code is not in main yet, and a dependent workspace — which is cloned
// FROM main — would not contain it. Starting the dependent then would hand an agent a
// tree without the code it was told to build on, which is precisely the failure the
// dependency was declared to prevent.
//
// A dep that is dead (failed/killed/abandoned) blocks too, and is reported as dead
// rather than silently pending. A visible deadlock a human can unlink is strictly
// better than an invisible one that looks like an empty queue.
func weaveBlockers(q *weaveQueue, it *weaveItem) []*weaveItem {
	var out []*weaveItem
	for _, id := range it.DependsOn {
		dep := weaveFindItem(q, id)
		if dep == nil {
			continue // a dangling dep cannot block; `weave link` refuses to create one
		}
		if dep.State != "done" {
			out = append(out, dep)
		}
	}
	return out
}

func weaveDepDead(dep *weaveItem) bool {
	switch dep.State {
	case "failed", "killed", "no-op", "abandoned":
		return true
	}
	return false
}

// weaveClaimable reports whether nextTodo may hand this item to an agent.
func weaveClaimable(q *weaveQueue, it *weaveItem) bool {
	return it.State == "todo" &&
		!weaveIsContainer(q, it) &&
		len(weaveBlockers(q, it)) == 0
}

// weaveWouldCycle reports whether making `it` depend on `dep` closes a loop.
//
// Without this, `weave link 1 --depends-on 2` followed by `weave link 2 --depends-on 1`
// makes both items permanently unclaimable and the queue looks empty with work in it.
func weaveWouldCycle(q *weaveQueue, itID, depID int64) bool {
	if itID == depID {
		return true
	}
	seen := map[int64]bool{}
	var reaches func(from int64) bool
	reaches = func(from int64) bool {
		if from == itID {
			return true
		}
		if seen[from] {
			return false
		}
		seen[from] = true
		f := weaveFindItem(q, from)
		if f == nil {
			return false
		}
		for _, next := range f.DependsOn {
			if reaches(next) {
				return true
			}
		}
		return false
	}
	return reaches(depID)
}

// ---------------------------------------------------------------------------
// weave split
// ---------------------------------------------------------------------------

func newWeaveSplitCmd() *cobra.Command {
	var flags weaveOutputFlags
	var into []string
	cmd := &cobra.Command{
		Use:   "split <id> --into <title> [--into <title>...]",
		Short: "Break an oversized issue into children, keeping the link to the parent",
		Long: `split turns one issue into a parent (an epic) and N children.

weave has always TOLD you to do this -- 'weave add --points' says "8 = ~30m cap,
split bigger work" -- while giving you no way to do it. Hand-adding the children
worked, but the link to the parent was lost the moment you did, so nothing could
answer "what was this decomposed from?" or "is the epic finished?".

The parent becomes a CONTAINER: it is never claimed by an agent, and its state is
derived from its children (done when they all are). Children inherit the parent's
stage, priority, verify command and suite gate -- the decomposition of a task does
not change what "correct" means for it.`,
		Example: `  weave split 7 --into "extract the Pool type" --into "wire the SSH transport"
  weave split 7 --into "..." --points 3`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			points, _ := cmd.Flags().GetInt("points")
			return runWeaveSplit(cmd, args[0], into, points, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().StringArrayVar(&into, "into", nil, "a child issue title (repeatable)")
	cmd.Flags().Int("points", 0, "story points for each child (1,2,3,5,8)")
	return cmd
}

func runWeaveSplit(cmd *cobra.Command, idArg string, into []string, points int, flags *weaveOutputFlags) error {
	mode := flags.mode()
	id, err := parseWeaveID(idArg)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave split", weavecli.ExitInvalidArg, err))
	}
	if len(into) == 0 {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave split", weavecli.ExitInvalidArg,
			fmt.Errorf("--into is required: a split with no children is a no-op")))
	}
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave split", weavecli.ExitPrecondFail, err))
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave split", weavecli.ExitGenericFail, err))
	}

	var kids []*weaveItem
	var parent *weaveItem
	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		parent = weaveFindItem(q, id)
		if parent == nil {
			return fmt.Errorf("run #%d not found", id)
		}
		// Splitting work an agent is actively doing would orphan its workspace.
		if parent.State != "todo" {
			return fmt.Errorf("run #%d is %s: only a todo issue can be split (its work has already started)", id, parent.State)
		}
		now := timeNowUTC()
		for _, title := range into {
			title = strings.TrimSpace(title)
			if title == "" {
				continue
			}
			kid := &weaveItem{
				ID:       q.NextID,
				Title:    title,
				Parent:   parent.ID,
				Stage:    parent.Stage,
				Priority: parent.Priority,
				Points:   points,
				State:    "todo",
				// Correctness does not change when a task is decomposed: each child
				// must clear the same bar the parent would have had to clear.
				VerifyCommand: parent.VerifyCommand,
				SuiteGate:     parent.SuiteGate,
				Created:       now,
			}
			q.NextID++
			q.Items = append(q.Items, kid)
			kids = append(kids, kid)
		}
		return nil
	})
	if lockErr != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave split", weavecli.ExitGenericFail, lockErr))
	}

	if mode == weavecli.OutputJSON {
		ids := make([]int64, 0, len(kids))
		for _, k := range kids {
			ids = append(ids, k.ID)
		}
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave split", map[string]any{
			"parent":   parent.ID,
			"children": ids,
			"stage":    itemStage(parent),
		}))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "weave split: #%d is now an epic (%d children)\n", parent.ID, len(kids))
	for _, k := range kids {
		fmt.Fprintf(cmd.OutOrStdout(), "  #%-4d %s\n", k.ID, k.Title)
	}
	return nil
}

// ---------------------------------------------------------------------------
// weave link
// ---------------------------------------------------------------------------

func newWeaveLinkCmd() *cobra.Command {
	var flags weaveOutputFlags
	var dependsOn, blocks []int64
	var unlink bool
	cmd := &cobra.Command{
		Use:   "link <id> --depends-on <id> | --blocks <id>",
		Short: "Record that one issue needs another first (parallel safety, as data)",
		Long: `link records a dependency between two issues, so the queue itself knows what
may run in parallel and what may not.

This is the conductor's central rule -- SCHEDULE BY PARALLEL SAFETY -- moved out of
the conductor's head and into the data. Until now, "issue #7 must not start before
#3 lands" existed only as a sentence in a plan: a resumed conductor could not know
it, and a second agent could not see it. Now 'weave next' will not hand out a
blocked issue at all.

A dependency is satisfied only when the other issue is DONE -- merged. Not
"submitted": a submitted branch is not in main yet, and the dependent's workspace is
cloned FROM main, so it would not contain the code it was told to build on.

Cycles are refused: two issues that wait on each other are two issues that never run,
and the queue would look empty while holding work.`,
		Example: `  weave link 7 --depends-on 3        # 7 waits for 3
  weave link 3 --blocks 7           # the same fact, said the other way
  weave link 7 --depends-on 3 --unlink`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWeaveLink(cmd, args[0], dependsOn, blocks, unlink, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().Int64SliceVar(&dependsOn, "depends-on", nil, "this issue cannot start until <id> is done (repeatable)")
	cmd.Flags().Int64SliceVar(&blocks, "blocks", nil, "<id> cannot start until this issue is done (repeatable)")
	cmd.Flags().BoolVar(&unlink, "unlink", false, "remove the dependency instead of adding it")
	return cmd
}

func runWeaveLink(cmd *cobra.Command, idArg string, dependsOn, blocks []int64, unlink bool, flags *weaveOutputFlags) error {
	mode := flags.mode()
	id, err := parseWeaveID(idArg)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave link", weavecli.ExitInvalidArg, err))
	}
	if len(dependsOn) == 0 && len(blocks) == 0 {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave link", weavecli.ExitInvalidArg,
			fmt.Errorf("--depends-on or --blocks is required")))
	}
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave link", weavecli.ExitPrecondFail, err))
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave link", weavecli.ExitGenericFail, err))
	}

	type edge struct{ from, to int64 } // from depends on to
	var edges []edge
	for _, d := range dependsOn {
		edges = append(edges, edge{from: id, to: d})
	}
	for _, b := range blocks {
		edges = append(edges, edge{from: b, to: id})
	}

	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		for _, e := range edges {
			from := weaveFindItem(q, e.from)
			if from == nil {
				return fmt.Errorf("run #%d not found", e.from)
			}
			if weaveFindItem(q, e.to) == nil {
				return fmt.Errorf("run #%d not found", e.to)
			}
			if unlink {
				from.DependsOn = removeID(from.DependsOn, e.to)
				continue
			}
			if containsID(from.DependsOn, e.to) {
				continue
			}
			if weaveWouldCycle(q, e.from, e.to) {
				return fmt.Errorf("#%d depending on #%d would create a cycle: both issues would wait forever and the queue would look empty while holding work",
					e.from, e.to)
			}
			from.DependsOn = append(from.DependsOn, e.to)
			sort.Slice(from.DependsOn, func(i, j int) bool { return from.DependsOn[i] < from.DependsOn[j] })
		}
		return nil
	})
	if lockErr != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave link", weavecli.ExitGenericFail, lockErr))
	}

	verb := "linked"
	if unlink {
		verb = "unlinked"
	}
	if mode == weavecli.OutputJSON {
		out := make([]map[string]any, 0, len(edges))
		for _, e := range edges {
			out = append(out, map[string]any{"issue": e.from, "depends_on": e.to})
		}
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave link", map[string]any{
			"action": verb,
			"edges":  out,
		}))
	}
	for _, e := range edges {
		fmt.Fprintf(cmd.OutOrStdout(), "weave link: %s — #%d depends on #%d\n", verb, e.from, e.to)
	}
	return nil
}

func parseWeaveID(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(s), "#"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid issue id %q", s)
	}
	return id, nil
}

func timeNowUTC() time.Time { return time.Now().UTC() }

func containsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func removeID(ids []int64, id int64) []int64 {
	out := ids[:0]
	for _, v := range ids {
		if v != id {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
