package weave

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

type weaveReviewResult struct {
	Issue         int64     `json:"issue"`
	Verdict       string    `json:"verdict"`
	Blocking      bool      `json:"blocking"`
	Notes         string    `json:"notes"`
	Exit          int       `json:"exit"`
	Files         int       `json:"files"`
	Insertions    int       `json:"insertions,omitempty"`
	Commits       int       `json:"commits"`
	Branch        string    `json:"branch,omitempty"`
	ReviewBy      string    `json:"review_by"`
	ReviewAt      time.Time `json:"review_at"`
	VerifyCommand string    `json:"verify_command,omitempty"`
}

func runWeaveReview(cmd *cobra.Command, id int64, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave review",
			weavecli.ExitPrecondFail, err))
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave review",
			weavecli.ExitPrecondFail, err))
	}

	q, err := loadWeaveQueue(dir)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave review",
			weavecli.ExitGenericFail, err))
	}
	it := findWeaveItem(q, id)
	if it == nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave review",
			weavecli.ExitInvalidArg, fmt.Errorf("run #%d not found%s", id, weaveOtherActiveQueuesHintSuffix(dir))))
	}
	if it.Workspace == "" {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave review",
			weavecli.ExitStateConflict, fmt.Errorf("run #%d has no workspace recorded", id)))
	}
	if _, err := os.Stat(it.Workspace); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave review",
			weavecli.ExitStateConflict, fmt.Errorf("run #%d workspace unavailable: %s: %w", id, it.Workspace, err)))
	}
	if it.Branch == "" {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave review",
			weavecli.ExitStateConflict, fmt.Errorf("run #%d has no branch to review", id)))
	}

	tmpParent, err := os.MkdirTemp("", "weave-review-*")
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave review",
			weavecli.ExitGenericFail, err))
	}
	defer os.RemoveAll(tmpParent)
	reviewDir := filepath.Join(tmpParent, "checkout")
	if out, err := exec.Command("git", "clone", "--local", "--no-hardlinks", it.Workspace, reviewDir).CombinedOutput(); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave review",
			weavecli.ExitGenericFail, fmt.Errorf("git clone --local --no-hardlinks: %w: %s", err, strings.TrimSpace(string(out)))))
	}
	if out, err := exec.Command("git", "-C", reviewDir, "checkout", it.Branch).CombinedOutput(); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave review",
			weavecli.ExitStateConflict, fmt.Errorf("checkout %s: %w: %s", it.Branch, err, strings.TrimSpace(string(out)))))
	}
	if synced, failed := weaveSyncSiblingDeps(root, reviewDir); len(synced) > 0 || len(failed) > 0 {
		if len(failed) > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "weave review: sibling sync failed for %s; verify may fail if it needs them\n", strings.Join(failed, ", "))
		}
	}

	base := weaveBaseBranch(root)
	baseRef := weaveReviewBaseRef(reviewDir, base)
	commits := weaveReviewCommitCount(reviewDir, baseRef)
	files, insertions := weaveReviewDiffStat(reviewDir, baseRef)
	verifyExit := 0
	if it.VerifyCommand != "" {
		verifyExit, _ = weaveRunVerify(reviewDir, dir, it.VerifyCommand, it)
	}
	real := commits > 0
	blocking := !real || (it.VerifyCommand != "" && verifyExit != 0)
	verdict := "pass"
	if blocking {
		verdict = "blocked"
	}
	notes := fmt.Sprintf("clean-room verify exit=%d, %d files, %d commits", verifyExit, files, commits)
	now := time.Now().UTC()
	res := weaveReviewResult{
		Issue:         it.ID,
		Verdict:       verdict,
		Blocking:      blocking,
		Notes:         notes,
		Exit:          verifyExit,
		Files:         files,
		Insertions:    insertions,
		Commits:       commits,
		Branch:        it.Branch,
		ReviewBy:      "weave review",
		ReviewAt:      now,
		VerifyCommand: it.VerifyCommand,
	}

	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		fresh := findWeaveItem(q, id)
		if fresh == nil {
			return fmt.Errorf("run #%d not found%s", id, weaveOtherActiveQueuesHintSuffix(dir))
		}
		fresh.ReviewVerdict = verdict
		fresh.ReviewBlocking = blocking
		fresh.ReviewNotes = notes
		fresh.ReviewExit = verifyExit
		fresh.ReviewBy = "weave review"
		fresh.ReviewAt = now
		return nil
	})
	if lockErr != nil {
		code := weavecli.ExitGenericFail
		if strings.Contains(lockErr.Error(), "not found") {
			code = weavecli.ExitInvalidArg
		}
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave review",
			code, lockErr))
	}

	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave review", res))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "review: %s (exit=%d, files=%d) — %s\n", verdict, verifyExit, files, notes)
	return nil
}

func weaveReviewCommitCount(workspace, base string) int {
	if base == "" {
		return 0
	}
	out, err := exec.Command("git", "-C", workspace, "rev-list", "--count", base+"..HEAD").Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

func weaveReviewDiffStat(workspace, base string) (files, insertions int) {
	if base == "" {
		return 0, 0
	}
	out, err := exec.Command("git", "-C", workspace, "diff", "--numstat", base+"..HEAD").Output()
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		files++
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] != "-" {
			if n, err := strconv.Atoi(fields[0]); err == nil {
				insertions += n
			}
		}
	}
	return files, insertions
}

func weaveReviewBaseRef(workspace, base string) string {
	for _, ref := range []string{base, "origin/" + base, "refs/remotes/origin/" + base} {
		if ref == "" || ref == "origin/" || ref == "refs/remotes/origin/" {
			continue
		}
		if err := exec.Command("git", "-C", workspace, "rev-parse", "--verify", "--quiet", ref+"^{commit}").Run(); err == nil {
			return ref
		}
	}
	return base
}
