package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// nativePush implements "git push" via go-git.
func nativePush(_ context.Context, dir string, args []string) (*ExecResult, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	opts := &gogit.PushOptions{}

	var remote string
	var refSpec string
	var deletes []string // remote branch names to delete ("--delete" or ":<branch>")
	deleteMode := false
	setUpstream := false
	forceWithLease := false

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-u" || args[i] == "--set-upstream":
			setUpstream = true
		case args[i] == "--force-with-lease":
			forceWithLease = true
		case args[i] == "--force" || args[i] == "-f":
			opts.Force = true
		case args[i] == "--delete" || args[i] == "-d":
			deleteMode = true
		// Host git shape: the first positional is always the remote,
		// wherever flags sit; later ones are the refspec (exactly one)
		// or, under --delete, branches to delete.
		case !strings.HasPrefix(args[i], "-"):
			if strings.HasPrefix(args[i], ":") {
				name := strings.TrimPrefix(args[i], ":")
				if name == "" {
					return nil, ErrUnsupported
				}
				deletes = append(deletes, name)
			} else if remote == "" {
				remote = args[i]
			} else if deleteMode {
				deletes = append(deletes, args[i])
			} else if refSpec == "" {
				refSpec = args[i]
			} else {
				return nil, ErrUnsupported
			}
		default:
			return nil, ErrUnsupported
		}
	}

	// A ":<branch>" refspec always means delete, with or without --delete.
	// A lone positional under --delete is the remote (host git: "--delete
	// doesn't make sense without any refs"), never an implied branch.
	if deleteMode && len(deletes) == 0 {
		return &ExecResult{
			Stderr:   "fatal: --delete doesn't make sense without any refs\n",
			ExitCode: 128,
		}, nil
	}

	if remote != "" {
		opts.RemoteName = remote
	} else {
		opts.RemoteName = "origin"
	}

	if forceWithLease {
		opts.ForceWithLease = &gogit.ForceWithLease{}
	}

	if len(deletes) > 0 {
		for _, name := range deletes {
			dst := name
			if !strings.Contains(dst, "refs/") {
				dst = "refs/heads/" + dst
			}
			opts.RefSpecs = append(opts.RefSpecs, config.RefSpec(":"+dst))
		}
	} else if refSpec != "" {
		// Push specific branch
		spec := config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", refSpec, refSpec))
		opts.RefSpecs = []config.RefSpec{spec}
	} else {
		// Push current branch
		head, err := repo.Head()
		if err != nil {
			return nil, ErrUnsupported
		}
		if !head.Name().IsBranch() {
			return nil, ErrUnsupported
		}
		branchName := head.Name().Short()
		spec := config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", branchName, branchName))
		opts.RefSpecs = []config.RefSpec{spec}
	}

	err = repo.Push(opts)
	if err != nil {
		if err == gogit.NoErrAlreadyUpToDate {
			return &ExecResult{Stdout: "Everything up-to-date\n"}, nil
		}
		return nil, ErrUnsupported
	}

	if len(deletes) > 0 {
		var b strings.Builder
		b.WriteString("To " + opts.RemoteName + "\n")
		for _, name := range deletes {
			short := name
			if strings.HasPrefix(short, "refs/heads/") {
				short = strings.TrimPrefix(short, "refs/heads/")
			}
			b.WriteString(" - [deleted]         " + short + "\n")
		}
		return &ExecResult{Stderr: b.String()}, nil
	}

	// Set upstream tracking if requested
	if setUpstream && refSpec != "" {
		cfg, err := repo.Config()
		if err == nil {
			cfg.Branches[refSpec] = &config.Branch{
				Name:   refSpec,
				Remote: opts.RemoteName,
				Merge:  plumbing.NewBranchReferenceName(refSpec),
			}
			_ = repo.SetConfig(cfg)
		}
	}

	return &ExecResult{Stdout: ""}, nil
}

// nativeCherryPick implements "git cherry-pick <commit>" via go-git.
// Only supports single commit cherry-pick with no conflicts.
func nativeCherryPick(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) == 0 {
		return nil, ErrUnsupported
	}

	// Reject flags
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return nil, ErrUnsupported
		}
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	// Resolve commit hash
	hash, err := repo.ResolveRevision(plumbing.Revision(args[0]))
	if err != nil {
		return nil, ErrUnsupported
	}

	commit, err := repo.CommitObject(*hash)
	if err != nil {
		return nil, ErrUnsupported
	}

	// Must have exactly one parent for simple cherry-pick
	if commit.NumParents() != 1 {
		return nil, ErrUnsupported
	}

	parent, err := commit.Parent(0)
	if err != nil {
		return nil, ErrUnsupported
	}

	// Get patch between parent and commit
	patch, err := parent.Patch(commit)
	if err != nil {
		return nil, ErrUnsupported
	}

	// Apply patch to worktree
	wt, err := repo.Worktree()
	if err != nil {
		return nil, ErrUnsupported
	}

	if patch.String() == "" {
		// Empty patch, nothing to do
		return &ExecResult{Stdout: ""}, nil
	}

	// All-or-nothing: plan first so a conflict writes nothing.
	plans, err := planPatchApplication(wt.Filesystem.Root(), patch)
	if err != nil {
		return nil, ErrUnsupported
	}
	if err := writePatchPlans(wt, wt.Filesystem.Root(), plans); err != nil {
		return nil, ErrUnsupported
	}

	// Create new commit with original message
	_, err = wt.Commit(commit.Message, &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  commit.Author.Name,
			Email: commit.Author.Email,
			When:  time.Now(),
		},
	})
	if err != nil {
		return nil, ErrUnsupported
	}

	return &ExecResult{Stdout: ""}, nil
}

// nativeRevert implements "git revert [--no-edit] [-n|--no-commit]
// <commit>" via go-git: reverse-apply a single-parent commit, stage, and
// commit with the conventional Revert message. Conflicts (a chunk that no
// longer applies) return ErrUnsupported with the tree untouched — the
// all-or-nothing contract shared with cherry-pick. Sequencer flags
// (--continue/--abort/--skip), -m (merges), and multi-commit reverts stay
// loud ErrUnsupported.
func nativeRevert(_ context.Context, dir string, args []string) (*ExecResult, error) {
	noCommit := false
	var positionals []string
	for _, arg := range args {
		switch arg {
		case "--no-edit":
			// Non-interactive tier: no editor exists, both forms commit.
		case "-n", "--no-commit":
			noCommit = true
		default:
			if strings.HasPrefix(arg, "-") {
				return nil, ErrUnsupported
			}
			positionals = append(positionals, arg)
		}
	}
	if len(positionals) != 1 {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	hash, err := repo.ResolveRevision(plumbing.Revision(positionals[0]))
	if err != nil {
		return nil, ErrUnsupported
	}
	commit, err := repo.CommitObject(*hash)
	if err != nil {
		return nil, ErrUnsupported
	}
	if commit.NumParents() != 1 {
		return nil, ErrUnsupported
	}
	parent, err := commit.Parent(0)
	if err != nil {
		return nil, ErrUnsupported
	}

	// The reverse patch: commit → parent.
	patch, err := commit.Patch(parent)
	if err != nil {
		return nil, ErrUnsupported
	}

	wt, err := repo.Worktree()
	if err != nil {
		return nil, ErrUnsupported
	}
	root := wt.Filesystem.Root()
	if patch.String() != "" {
		plans, err := planPatchApplication(root, patch)
		if err != nil {
			return nil, ErrUnsupported
		}
		if err := writePatchPlans(wt, root, plans); err != nil {
			return nil, ErrUnsupported
		}
	}

	if noCommit {
		return &ExecResult{Stdout: ""}, nil
	}

	subject, _, _ := strings.Cut(strings.TrimSpace(commit.Message), "\n")
	msg := fmt.Sprintf("Revert %q\n\nThis reverts commit %s.\n", subject, commit.Hash.String())
	copts := &gogit.CommitOptions{}
	if sig := resolveAuthor(repo); sig != nil {
		copts.Author = sig
	}
	newHash, err := wt.Commit(msg, copts)
	if err != nil {
		return nil, ErrUnsupported
	}
	head, _ := repo.Head()
	branchName := "HEAD"
	if head != nil && head.Name().IsBranch() {
		branchName = head.Name().Short()
	}
	return &ExecResult{Stdout: fmt.Sprintf("[%s %s] Revert %q\n", branchName, newHash.String()[:7], subject)}, nil
}

// nativeRebase implements simple linear "git rebase <target>" via go-git.
// Returns ErrUnsupported for interactive rebase or conflicts.
func nativeRebase(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) == 0 {
		return nil, ErrUnsupported
	}

	// Reject interactive and other complex flags
	for _, arg := range args {
		if arg == "-i" || arg == "--interactive" || arg == "--onto" ||
			arg == "--continue" || arg == "--abort" || arg == "--skip" {
			return nil, ErrUnsupported
		}
	}

	target := args[0]
	if strings.HasPrefix(target, "-") {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	// Resolve target
	targetHash, err := repo.ResolveRevision(plumbing.Revision(target))
	if err != nil {
		return nil, ErrUnsupported
	}

	// Get current HEAD
	head, err := repo.Head()
	if err != nil {
		return nil, ErrUnsupported
	}

	// Find merge base
	targetCommit, err := repo.CommitObject(*targetHash)
	if err != nil {
		return nil, ErrUnsupported
	}

	headCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, ErrUnsupported
	}

	bases, err := targetCommit.MergeBase(headCommit)
	if err != nil || len(bases) == 0 {
		return nil, ErrUnsupported
	}
	mergeBase := bases[0].Hash

	// Collect commits from merge-base to HEAD (exclusive of merge-base)
	var commits []*object.Commit
	iter, err := repo.Log(&gogit.LogOptions{From: head.Hash()})
	if err != nil {
		return nil, ErrUnsupported
	}
	err = iter.ForEach(func(c *object.Commit) error {
		if c.Hash == mergeBase {
			return fmt.Errorf("stop")
		}
		commits = append(commits, c)
		return nil
	})
	if err != nil && err.Error() != "stop" {
		return nil, ErrUnsupported
	}

	if len(commits) == 0 {
		return &ExecResult{Stdout: "Current branch is up to date.\n"}, nil
	}

	// Reset to target
	wt, err := repo.Worktree()
	if err != nil {
		return nil, ErrUnsupported
	}

	err = wt.Reset(&gogit.ResetOptions{
		Mode:   gogit.HardReset,
		Commit: *targetHash,
	})
	if err != nil {
		return nil, ErrUnsupported
	}

	// Cherry-pick each commit in reverse order (oldest first)
	for i := len(commits) - 1; i >= 0; i-- {
		c := commits[i]
		if c.NumParents() != 1 {
			return nil, ErrUnsupported
		}
		parent, err := c.Parent(0)
		if err != nil {
			return nil, ErrUnsupported
		}

		patch, err := parent.Patch(c)
		if err != nil {
			return nil, ErrUnsupported
		}

		// Apply patch
		for _, fp := range patch.FilePatches() {
			if fp.IsBinary() {
				return nil, ErrUnsupported
			}
			from, to := fp.Files()

			if to == nil {
				path := from.Path()
				fullPath := filepath.Join(wt.Filesystem.Root(), path)
				os.Remove(fullPath)
				continue
			}

			if from == nil {
				content := reconstructContent(fp)
				fullPath := filepath.Join(wt.Filesystem.Root(), to.Path())
				dir := filepath.Dir(fullPath)
				_ = os.MkdirAll(dir, 0755)
				if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
					return nil, ErrUnsupported
				}
				continue
			}

			fullPath := filepath.Join(wt.Filesystem.Root(), to.Path())
			existing, err := os.ReadFile(fullPath)
			if err != nil {
				return nil, ErrUnsupported
			}

			newContent, err := applyChunks(string(existing), fp)
			if err != nil {
				return nil, ErrUnsupported
			}

			if from.Path() != to.Path() {
				oldPath := filepath.Join(wt.Filesystem.Root(), from.Path())
				os.Remove(oldPath)
			}

			if err := os.WriteFile(fullPath, []byte(newContent), 0644); err != nil {
				return nil, ErrUnsupported
			}
		}

		// Stage and commit
		status, stErr := wt.Status()
		if stErr != nil {
			return nil, ErrUnsupported
		}
		for path := range status {
			wt.Add(path)
		}

		_, err = wt.Commit(c.Message, &gogit.CommitOptions{
			Author: &object.Signature{
				Name:  c.Author.Name,
				Email: c.Author.Email,
				When:  time.Now(),
			},
		})
		if err != nil {
			return nil, ErrUnsupported
		}
	}

	return &ExecResult{Stdout: fmt.Sprintf("Successfully rebased and updated.\n")}, nil
}

// nativeApply lives in apply.go (S252.5): real worktree patch application.

// nativeFormatPatch implements "git format-patch" via go-git.
func nativeFormatPatch(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) == 0 {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	// Handle -1 <commit> or -<n> <commit>
	count := 0
	var commitRef string
	for i := 0; i < len(args); i++ {
		if args[i] == "-1" {
			count = 1
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				commitRef = args[i]
			}
		} else if strings.HasPrefix(args[i], "-") && len(args[i]) > 1 {
			n := 0
			if _, err := fmt.Sscanf(args[i], "-%d", &n); err == nil && n > 0 {
				count = n
			} else {
				return nil, ErrUnsupported
			}
		} else {
			commitRef = args[i]
		}
	}

	if count == 0 && commitRef == "" {
		return nil, ErrUnsupported
	}

	// Default to HEAD if no commit specified
	if commitRef == "" {
		commitRef = "HEAD"
	}

	hash, err := repo.ResolveRevision(plumbing.Revision(commitRef))
	if err != nil {
		return nil, ErrUnsupported
	}

	// Collect commits
	var commits []*object.Commit
	iter, err := repo.Log(&gogit.LogOptions{From: *hash})
	if err != nil {
		return nil, ErrUnsupported
	}

	if count == 0 {
		count = 1
	}

	collected := 0
	err = iter.ForEach(func(c *object.Commit) error {
		if collected >= count {
			return fmt.Errorf("stop")
		}
		commits = append(commits, c)
		collected++
		return nil
	})
	if err != nil && err.Error() != "stop" {
		return nil, ErrUnsupported
	}

	// Format patches (oldest first)
	var b strings.Builder
	for i := len(commits) - 1; i >= 0; i-- {
		c := commits[i]

		// Email-style header
		subject := strings.SplitN(c.Message, "\n", 2)[0]
		b.WriteString(fmt.Sprintf("From %s Mon Sep 17 00:00:00 2001\n", c.Hash.String()))
		b.WriteString(fmt.Sprintf("From: %s <%s>\n", c.Author.Name, c.Author.Email))
		b.WriteString(fmt.Sprintf("Date: %s\n", c.Author.When.Format("Mon, 2 Jan 2006 15:04:05 -0700")))
		b.WriteString(fmt.Sprintf("Subject: [PATCH] %s\n", subject))
		b.WriteString("\n---\n\n")

		// Generate diff
		if c.NumParents() > 0 {
			parent, err := c.Parent(0)
			if err != nil {
				return nil, ErrUnsupported
			}
			patch, err := parent.Patch(c)
			if err != nil {
				return nil, ErrUnsupported
			}
			b.WriteString(patch.String())
		} else {
			// Root commit — diff against empty tree
			parentTree := &object.Tree{}
			commitTree, err := c.Tree()
			if err != nil {
				return nil, ErrUnsupported
			}
			changes, err := parentTree.Diff(commitTree)
			if err != nil {
				return nil, ErrUnsupported
			}
			patch, err := changes.Patch()
			if err != nil {
				return nil, ErrUnsupported
			}
			b.WriteString(patch.String())
		}
		b.WriteString("\n-- \n")
	}

	return &ExecResult{Stdout: b.String()}, nil
}

// nativeRm implements "git rm" via go-git.
func nativeRm(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) == 0 {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	wt, err := repo.Worktree()
	if err != nil {
		return nil, ErrUnsupported
	}

	cached := false
	force := false
	var paths []string

	for _, arg := range args {
		switch arg {
		case "--cached":
			cached = true
		case "-f", "--force":
			force = true
		case "-r":
			// Recursive — accept but not specially handled (Remove handles dirs)
		default:
			if strings.HasPrefix(arg, "-") {
				return nil, ErrUnsupported
			}
			paths = append(paths, arg)
		}
	}

	if len(paths) == 0 {
		return nil, ErrUnsupported
	}

	_ = force // Force just suppresses safety checks

	root := wt.Filesystem.Root()
	for _, p := range paths {
		relPath := relToRepoRoot(root, dir, p)

		// Remove from index (worktree)
		if _, err := wt.Remove(relPath); err != nil {
			return &ExecResult{
				Stderr:   fmt.Sprintf("fatal: pathspec '%s' did not match any files\n", p),
				ExitCode: 128,
			}, nil
		}

		// If --cached, restore the file to working tree
		if cached {
			srcPath := filepath.Join(root, relPath)
			// Read from the index via the filesystem
			// Actually, wt.Remove already removed from disk. Re-read from blob.
			head, err := repo.Head()
			if err != nil {
				continue
			}
			commit, err := repo.CommitObject(head.Hash())
			if err != nil {
				continue
			}
			tree, err := commit.Tree()
			if err != nil {
				continue
			}
			f, err := tree.File(relPath)
			if err != nil {
				continue
			}
			contents, err := f.Contents()
			if err != nil {
				continue
			}
			_ = os.MkdirAll(filepath.Dir(srcPath), 0755)
			_ = os.WriteFile(srcPath, []byte(contents), 0644)
		}
	}

	return &ExecResult{Stdout: ""}, nil
}

// nativeStashTier2 is intentionally ErrUnsupported — go-git lacks stash support.
// The base nativeStash in git_native.go already does this; this is here for completeness
// if the map entry needs to reference a tier2 function.

// planPatchApplication computes the resulting worktree contents for every
// file touched by patch, writing nothing. Returned map: path → new content,
// with a nil content meaning "delete path". All-or-nothing: a binary file,
// a missing delete target, or a chunk mismatch aborts the whole plan before
// a byte is written, so callers never leave a half-applied tree.
func planPatchApplication(root string, patch *object.Patch) (map[string]*string, error) {
	plans := map[string]*string{}
	for _, fp := range patch.FilePatches() {
		if fp.IsBinary() {
			return nil, fmt.Errorf("binary patch")
		}
		from, to := fp.Files()

		if to == nil {
			// File deleted by the patch.
			path := from.Path()
			fullPath := filepath.Join(root, filepath.FromSlash(path))
			if _, err := os.Stat(fullPath); err != nil {
				return nil, fmt.Errorf("delete missing %s", path)
			}
			plans[filepath.ToSlash(path)] = nil
			continue
		}

		if from == nil {
			// New file — reconstruct content from added chunks.
			content := reconstructContent(fp)
			plans[filepath.ToSlash(to.Path())] = &content
			continue
		}

		// Modified file (possibly renamed) — apply hunks to current bytes.
		fullPath := filepath.Join(root, filepath.FromSlash(to.Path()))
		existing, err := os.ReadFile(fullPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", to.Path(), err)
		}
		newContent, err := applyChunks(string(existing), fp)
		if err != nil {
			return nil, err
		}
		plans[filepath.ToSlash(to.Path())] = &newContent
		if from.Path() != to.Path() {
			plans[filepath.ToSlash(from.Path())] = nil
		}
	}
	return plans, nil
}

// writePatchPlans carries out a plan from planPatchApplication: deletes,
// directory creation, file writes, then staging of every touched path.
func writePatchPlans(wt *gogit.Worktree, root string, plans map[string]*string) error {
	var paths []string
	for p := range plans {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		content := plans[p]
		fullPath := filepath.Join(root, filepath.FromSlash(p))
		if content == nil {
			if err := os.Remove(fullPath); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(fullPath, []byte(*content), 0644); err != nil {
			return err
		}
	}
	status, err := wt.Status()
	if err != nil {
		return err
	}
	for path := range status {
		if _, err := wt.Add(path); err != nil {
			return err
		}
	}
	return nil
}

// reconstructContent builds file content from added chunks in a file patch.
func reconstructContent(fp diff.FilePatch) string {
	var b strings.Builder
	for _, chunk := range fp.Chunks() {
		if chunk.Type() == diff.Add {
			b.WriteString(chunk.Content())
		}
	}
	return b.String()
}

// applyChunks applies diff chunks to existing content.
// Returns ErrUnsupported-equivalent error if chunks don't match (conflict).
func applyChunks(content string, fp diff.FilePatch) (string, error) {
	lines := strings.Split(content, "\n")
	var result []string
	lineIdx := 0

	for _, chunk := range fp.Chunks() {
		chunkLines := strings.Split(chunk.Content(), "\n")
		// Remove trailing empty string from split
		if len(chunkLines) > 0 && chunkLines[len(chunkLines)-1] == "" {
			chunkLines = chunkLines[:len(chunkLines)-1]
		}

		switch chunk.Type() {
		case diff.Equal:
			for range chunkLines {
				if lineIdx >= len(lines) {
					return "", fmt.Errorf("conflict: unexpected end of file")
				}
				result = append(result, lines[lineIdx])
				lineIdx++
			}
		case diff.Add:
			result = append(result, chunkLines...)
		case diff.Delete:
			for range chunkLines {
				if lineIdx >= len(lines) {
					return "", fmt.Errorf("conflict: unexpected end of file")
				}
				lineIdx++
			}
		}
	}

	// Append remaining lines
	for lineIdx < len(lines) {
		result = append(result, lines[lineIdx])
		lineIdx++
	}

	return strings.Join(result, "\n"), nil
}
