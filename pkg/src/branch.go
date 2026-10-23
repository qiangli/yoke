package src

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ygit "github.com/qiangli/yoke/git"
)

// localBranch is the host's own branch: topic branches fork from it and PR
// merges land on it. It is created from `pristine` on first use. `pristine`
// (fetch.go) is the known-good release point and is never merged into,
// committed to, or pushed from by this package.
const localBranch = "local"

// repoDir resolves $BASHY_HOME/src/<repo> and refuses names that could
// escape it. Local operations need no allowlist — acquisition does — but a
// repo that was never fetched is an error, never an implicit creation.
func repoDir(home, repo string) (string, error) {
	if home == "" {
		home = Home()
	}
	if repo == "" || repo == "." || repo == ".." ||
		strings.Contains(repo, "/") || strings.Contains(repo, "\\") ||
		strings.ContainsAny(repo, " \t\n\r\x00") {
		return "", fmt.Errorf("src: invalid repo name %q", repo)
	}
	dir := filepath.Join(SrcDir(home), repo)
	st, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return "", fmt.Errorf("src: repo %q is not fetched (no checkout at %s)", repo, dir)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("src: repo %q is not fetched (no checkout at %s)", repo, dir)
	}
	return dir, nil
}

// validBranchName rejects names git would misparse or that could escape the
// refs namespace.
func validBranchName(name string) error {
	if name == "" {
		return fmt.Errorf("src: empty branch name")
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") ||
		strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".lock") ||
		name == "@" || strings.Contains(name, "..") || strings.Contains(name, "@{") ||
		strings.ContainsAny(name, " \t\n\r\x00~^:?*[\\") {
		return fmt.Errorf("src: invalid branch name %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || strings.HasSuffix(part, ".") {
			return fmt.Errorf("src: invalid branch name %q", name)
		}
	}
	return nil
}

// branchNames lists local branches without the "* "/two-space display
// prefix ygit.Branch decorates them with.
func branchNames(dir string) ([]string, error) {
	_, lines, err := ygit.Branch(ygit.BranchOptions{RepoPath: dir})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range lines {
		out = append(out, strings.TrimPrefix(strings.TrimPrefix(l, "* "), "  "))
	}
	return out, nil
}

// branchExists reports whether branch exists locally.
func branchExists(dir, name string) (bool, error) {
	names, err := branchNames(dir)
	if err != nil {
		return false, err
	}
	for _, n := range names {
		if n == name {
			return true, nil
		}
	}
	return false, nil
}

// currentBranch returns the checked-out branch, or "" on a detached HEAD.
func currentBranch(dir string) (string, error) {
	_, lines, err := ygit.Branch(ygit.BranchOptions{RepoPath: dir})
	if err != nil {
		return "", err
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "* ") {
			name := strings.TrimPrefix(l, "* ")
			if name == "HEAD" || strings.HasPrefix(name, "(") {
				return "", nil
			}
			return name, nil
		}
	}
	return "", nil
}

// checkoutUnlocked switches branches with protected-manifest files loosened
// first: go-git rewrites files that differ, and the read-only bit fetch
// leaves on guard-rail files would fail the write as a non-root user.
// Callers relock when their mutation sequence completes.
func checkoutUnlocked(home, dir, repo, branch string) error {
	if err := unlockRepo(dir, repo); err != nil {
		return err
	}
	if _, err := ygit.Checkout(ygit.CheckoutOptions{RepoPath: dir, Branch: branch}); err != nil {
		return fmt.Errorf("src: checkout %s: %w", branch, err)
	}
	return nil
}

// requireClean refuses when the worktree carries any change — staged,
// unstaged, or untracked. Merges and rebases never clobber local edits.
func requireClean(dir string) error {
	_, entries, err := ygit.Status(dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.File)
		}
		return fmt.Errorf("src: working tree is not clean (%s) — commit or discard local changes first",
			strings.Join(names, ", "))
	}
	return nil
}

// unlockRepo loosens the read-only bit on every file so git mechanics
// (checkout, merge, apply, reset) can rewrite guard-rail paths. The
// read-only bit is a tripwire, not access control — Check is the guard.
func unlockRepo(dir, repo string) error {
	_ = repo
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o200 == 0 {
			if err := os.Chmod(path, 0o644); err != nil {
				return err
			}
		}
		return nil
	})
}

// relockRepo restores fetch's read-only protection on manifest paths.
func relockRepo(dir, repo string) error { return protectFiles(dir, repo) }

// EnsureLocal creates the host's `local` branch from `pristine` when it is
// missing and otherwise leaves it alone. It never moves an existing local.
func EnsureLocal(home, repo string) error {
	dir, err := repoDir(home, repo)
	if err != nil {
		return err
	}
	ok, err := branchExists(dir, localBranch)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	if ok, err := branchExists(dir, pristineBranch); err != nil || !ok {
		return fmt.Errorf("src: cannot seed %s: no %s branch in %s (fetch first)",
			localBranch, pristineBranch, repo)
	}
	pristineSHA, err := ygit.ResolveCommit(dir, pristineBranch)
	if err != nil {
		return err
	}
	cur, err := currentBranch(dir)
	if err != nil {
		return err
	}
	// Create local at pristine's tip however the worktree sits, then go
	// back: Branch pins the new ref at HEAD, so HEAD must be pristine.
	if err := requireClean(dir); err != nil {
		return err
	}
	if err := checkoutUnlocked(home, dir, repo, pristineBranch); err != nil {
		return err
	}
	if _, _, err := ygit.Branch(ygit.BranchOptions{RepoPath: dir, Name: localBranch}); err != nil {
		return fmt.Errorf("src: branch %s: %w", localBranch, err)
	}
	localSHA, err := ygit.ResolveCommit(dir, localBranch)
	if err != nil {
		return err
	}
	if localSHA != pristineSHA {
		return fmt.Errorf("src: branch %s landed at %s, want %s", localBranch, shortSHA(localSHA), shortSHA(pristineSHA))
	}
	if cur != "" && cur != pristineBranch {
		if err := checkoutUnlocked(home, dir, repo, cur); err != nil {
			return err
		}
	}
	return relockRepo(dir, repo)
}

// CreateBranch creates topic branch name off `local` (seeding local from
// `pristine` on first use) and checks it out. It refuses when the name
// exists and never touches `pristine` or any remote.
func CreateBranch(home, repo, name string) error {
	if err := validBranchName(name); err != nil {
		return err
	}
	dir, err := repoDir(home, repo)
	if err != nil {
		return err
	}
	if err := EnsureLocal(home, repo); err != nil {
		return err
	}
	if ok, err := branchExists(dir, name); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("src: branch %q already exists in %s", name, repo)
	}
	if err := requireClean(dir); err != nil {
		return err
	}
	if err := checkoutUnlocked(home, dir, repo, localBranch); err != nil {
		return err
	}
	if _, err := ygit.Checkout(ygit.CheckoutOptions{RepoPath: dir, Branch: name, Create: true}); err != nil {
		_ = relockRepo(dir, repo)
		return fmt.Errorf("src: branch %s: %w", name, err)
	}
	return relockRepo(dir, repo)
}
