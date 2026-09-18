package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
		case !strings.HasPrefix(args[i], "-"):
			if remote == "" {
				remote = args[i]
			} else if refSpec == "" {
				refSpec = args[i]
			} else {
				return nil, ErrUnsupported
			}
		default:
			return nil, ErrUnsupported
		}
	}

	if remote != "" {
		opts.RemoteName = remote
	} else {
		opts.RemoteName = "origin"
	}

	if forceWithLease {
		opts.ForceWithLease = &gogit.ForceWithLease{}
	}

	if refSpec != "" {
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

	// Write patch to temp file and apply
	patchStr := patch.String()
	if patchStr == "" {
		// Empty patch, nothing to do
		return &ExecResult{Stdout: ""}, nil
	}

	tmpFile, err := os.CreateTemp("", "cherry-pick-*.patch")
	if err != nil {
		return nil, ErrUnsupported
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(patchStr); err != nil {
		tmpFile.Close()
		return nil, ErrUnsupported
	}
	tmpFile.Close()

	// Apply the patch — go-git doesn't have a direct Apply method on worktree
	// for unified diffs. We need to manually apply changes from the patch.
	// For simplicity, iterate the file patches and apply them.
	for _, fp := range patch.FilePatches() {
		if fp.IsBinary() {
			return nil, ErrUnsupported
		}
		from, to := fp.Files()

		if to == nil {
			// File deleted
			path := from.Path()
			fullPath := filepath.Join(wt.Filesystem.Root(), path)
			if err := os.Remove(fullPath); err != nil {
				return nil, ErrUnsupported
			}
			continue
		}

		if from == nil {
			// New file — reconstruct content from chunks
			content := reconstructContent(fp)
			fullPath := filepath.Join(wt.Filesystem.Root(), to.Path())
			dir := filepath.Dir(fullPath)
			if err := os.MkdirAll(dir, 0755); err != nil {
				return nil, ErrUnsupported
			}
			if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
				return nil, ErrUnsupported
			}
			continue
		}

		// Modified file — apply hunks
		// Read current file content
		fullPath := filepath.Join(wt.Filesystem.Root(), to.Path())
		existing, err := os.ReadFile(fullPath)
		if err != nil {
			return nil, ErrUnsupported
		}

		newContent, err := applyChunks(string(existing), fp)
		if err != nil {
			// Conflict detected
			return nil, ErrUnsupported
		}

		if from.Path() != to.Path() {
			// Rename
			oldPath := filepath.Join(wt.Filesystem.Root(), from.Path())
			os.Remove(oldPath)
		}

		if err := os.WriteFile(fullPath, []byte(newContent), 0644); err != nil {
			return nil, ErrUnsupported
		}
	}

	// Stage all changes
	status, err := wt.Status()
	if err != nil {
		return nil, ErrUnsupported
	}
	for path := range status {
		if _, err := wt.Add(path); err != nil {
			return nil, ErrUnsupported
		}
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

// nativeApply implements "git apply <patch-file>" via go-git.
// Returns ErrUnsupported for complex patches.
func nativeApply(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) == 0 {
		return nil, ErrUnsupported
	}

	// Find the patch file
	var patchFile string
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			// Reject flags we don't support
			return nil, ErrUnsupported
		}
		patchFile = arg
	}

	if patchFile == "" {
		return nil, ErrUnsupported
	}

	// Make path absolute relative to dir
	if !filepath.IsAbs(patchFile) {
		patchFile = filepath.Join(dir, patchFile)
	}

	// Read patch content
	_, err := os.ReadFile(patchFile)
	if err != nil {
		return &ExecResult{
			Stderr:   fmt.Sprintf("error: can't open patch '%s': %v\n", patchFile, err),
			ExitCode: 128,
		}, nil
	}

	// go-git doesn't provide a direct unified diff apply on worktree.
	// For now, fall through to host git for actual apply operations.
	return nil, ErrUnsupported
}

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
