package src

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	ygit "github.com/qiangli/yoke/git"
)

// pristineBranch is the local branch fetch leaves at the release commit: the
// known-good point later fetches and audits compare against.
const pristineBranch = "pristine"

// Fetch pins the OSS repo at $BASHY_HOME/src/<repo> on the running binary's
// release commit, creating a local `pristine` branch there. It is
// fail-closed throughout:
//
//   - repos off the OSS allowlist are refused (proprietary origins never
//     land under src/);
//   - without a known release commit it refuses (set $BASHY_SRC_COMMIT in
//     development builds);
//   - the origin is the Sprint 386 offline seed ($BASHY_HOME/seed/<repo>)
//     when present, else the canonical remote — and offline mode without a
//     seed refuses;
//   - a missing commit (tampered or mismatched seed/remote) aborts and the
//     partial checkout is removed;
//   - an existing checkout anywhere but the release commit is refused, never
//     clobbered; an existing checkout already there is a no-op.
//
// Protected-manifest files land read-only. All git work goes through yoke's
// pure-Go git client: no shell-outs.
func Fetch(home, repo string) (string, error) {
	if home == "" {
		home = Home()
	}
	remote, err := remoteFor(repo)
	if err != nil {
		return "", err
	}
	commit := ReleaseCommit()
	if commit == "" {
		return "", fmt.Errorf("src: no release commit known (development build without VCS stamp; set $%s to fetch)", commitOverrideEnv)
	}
	dest := filepath.Join(SrcDir(home), repo)
	if st, err := os.Stat(dest); err == nil && st.IsDir() {
		return fetchExisting(dest, repo, commit)
	}
	origin := seedOrigin(home, repo)
	if origin == "" {
		if Offline() {
			return "", fmt.Errorf("src: %s is not cached and BASHY_OFFLINE=1 forbids downloading it (install the Sprint 386 seed at %s)",
				repo, filepath.Join(SeedDir(home), repo))
		}
		origin = remote
	}
	if _, err := ygit.Clone(ygit.CloneOptions{URL: origin, Path: dest}); err != nil {
		_ = os.RemoveAll(dest)
		return "", fmt.Errorf("src: clone %s: %w", repo, err)
	}
	if _, err := ygit.Checkout(ygit.CheckoutOptions{RepoPath: dest, Branch: commit}); err != nil {
		_ = os.RemoveAll(dest)
		return "", fmt.Errorf("src: refusing %s: commit %s not found in %s (%v)", repo, shortSHA(commit), origin, err)
	}
	rev, err := ygit.RevParse(ygit.RevParseOptions{RepoPath: dest})
	if err != nil {
		_ = os.RemoveAll(dest)
		return "", fmt.Errorf("src: verify %s: %w", repo, err)
	}
	if rev.Hash != commit {
		_ = os.RemoveAll(dest)
		return "", fmt.Errorf("src: refusing %s: checkout at %s, want release commit %s", repo, shortSHA(rev.Hash), shortSHA(commit))
	}
	if err := ensurePristine(dest, repo); err != nil {
		_ = os.RemoveAll(dest)
		return "", err
	}
	if err := protectFiles(dest, repo); err != nil {
		_ = os.RemoveAll(dest)
		return "", err
	}
	return fmt.Sprintf("fetched %s at %s from %s", repo, shortSHA(commit), origin), nil
}

// fetchExisting handles a present checkout: exact-commit match is a no-op,
// anything else is refused (a tampered or mismatched tree must never be
// silently kept or clobbered).
func fetchExisting(dest, repo, commit string) (string, error) {
	rev, err := ygit.RevParse(ygit.RevParseOptions{RepoPath: dest})
	if err != nil {
		return "", fmt.Errorf("src: refusing %s: %v", dest, err)
	}
	if rev.Hash != commit {
		return "", fmt.Errorf("src: refusing %s: checkout at %s, want release commit %s (remove it to re-fetch)",
			dest, shortSHA(rev.Hash), shortSHA(commit))
	}
	if err := ensurePristine(dest, repo); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s already at %s (no-op)", dest, shortSHA(commit)), nil
}

// ensurePristine creates (or confirms) the local `pristine` branch at HEAD —
// callers guarantee HEAD is the release commit — and checks it out.
func ensurePristine(dest, repo string) error {
	if _, _, err := ygit.Branch(ygit.BranchOptions{RepoPath: dest, Name: pristineBranch}); err != nil {
		return fmt.Errorf("src: branch %s: %w", pristineBranch, err)
	}
	cur, err := currentBranch(dest)
	if err != nil {
		return err
	}
	if cur == pristineBranch {
		return nil
	}
	if err := unlockRepo(dest, repo); err != nil {
		return err
	}
	if _, err := ygit.Checkout(ygit.CheckoutOptions{RepoPath: dest, Branch: pristineBranch}); err != nil {
		_ = relockRepo(dest, repo)
		return fmt.Errorf("src: checkout %s: %w", pristineBranch, err)
	}
	return relockRepo(dest, repo)
}

// protectFiles leaves every file under a protected-manifest prefix read-only.
// The .git directory is excluded: integrity tooling must keep working.
func protectFiles(dest, repo string) error {
	return filepath.WalkDir(dest, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dest, path)
		if err != nil {
			return err
		}
		if matchProt(repo, filepath.ToSlash(rel)) == nil {
			return nil
		}
		if err := os.Chmod(path, 0o444); err != nil {
			return fmt.Errorf("src: protect %s: %w", rel, err)
		}
		return nil
	})
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
