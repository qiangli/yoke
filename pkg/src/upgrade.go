package src

import (
	"context"
	"fmt"
	"os"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	ygit "github.com/qiangli/yoke/git"
)

// replayCommit is one local commit to re-apply onto the new release.
type replayCommit struct {
	hash        string
	parent      string
	message     string
	authorName  string
	authorEmail string
}

// localCommitsSince walks the first-parent chain from tip down to base
// (exclusive), oldest first. Merge commits ride along as their net diff
// against their first parent — the rebase linearizes them, as `git
// rebase` does by default.
func localCommitsSince(dir, base, tip string) ([]replayCommit, error) {
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		return nil, fmt.Errorf("src: open %s: %w", dir, err)
	}
	baseHash := plumbing.NewHash(base)
	tipHash := plumbing.NewHash(tip)
	var rev []replayCommit
	c, err := r.CommitObject(tipHash)
	if err != nil {
		return nil, fmt.Errorf("src: commit %s: %w", shortSHA(tip), err)
	}
	for c.Hash != baseHash {
		if len(rev) > 100000 {
			return nil, fmt.Errorf("src: history walk exceeded 100000 commits")
		}
		if c.NumParents() == 0 {
			return nil, fmt.Errorf("src: %s does not descend from %s", shortSHA(tip), shortSHA(base))
		}
		parent, err := c.Parent(0)
		if err != nil {
			return nil, fmt.Errorf("src: parent of %s: %w", shortSHA(c.Hash.String()), err)
		}
		rev = append(rev, replayCommit{
			hash: c.Hash.String(), parent: parent.Hash.String(),
			message: c.Message, authorName: c.Author.Name, authorEmail: c.Author.Email,
		})
		c = parent
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, nil
}

// moveBranchTo points branch at sha with a hard reset of its checkout.
// The tree must be clean; nothing is lost.
func moveBranchTo(home, dir, repo, branch, sha string) error {
	if err := checkoutUnlocked(home, dir, repo, branch); err != nil {
		return err
	}
	if _, err := ygit.Reset(ygit.ResetOpts{RepoPath: dir, Mode: ygit.ResetHard, Commit: sha}); err != nil {
		return fmt.Errorf("src: reset %s to %s: %w", branch, shortSHA(sha), err)
	}
	return relockRepo(dir, repo)
}

// overlap returns paths changed on both sides: the replay patch and the
// upstream release range. It is the fallback conflict report when apply's
// own stderr names nothing.
func overlap(dir, base, upstream string, patchFiles []string) []string {
	upText, err := diffText(dir, base, upstream)
	if err != nil {
		return patchFiles
	}
	up := map[string]bool{}
	for _, f := range parseDiffFiles(upText) {
		up[f] = true
	}
	var out []string
	for _, f := range patchFiles {
		if up[f] {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return patchFiles
	}
	return out
}

// Upgrade moves repo to the new release commit: it fetches, points
// `pristine` at newCommit, and rebases `local` onto it by replaying every
// local commit past the old pristine. On conflict it aborts — `local`,
// `pristine`, and the worktree files are exactly as they were — and
// returns the conflicting paths alongside the error. A dirty worktree, an
// unknown commit, or a non-descendant release is refused before anything
// moves. Nothing is ever pushed; no remote ref is written.
func Upgrade(home, repo, newCommit string) ([]string, error) {
	dir, err := repoDir(home, repo)
	if err != nil {
		return nil, err
	}
	if err := requireClean(dir); err != nil {
		return nil, err
	}
	fetchErr := func() error {
		_, err := ygit.Fetch(ygit.FetchOptions{RepoPath: dir})
		return err
	}()
	newSHA, err := ygit.ResolveCommit(dir, newCommit)
	if err != nil {
		if fetchErr != nil {
			return nil, fmt.Errorf("src: upgrade %s: fetch: %v; unknown commit %s", repo, fetchErr, shortSHA(newCommit))
		}
		return nil, fmt.Errorf("src: upgrade %s: unknown commit %s", repo, shortSHA(newCommit))
	}
	oldPristine, err := ygit.ResolveCommit(dir, pristineBranch)
	if err != nil {
		return nil, err
	}
	if newSHA == oldPristine {
		return nil, nil
	}
	anc, err := ygit.IsAncestor(dir, oldPristine, newSHA)
	if err != nil {
		return nil, err
	}
	if !anc {
		return nil, fmt.Errorf("src: refusing upgrade: %s is not a descendant of %s (would clobber history)",
			shortSHA(newSHA), shortSHA(oldPristine))
	}

	hasLocal, err := branchExists(dir, localBranch)
	if err != nil {
		return nil, err
	}
	localTip := oldPristine
	if hasLocal {
		if localTip, err = ygit.ResolveCommit(dir, localBranch); err != nil {
			return nil, err
		}
	}
	cur, err := currentBranch(dir)
	if err != nil {
		return nil, err
	}
	restoreBranch := func() {
		if cur != "" {
			_, _ = ygit.Checkout(ygit.CheckoutOptions{RepoPath: dir, Branch: cur})
		}
		_ = relockRepo(dir, repo)
	}

	// No local divergence: slide both pins forward, no replay.
	if localTip == oldPristine {
		if err := unlockRepo(dir, repo); err != nil {
			return nil, err
		}
		if err := moveBranchTo(home, dir, repo, pristineBranch, newSHA); err != nil {
			restoreBranch()
			return nil, err
		}
		if hasLocal {
			if err := moveBranchTo(home, dir, repo, localBranch, newSHA); err != nil {
				restoreBranch()
				return nil, err
			}
		}
		restoreBranch()
		return nil, nil
	}

	commits, err := localCommitsSince(dir, oldPristine, localTip)
	if err != nil {
		return nil, err
	}
	if err := unlockRepo(dir, repo); err != nil {
		return nil, err
	}
	abort := func(conflicts []string, format string, args ...any) ([]string, error) {
		_, _ = ygit.Checkout(ygit.CheckoutOptions{RepoPath: dir, Branch: localBranch})
		_, _ = ygit.Reset(ygit.ResetOpts{RepoPath: dir, Mode: ygit.ResetHard, Commit: localTip})
		restoreBranch()
		return conflicts, fmt.Errorf("src: upgrade aborted: "+format, args...)
	}
	// Stage local at the new release, then replay.
	if err := checkoutUnlocked(home, dir, repo, localBranch); err != nil {
		restoreBranch()
		return nil, err
	}
	if _, err := ygit.Reset(ygit.ResetOpts{RepoPath: dir, Mode: ygit.ResetHard, Commit: newSHA}); err != nil {
		return abort(nil, "reset to %s: %v", shortSHA(newSHA), err)
	}
	for _, c := range commits {
		text, err := diffText(dir, c.parent, c.hash)
		if err != nil {
			return abort(nil, "diff %s: %v", shortSHA(c.hash), err)
		}
		files := parseDiffFiles(text)
		if len(files) == 0 {
			continue
		}
		tmp, err := os.CreateTemp("", "src-upgrade-*.patch")
		if err != nil {
			return abort(nil, "temp patch: %v", err)
		}
		tmpName := tmp.Name()
		_, _ = tmp.WriteString(text)
		_ = tmp.Close()
		res, execErr := ygit.Exec(context.Background(), dir, []string{"apply", tmpName})
		_ = os.Remove(tmpName)
		if execErr != nil {
			return abort(overlap(dir, oldPristine, newSHA, files), "replay %s: %v", shortSHA(c.hash), execErr)
		}
		if res.ExitCode != 0 {
			conflicts := applyFailurePaths(res.Stderr)
			if len(conflicts) == 0 {
				conflicts = overlap(dir, oldPristine, newSHA, files)
			}
			return abort(conflicts, "replay %s conflicts on %s", shortSHA(c.hash), strings.Join(conflicts, ", "))
		}
		if err := stageAll(dir); err != nil {
			return abort(nil, "stage %s: %v", shortSHA(c.hash), err)
		}
		if _, entries, err := ygit.Status(dir); err != nil {
			return abort(nil, "status: %v", err)
		} else if len(entries) == 0 {
			continue // already upstream; drop the empty replay.
		}
		name, email := c.authorName, c.authorEmail
		if name == "" {
			name = agentName()
		}
		if email == "" {
			email = "bashy@localhost"
		}
		if err := commitAll(dir, c.message, name, email); err != nil {
			return abort(nil, "commit %s: %v", shortSHA(c.hash), err)
		}
	}
	// Replay held: move pristine under the rebased local.
	if err := moveBranchTo(home, dir, repo, pristineBranch, newSHA); err != nil {
		return abort(nil, "move pristine: %v", err)
	}
	restoreBranch()
	return nil, nil
}
