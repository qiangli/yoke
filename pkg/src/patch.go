package src

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ygit "github.com/qiangli/yoke/git"
)

// ExportPatch renders the PR's diff (merge base of local and the recorded
// head SHA to that SHA) as a unified patch for `src patch apply` on
// another host. The text goes to the caller — the CLI prints only the
// patch on stdout so `src patch export <id> > file` round-trips.
func ExportPatch(home, id string) (string, error) {
	pr, err := GetPR(home, id)
	if err != nil {
		return "", err
	}
	dir, err := repoDir(home, pr.Repo)
	if err != nil {
		return "", err
	}
	base, err := mergeBase(dir, localBranch, pr.HeadSHA)
	if err != nil {
		return "", fmt.Errorf("src: PR %s head %s: %w", id, shortSHA(pr.HeadSHA), err)
	}
	if base == pr.HeadSHA {
		return "", fmt.Errorf("src: PR %s has no differences to export", id)
	}
	text, err := diffText(dir, base, pr.HeadSHA)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("src: PR %s has no differences to export", id)
	}
	return text, nil
}

// sanitizePatchName derives a branch-safe stem from a patch file name.
func sanitizePatchName(file string) string {
	base := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "patch"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// applyFailurePaths recovers the conflicting paths from a failed apply run.
func applyFailurePaths(stderr string) []string {
	var out []string
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		var p string
		switch {
		case strings.HasPrefix(line, "error: patch failed: "):
			p = strings.TrimPrefix(line, "error: patch failed: ")
		case strings.HasPrefix(line, "error: ") && strings.Contains(line, ": already exists"):
			p = strings.TrimSuffix(strings.TrimPrefix(line, "error: "), ": already exists in working directory")
		case strings.HasPrefix(line, "error: ") && strings.Contains(line, ": No such file"):
			p = strings.TrimSuffix(strings.TrimPrefix(line, "error: "), ": No such file or directory")
		}
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// applyPatchFile runs the pure-Go `git apply` over an already-checked-out
// branch and stages the result, ready to commit.
func applyPatchFile(dir, patchFile string) error {
	res, err := ygit.Exec(context.Background(), dir, []string{"apply", patchFile})
	if err != nil {
		return fmt.Errorf("src: apply %s: %w", filepath.Base(patchFile), err)
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("exit %d", res.ExitCode)
		}
		return fmt.Errorf("src: apply %s: %s", filepath.Base(patchFile), msg)
	}
	return nil
}

// stageAll stages the worktree for a replay/apply commit: new files through
// Add and tracked modifications plus deletions through Commit -a.
func stageAll(dir string) error {
	if _, err := ygit.Add(ygit.AddOptions{RepoPath: dir, All: true}); err != nil {
		return fmt.Errorf("src: add: %w", err)
	}
	return nil
}

// commitAll records a commit with the given identity, staging tracked
// changes like `commit -a`.
func commitAll(dir, msg, name, email string) error {
	if strings.TrimSpace(msg) == "" {
		return fmt.Errorf("src: commit message required")
	}
	if _, err := ygit.Commit(ygit.CommitOptions{
		RepoPath: dir, Message: msg, All: true,
		AuthorName: name, AuthorEmail: email,
	}); err != nil {
		return fmt.Errorf("src: commit: %w", err)
	}
	return nil
}

// ApplyPatch applies a patch file (as written by `src patch export`) to
// repo on a fresh topic branch off `local`, commits it, and opens a PR.
// It returns the new PR and leaves the new branch checked out. Failures
// remove the fresh branch and restore the previous one.
func ApplyPatch(home, repo, patchFile, title string) (PR, error) {
	raw, err := os.ReadFile(patchFile)
	if err != nil {
		return PR{}, fmt.Errorf("src: patch %s: %w", patchFile, err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return PR{}, fmt.Errorf("src: patch %s is empty", patchFile)
	}
	if title == "" {
		title = "Apply " + filepath.Base(patchFile)
	}
	dir, err := repoDir(home, repo)
	if err != nil {
		return PR{}, err
	}
	if err := EnsureLocal(home, repo); err != nil {
		return PR{}, err
	}
	if err := requireClean(dir); err != nil {
		return PR{}, err
	}
	abs, err := filepath.Abs(patchFile)
	if err != nil {
		return PR{}, err
	}
	stem := "patch-" + sanitizePatchName(patchFile)
	name := stem
	for n := 2; ; n++ {
		ok, err := branchExists(dir, name)
		if err != nil {
			return PR{}, err
		}
		if !ok {
			break
		}
		name = fmt.Sprintf("%s-%d", stem, n)
	}
	cur, err := currentBranch(dir)
	if err != nil {
		return PR{}, err
	}
	if err := unlockRepo(dir, repo); err != nil {
		return PR{}, err
	}
	abort := func() {
		_, _ = ygit.Checkout(ygit.CheckoutOptions{RepoPath: dir, Branch: localBranch})
		_, _, _ = ygit.Branch(ygit.BranchOptions{RepoPath: dir, Name: name, Delete: true, Force: true})
		if cur != "" && cur != localBranch {
			_, _ = ygit.Checkout(ygit.CheckoutOptions{RepoPath: dir, Branch: cur})
		}
		_ = relockRepo(dir, repo)
	}
	if err := checkoutUnlocked(home, dir, repo, localBranch); err != nil {
		abort()
		return PR{}, err
	}
	if _, err := ygit.Checkout(ygit.CheckoutOptions{RepoPath: dir, Branch: name, Create: true}); err != nil {
		abort()
		return PR{}, fmt.Errorf("src: branch %s: %w", name, err)
	}
	if err := applyPatchFile(dir, abs); err != nil {
		abort()
		return PR{}, err
	}
	if err := stageAll(dir); err != nil {
		abort()
		return PR{}, err
	}
	if _, entries, err := ygit.Status(dir); err != nil {
		abort()
		return PR{}, err
	} else if len(entries) == 0 {
		abort()
		return PR{}, fmt.Errorf("src: patch %s applied with no changes", filepath.Base(patchFile))
	}
	if err := commitAll(dir, title, agentName(), "bashy@localhost"); err != nil {
		abort()
		return PR{}, err
	}
	if err := relockRepo(dir, repo); err != nil {
		return PR{}, err
	}
	return OpenPR(home, repo, name, title, "")
}
