// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package weave

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/judge"
)

// weave supplies judge's run seam.
//
// The dependency points weave -> judge, never the reverse, so the conductor inside
// `weave autopilot` stays free to ask for a semantic review before it merges. See
// judge/run.go.
func init() {
	judge.RunReader = readRunForJudging
	judge.RunRecorder = recordJudgment
}

// readRunForJudging returns a run's work as text a reviewer can actually read: the DIFF
// its agent produced, not the run's metadata.
func readRunForJudging(id int64) (subject, content, stage string, err error) {
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return "", "", "", err
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		return "", "", "", err
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return "", "", "", err
	}
	it := weaveFindItem(q, id)
	if it == nil {
		return "", "", "", fmt.Errorf("run #%d not found", id)
	}
	if it.Branch == "" {
		return "", "", "", fmt.Errorf("run #%d has produced no branch yet (state: %s) — there is nothing to review",
			id, it.State)
	}
	base := it.BaseSHA
	if base == "" {
		base = "HEAD"
	}
	// The agent's work, and only the agent's work: everything on its branch since it
	// started. A plain `git diff HEAD` in the workspace would miss what it committed.
	out, err := exec.Command(gitBin(), "-C", root, "diff", base+".."+it.Branch).Output()
	if err != nil {
		return "", "", "", fmt.Errorf("reading run #%d's diff: %w", id, err)
	}
	subject = fmt.Sprintf("run #%d — %s", id, it.Title)
	if it.Body != "" {
		// The reviewer must know what the run was ASKED to do, or it can only judge
		// whether the code is nice, never whether it is right.
		content = "The task given to the agent was:\n\n" + it.Body + "\n\n--- ITS DIFF ---\n\n" + string(out)
	} else {
		content = string(out)
	}
	return subject, content, itemStage(it), nil
}

// recordJudgment writes the verdict onto the run.
//
// It fills ReviewVerdict / ReviewBlocking / ReviewNotes / ReviewBy / ReviewAt — fields
// that have existed on a weave item all along and were, until now, written only by a
// mechanical clean-room re-run of the verify command. That is why `weave review` was a
// misnomer: the item had a slot for a review and nothing that could produce one.
func recordJudgment(id int64, r judge.Report) error {
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return err
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		return err
	}
	return withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it := weaveFindItem(q, id)
		if it == nil {
			return fmt.Errorf("run #%d not found", id)
		}
		it.ReviewVerdict = string(r.Verdict)
		// Blocking records the OPINION, not a policy. Whether a non-approving verdict
		// actually stops a merge is the caller's choice (`judge --gate`) — an LLM
		// opinion is not reproducible, and wiring one straight into the merge path
		// would let a single hallucinated "reject" wedge the fleet.
		it.ReviewBlocking = r.Verdict != judge.Approve
		it.ReviewNotes = summarize(r)
		it.ReviewBy = strings.Join(agentsOf(r), ",")
		it.ReviewAt = time.Now().UTC()
		return nil
	})
}

func agentsOf(r judge.Report) []string {
	var out []string
	for _, o := range r.Panel {
		out = append(out, o.Agent)
	}
	return out
}

func summarize(r judge.Report) string {
	var b strings.Builder
	for _, f := range r.Findings {
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		fmt.Fprintf(&b, "[%s] %s %s\n", f.Severity, loc, f.Summary)
	}
	for _, o := range r.Panel {
		if o.Notes != "" {
			fmt.Fprintf(&b, "%s: %s\n", o.Agent, o.Notes)
		}
	}
	return strings.TrimSpace(b.String())
}
