package src

import (
	"context"
	"fmt"
	"sort"
	"strings"

	ygit "github.com/qiangli/yoke/git"
)

// RevertResult describes what would be (or was) discarded by Revert.
type RevertResult struct {
	Repo        string
	PristineSHA string
	Commits     []RevertCommit
	Files       []string
	Applied     bool
}

// RevertCommit is one commit that would be (or was) discarded.
type RevertCommit struct {
	Hash    string
	Message string
}

// Revert resets the local src checkout to pristine (base tip).
// When apply is false (dry-run), it lists what would be discarded (commits on local
// past pristine, and uncommitted working-tree changes) without making any changes.
// When apply is true, it resets the checkout and the local branch to pristine,
// removes untracked files, and restores read-only protection on manifest paths.
// It never touches branches or PRs of other repos.
func Revert(home, repo string, apply bool) (*RevertResult, error) {
	dir, err := repoDir(home, repo)
	if err != nil {
		return nil, err
	}
	ok, err := branchExists(dir, pristineBranch)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("src: no %s branch in %s (fetch first)", pristineBranch, repo)
	}
	pristineSHA, err := ygit.ResolveCommit(dir, pristineBranch)
	if err != nil {
		return nil, err
	}

	res := &RevertResult{
		Repo:        repo,
		PristineSHA: pristineSHA,
		Applied:     apply,
	}

	// 1. Commits on local past pristine.
	hasLocal, err := branchExists(dir, localBranch)
	if err != nil {
		return nil, err
	}
	if hasLocal {
		localSHA, err := ygit.ResolveCommit(dir, localBranch)
		if err != nil {
			return nil, err
		}
		if localSHA != pristineSHA {
			anc, err := ygit.IsAncestor(dir, pristineSHA, localSHA)
			if err == nil && anc {
				replays, err := localCommitsSince(dir, pristineSHA, localSHA)
				if err == nil {
					for _, r := range replays {
						res.Commits = append(res.Commits, RevertCommit{
							Hash:    r.hash,
							Message: strings.TrimSpace(r.message),
						})
					}
				}
			} else {
				mb, err := mergeBase(dir, pristineSHA, localSHA)
				if err == nil {
					replays, err := localCommitsSince(dir, mb, localSHA)
					if err == nil {
						for _, r := range replays {
							res.Commits = append(res.Commits, RevertCommit{
								Hash:    r.hash,
								Message: strings.TrimSpace(r.message),
							})
						}
					}
				}
			}
		}
	}

	// 2. Working tree files (modified, untracked, staged, deleted).
	seenFiles := make(map[string]bool)
	if _, entries, err := ygit.Status(dir); err == nil {
		for _, e := range entries {
			if !seenFiles[e.File] {
				seenFiles[e.File] = true
				res.Files = append(res.Files, e.File)
			}
		}
	}
	sort.Strings(res.Files)

	if !apply {
		return res, nil
	}

	// Apply mode:
	if err := unlockRepo(dir, repo); err != nil {
		return nil, err
	}

	// Discard any staged or unstaged modifications in the current worktree.
	if _, err := ygit.Reset(ygit.ResetOpts{RepoPath: dir, Mode: ygit.ResetHard}); err != nil {
		return nil, fmt.Errorf("src: reset HEAD: %w", err)
	}

	// Discard untracked files.
	_, _ = ygit.Exec(context.Background(), dir, []string{"clean", "-fd"})

	// Ensure local branch exists.
	if !hasLocal {
		if err := EnsureLocal(home, repo); err != nil {
			return nil, err
		}
	}

	// Check out local.
	if err := checkoutUnlocked(home, dir, repo, localBranch); err != nil {
		return nil, err
	}

	// Reset local branch to pristine.
	if _, err := ygit.Reset(ygit.ResetOpts{RepoPath: dir, Mode: ygit.ResetHard, Commit: pristineSHA}); err != nil {
		return nil, fmt.Errorf("src: reset %s to %s: %w", localBranch, shortSHA(pristineSHA), err)
	}

	// Clean untracked files again if any were left by checkout.
	_, _ = ygit.Exec(context.Background(), dir, []string{"clean", "-fd"})

	// Relock protected manifest paths.
	if err := relockRepo(dir, repo); err != nil {
		return nil, err
	}

	return res, nil
}
