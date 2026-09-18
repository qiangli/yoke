package git

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/utils/merkletrie"
)

// isAncestor reports whether commit a is an ancestor of (or equal to)
// commit b. On shallow clones the history walk can hit the depth
// boundary; a missing object is treated as "cannot prove ancestry"
// (false) rather than an error, so callers degrade to the divergence
// message instead of failing outright.
func isAncestor(r *gogit.Repository, a, b plumbing.Hash) (bool, error) {
	if a == b {
		return true, nil
	}
	ca, err := r.CommitObject(a)
	if err != nil {
		return false, fmt.Errorf("commit %s: %w", a, err)
	}
	cb, err := r.CommitObject(b)
	if err != nil {
		return false, fmt.Errorf("commit %s: %w", b, err)
	}
	ok, err := ca.IsAncestor(cb)
	if err != nil {
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("ancestry walk: %w", err)
	}
	return ok, nil
}

// treeChanges returns the tree-level changes from commit a to commit b.
func treeChanges(r *gogit.Repository, a, b plumbing.Hash) (object.Changes, error) {
	ca, err := r.CommitObject(a)
	if err != nil {
		return nil, fmt.Errorf("commit %s: %w", a, err)
	}
	cb, err := r.CommitObject(b)
	if err != nil {
		return nil, fmt.Errorf("commit %s: %w", b, err)
	}
	ta, err := ca.Tree()
	if err != nil {
		return nil, fmt.Errorf("tree %s: %w", a, err)
	}
	tb, err := cb.Tree()
	if err != nil {
		return nil, fmt.Errorf("tree %s: %w", b, err)
	}
	return object.DiffTree(ta, tb)
}

// ffUpdate fast-forwards the checked-out branch from oldHash to target:
// branch ref, index, and working tree all end up at target. Local
// uncommitted changes survive as long as they don't touch a path the
// fast-forward changes — the same rule real `git merge --ff-only`
// applies. A conflicting local change aborts with a git-style error
// before anything is mutated.
func ffUpdate(r *gogit.Repository, w *gogit.Worktree, branch plumbing.ReferenceName, oldHash, target plumbing.Hash) error {
	st, err := w.Status()
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	if st.IsClean() {
		// MergeReset moves the branch ref and rebuilds index + worktree.
		// Safe here: the tree is clean, so nothing can be lost.
		if err := w.Reset(&gogit.ResetOptions{Mode: gogit.MergeReset, Commit: target}); err != nil {
			return fmt.Errorf("fast-forward: %w", err)
		}
		return nil
	}

	// Dirty worktree. go-git's MergeReset refuses on any unstaged change
	// and its HardReset deletes untracked files, so neither matches git's
	// behavior. Apply the tree diff by hand instead: only paths changed
	// between oldHash and target are touched, and the overlap check below
	// guarantees none of them carry local modifications.
	changes, err := treeChanges(r, oldHash, target)
	if err != nil {
		return err
	}

	dirty := map[string]bool{}
	for path, fs := range st {
		if fs.Staging == gogit.Unmodified && fs.Worktree == gogit.Unmodified {
			continue
		}
		dirty[path] = true
	}

	var conflicts []string
	seen := map[string]bool{}
	for _, ch := range changes {
		for _, name := range []string{ch.From.Name, ch.To.Name} {
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			if dirty[name] {
				conflicts = append(conflicts, name)
			}
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return fmt.Errorf("your local changes to the following files would be overwritten by merge:\n\t%s\ncommit or discard them first", strings.Join(conflicts, "\n\t"))
	}

	idx, err := r.Storer.Index()
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	root := w.Filesystem.Root()
	for _, ch := range changes {
		action, err := ch.Action()
		if err != nil {
			return fmt.Errorf("change action: %w", err)
		}
		switch action {
		case merkletrie.Delete:
			full := filepath.Join(root, filepath.FromSlash(ch.From.Name))
			if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove %s: %w", ch.From.Name, err)
			}
			if _, err := idx.Remove(ch.From.Name); err != nil && !errors.Is(err, index.ErrEntryNotFound) {
				return fmt.Errorf("index remove %s: %w", ch.From.Name, err)
			}
			removeEmptyParents(root, ch.From.Name)
		case merkletrie.Insert, merkletrie.Modify:
			// A rename surfaces as Delete+Insert, so From handling above
			// plus this branch covers it.
			if ch.To.TreeEntry.Mode == filemode.Submodule {
				return fmt.Errorf("fast-forward touches submodule %q, which cannot be updated with a dirty worktree — commit or discard local changes and retry", ch.To.Name)
			}
			size, err := writeBlobToWorktree(r, root, ch.To.Name, ch.To.TreeEntry)
			if err != nil {
				return err
			}
			entry, err := idx.Entry(ch.To.Name)
			if err != nil {
				entry = idx.Add(ch.To.Name)
			}
			entry.Hash = ch.To.TreeEntry.Hash
			entry.Mode = ch.To.TreeEntry.Mode
			entry.Size = uint32(size)
			now := time.Now()
			entry.ModifiedAt = now
			if entry.CreatedAt.IsZero() {
				entry.CreatedAt = now
			}
		}
	}
	if err := r.Storer.SetIndex(idx); err != nil {
		return fmt.Errorf("write index: %w", err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference(branch, target)); err != nil {
		return fmt.Errorf("update %s: %w", branch, err)
	}
	return nil
}

// writeBlobToWorktree materializes one tree entry under root and
// returns the blob size.
func writeBlobToWorktree(r *gogit.Repository, root, name string, te object.TreeEntry) (int64, error) {
	blob, err := r.BlobObject(te.Hash)
	if err != nil {
		return 0, fmt.Errorf("blob %s (%s): %w", te.Hash, name, err)
	}
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir for %s: %w", name, err)
	}
	rd, err := blob.Reader()
	if err != nil {
		return 0, fmt.Errorf("read blob %s: %w", name, err)
	}
	defer rd.Close()

	if te.Mode == filemode.Symlink {
		data, err := io.ReadAll(rd)
		if err != nil {
			return 0, fmt.Errorf("read symlink blob %s: %w", name, err)
		}
		_ = os.Remove(full)
		if err := os.Symlink(string(data), full); err != nil {
			return 0, fmt.Errorf("symlink %s: %w", name, err)
		}
		return blob.Size, nil
	}

	mode, err := te.Mode.ToOSFileMode()
	if err != nil {
		mode = 0o644
	}
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return 0, fmt.Errorf("write %s: %w", name, err)
	}
	if _, err := io.Copy(f, rd); err != nil {
		f.Close()
		return 0, fmt.Errorf("write %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("close %s: %w", name, err)
	}
	// O_TRUNC reuses an existing file's permissions, so a mode-only
	// change (e.g. adding the exec bit) would be lost on the worktree.
	// Chmod explicitly to honor the target tree entry's mode.
	if err := os.Chmod(full, mode.Perm()); err != nil {
		return 0, fmt.Errorf("chmod %s: %w", name, err)
	}
	return blob.Size, nil
}

// removeEmptyParents deletes now-empty parent directories of name,
// stopping at root. Best-effort: a non-empty dir ends the walk.
func removeEmptyParents(root, name string) {
	dir := filepath.Dir(filepath.Join(root, filepath.FromSlash(name)))
	for dir != root && strings.HasPrefix(dir, root) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// MergeOptions configures a Merge call.
type MergeOptions struct {
	RepoPath string
	Ref      string // branch, tag, or commit to merge into HEAD
	NoFF     bool   // record a merge commit even when fast-forward is possible
	Message  string // merge-commit message override (NoFF only)
}

// Merge integrates opts.Ref into the current branch. Fast-forward
// merges (the target is a descendant of HEAD) are fully supported,
// including --no-ff merge commits. Diverged histories perform a real
// 3-way merge via threeWayMerge; unreconcilable paths come back as a
// *ConflictError with the repository left untouched.
func Merge(opts MergeOptions) (*Result, error) {
	if opts.RepoPath == "" {
		opts.RepoPath = "."
	}
	if opts.Ref == "" {
		return nil, errors.New("merge: a branch, tag, or commit to merge is required")
	}
	r, err := gogit.PlainOpen(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	w, err := r.Worktree()
	if err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}
	headRef, err := r.Head()
	if err != nil {
		return nil, fmt.Errorf("HEAD: %w", err)
	}
	if !headRef.Name().IsBranch() {
		return nil, errors.New("merge: not on a branch (detached HEAD)")
	}
	targetHash, err := r.ResolveRevision(plumbing.Revision(opts.Ref))
	if err != nil {
		return nil, fmt.Errorf("merge: %q — not something we can merge: %w", opts.Ref, err)
	}

	headHash := headRef.Hash()
	if behind, err := isAncestor(r, *targetHash, headHash); err != nil {
		return nil, err
	} else if behind {
		return &Result{Success: true, Message: "Already up to date."}, nil
	}
	ff, err := isAncestor(r, headHash, *targetHash)
	if err != nil {
		return nil, err
	}
	if !ff {
		// Diverged histories: a genuine 3-way merge. threeWayMerge is
		// all-or-nothing — it returns a *ConflictError without mutating
		// anything when paths can't be reconciled, so no separate
		// --abort step is needed. A real merge always records a merge
		// commit, so NoFF is implied here.
		sig, err := commitSignature(r)
		if err != nil {
			return nil, err
		}
		msg := opts.Message
		if msg == "" {
			msg = fmt.Sprintf("Merge %s into %s", opts.Ref, headRef.Name().Short())
		}
		return threeWayMerge(r, w, headHash, *targetHash, msg, sig)
	}

	if !opts.NoFF {
		if err := ffUpdate(r, w, headRef.Name(), headHash, *targetHash); err != nil {
			return nil, err
		}
		return &Result{
			Success: true,
			Message: fmt.Sprintf("Updating %s..%s\nFast-forward", shortHash(headHash), shortHash(*targetHash)),
		}, nil
	}

	// --no-ff: advance the tree to target, then record a merge commit
	// with both parents. Requires a clean tree (the commit captures the
	// index verbatim) and a configured identity (checked up front so we
	// don't move the branch and then fail to commit).
	st, err := w.Status()
	if err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	if !st.IsClean() {
		return nil, errors.New("merge --no-ff: working tree is not clean — commit or discard local changes first")
	}
	sig, err := commitSignature(r)
	if err != nil {
		return nil, err
	}
	if err := w.Reset(&gogit.ResetOptions{Mode: gogit.MergeReset, Commit: *targetHash}); err != nil {
		return nil, fmt.Errorf("merge --no-ff: %w", err)
	}
	msg := opts.Message
	if msg == "" {
		msg = fmt.Sprintf("Merge %s into %s", opts.Ref, headRef.Name().Short())
	}
	commit, err := w.Commit(msg, &gogit.CommitOptions{
		Author:            sig,
		Parents:           []plumbing.Hash{headHash, *targetHash},
		AllowEmptyCommits: true,
	})
	if err != nil {
		return nil, fmt.Errorf("merge commit: %w", err)
	}
	return &Result{
		Success: true,
		Message: fmt.Sprintf("Merge made by fast-forward strategy with merge commit %s", shortHash(commit)),
	}, nil
}

// commitSignature resolves the committer identity from git config
// (repo then global). Errors with a how-to-fix hint when absent —
// callers use this to fail fast before mutating any ref.
func commitSignature(r *gogit.Repository) (*object.Signature, error) {
	// LocalScope merges system + global + repo-local config, so a
	// repo-local `git config user.name` takes precedence over global —
	// matching real git's identity resolution.
	cfg, err := r.ConfigScoped(config.LocalScope)
	if err != nil || cfg.User.Name == "" || cfg.User.Email == "" {
		return nil, fmt.Errorf("user identity not configured — run %q and %q first", CLIName+" config user.name <name>", CLIName+" config user.email <email>")
	}
	return &object.Signature{Name: cfg.User.Name, Email: cfg.User.Email, When: time.Now()}, nil
}

func shortHash(h plumbing.Hash) string {
	s := h.String()
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
