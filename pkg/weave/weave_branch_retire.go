package weave

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Branch retirement (Sprint 224 S3).
//
// A run owns exactly two local branch names in the user's repo: the one it was
// started on (`agent/weave-issue-N`, fetched there by `weave pull`) and the
// review sibling a human reviewer may have made by hand
// (`agent/weave-issue-N-reviewed`). Weave never creates the sibling, so it is
// recognised by NAME rather than recorded — a recorded field would always be
// empty. Nothing else is ever considered: not remotes, not arbitrary branches,
// not a name that merely contains the issue number.
//
// Deletion needs three proofs, in order: no worktree has the branch checked
// out; every patch is already upstream (ancestor OR `git cherry` reports no
// unique patch — the same rule sprint hygiene REPORTS with); and the tip has
// not moved since the proof was taken. The last one is git's own compare-and-
// swap, `update-ref -d <ref> <expected>`, which refuses if the ref no longer
// points at <expected>. `branch -D` appears nowhere on a cleanup path.

func weaveRunBranchNames(it *weaveItem) []string {
	if it == nil || strings.TrimSpace(it.Branch) == "" {
		return nil
	}
	return []string{it.Branch, it.Branch + "-reviewed"}
}

func gitBranchTip(root, branch string) (string, bool) {
	out, err := exec.Command(gitBin(), "-C", root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// gitBranchWorktree returns the path of the worktree that has branch checked
// out, or "" when none does. The main working tree counts: deleting the branch
// HEAD points at is refused by git anyway, but we want the reason by name.
func gitBranchWorktree(root, branch string) string {
	out, err := exec.Command(gitBin(), "-C", root, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return ""
	}
	want := "branch refs/heads/" + branch
	for _, block := range strings.Split(string(out), "\n\n") {
		path, attached := "", false
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "worktree "):
				path = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
			case strings.TrimSpace(line) == want:
				attached = true
			}
		}
		if attached && path != "" {
			return path
		}
	}
	return ""
}

// weaveRetireBranch deletes one integrated local branch, or says why not.
// It returns (deleted, err): (false, nil) means the branch did not exist.
//
// salvageRef, when set, is a second place the patches may already live: a
// rejected or superseded tip preserved under refs/salvage/ by `weave abandon`.
// A branch whose tip is reachable from it points at nothing unique either.
func weaveRetireBranch(root, base, branch, salvageRef string) (bool, error) {
	if base == "" || branch == "" || branch == base {
		return false, nil
	}
	tip, ok := gitBranchTip(root, branch)
	if !ok {
		return false, nil
	}
	if wt := gitBranchWorktree(root, branch); wt != "" {
		return false, fmt.Errorf("checked out in worktree %s; left alone", wt)
	}
	preserved := salvageRef != "" &&
		exec.Command(gitBin(), "-C", root, "merge-base", "--is-ancestor", tip, salvageRef).Run() == nil
	if !preserved && !gitBranchIntegrated(root, base, branch) {
		return false, errors.New("carries a patch not in " + base + "; left alone")
	}
	out, err := exec.Command(gitBin(), "-C", root, "update-ref", "-d", "refs/heads/"+branch, tip).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("tip moved during retirement (%s); left alone", strings.TrimSpace(string(out)))
	}
	return true, nil
}

// weaveRetireRunBranches retires the run's two names and reports each as an
// action, so `sprint end` can count branches alongside bytes.
func weaveRetireRunBranches(root, repo string, it *weaveItem) []sprintPruneAction {
	base := weaveBaseBranch(root)
	var acts []sprintPruneAction
	for _, name := range weaveRunBranchNames(it) {
		deleted, err := weaveRetireBranch(root, base, name, it.SalvageRef)
		if !deleted && err == nil {
			continue
		}
		a := sprintPruneAction{Kind: "branch", Repo: repo, Target: name, Done: deleted, BytesComplete: true}
		if err != nil {
			a.Err = err.Error()
		}
		acts = append(acts, a)
	}
	return acts
}
