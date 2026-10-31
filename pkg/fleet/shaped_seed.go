// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package fleet

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	ygit "github.com/qiangli/yoke/git"
)

// SeedShapedSource makes dir the managed checkout of a shaped app whose base
// tree the HOST carries — bytes embedded in a binary rather than a directory
// the operator points `app add --type shaped --source` at. The result is the
// same checkout importShapedApp produces (one repository, a `pristine`
// branch at the base revision, per-principal lines recorded beside it by
// pkg/src), reached from a writer instead of a path, so a built-in surface is
// shaped through exactly the mechanism every registered app uses and nothing
// else.
//
// write lays the base tree out under the root it is given. On the first call
// that tree becomes the import commit. On every later call it is compared to
// the current base and, when the host's bytes changed (an upgrade), recorded
// as a new base commit that `pristine` moves to — the shaped counterpart of
// `src fetch` leaving pristine at the release commit. Revisions principals
// already hold stay reachable: they are commits in the same repository and
// nothing here rewrites them. The returned SHA is the base revision.
func SeedShapedSource(dir, message string, write func(root string) error) (string, error) {
	if write == nil {
		return "", errors.New("fleet: seed shaped source: no base tree writer")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: %w", err)
	}
	dir = abs
	if message == "" {
		message = "Import shaped app"
	}
	if st, statErr := os.Stat(filepath.Join(dir, ".git")); statErr == nil && st.IsDir() {
		return refreshShapedBase(dir, message, write)
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return "", fmt.Errorf("fleet: seed shaped source: %w", statErr)
	}

	root := filepath.Dir(dir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: %w", err)
	}
	tmp, err := os.MkdirTemp(root, "."+filepath.Base(dir)+"-seed-*")
	if err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: %w", err)
	}
	defer os.RemoveAll(tmp)
	if err := write(tmp); err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: write base tree: %w", err)
	}
	if err := checkShapedManifest(tmp); err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: %w", err)
	}
	if _, err := ygit.Init(ygit.InitOptions{Path: tmp}); err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: initialize checkout: %w", err)
	}
	if err := commitShapedBase(tmp, message); err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: %w", err)
	}
	if err := ensureShapedBase(tmp); err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: %w", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		// A concurrent seeder installed the checkout first: its base is
		// the same bytes, so refresh against it rather than fail.
		if st, statErr := os.Stat(filepath.Join(dir, ".git")); statErr == nil && st.IsDir() {
			return refreshShapedBase(dir, message, write)
		}
		return "", fmt.Errorf("fleet: seed shaped source: install checkout: %w", err)
	}
	return ygit.ResolveCommit(dir, shapedBaseBranch)
}

// refreshShapedBase brings an existing managed checkout's base up to the
// writer's tree: a no-op returning the current base when nothing changed,
// otherwise one new commit on HEAD that pristine is moved to.
func refreshShapedBase(dir, message string, write func(root string) error) (string, error) {
	if err := ensureShapedBase(dir); err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: managed checkout: %w", err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+"-seed-*")
	if err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: %w", err)
	}
	defer os.RemoveAll(tmp)
	if err := write(tmp); err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: write base tree: %w", err)
	}
	if err := checkShapedManifest(tmp); err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: %w", err)
	}
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: open checkout: %w", err)
	}
	baseRef, err := r.Reference(plumbing.NewBranchReferenceName(shapedBaseBranch), true)
	if err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: %s: %w", shapedBaseBranch, err)
	}
	baseCommit, err := r.CommitObject(baseRef.Hash())
	if err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: base commit: %w", err)
	}
	baseTree, err := baseCommit.Tree()
	if err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: base tree: %w", err)
	}
	same, err := treeMatchesDir(baseTree, tmp)
	if err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: compare base: %w", err)
	}
	if same {
		return baseRef.Hash().String(), nil
	}
	if err := replaceWorktree(dir, tmp); err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: replace base tree: %w", err)
	}
	if err := commitShapedBase(dir, message); err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: %w", err)
	}
	head, err := ygit.ResolveCommit(dir, "HEAD")
	if err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: new base: %w", err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(shapedBaseBranch), plumbing.NewHash(head))); err != nil {
		return "", fmt.Errorf("fleet: seed shaped source: move %s: %w", shapedBaseBranch, err)
	}
	return head, nil
}

// commitShapedBase stages everything under dir and records one commit. A
// fresh host has no configured identity, so the retry uses bashy's service
// identity instead of asking the user to configure git.
func commitShapedBase(dir, message string) error {
	if _, err := ygit.Add(ygit.AddOptions{RepoPath: dir, All: true}); err != nil {
		return err
	}
	if _, err := ygit.Commit(ygit.CommitOptions{RepoPath: dir, Message: message}); err != nil {
		if _, fallbackErr := ygit.Commit(ygit.CommitOptions{
			RepoPath: dir, Message: message,
			AuthorName: shapedAuthorName, AuthorEmail: shapedAuthorMail,
		}); fallbackErr != nil {
			return fallbackErr
		}
	}
	return nil
}

// treeMatchesDir reports whether the regular files under dir are exactly
// the blobs of tree: same paths, same contents. Modes are not compared; a
// shaped base is served, not executed.
func treeMatchesDir(tree *object.Tree, dir string) (bool, error) {
	want := map[string]plumbing.Hash{}
	if err := tree.Files().ForEach(func(f *object.File) error {
		want[f.Name] = f.Hash
		return nil
	}); err != nil {
		return false, err
	}
	got := 0
	match := true
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == ".git" && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		got++
		if h, ok := want[filepath.ToSlash(rel)]; !ok || h != plumbing.ComputeHash(plumbing.BlobObject, data) {
			match = false
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return match && got == len(want), nil
}

// replaceWorktree swaps the checkout's working files for the tree under
// src, leaving .git alone.
func replaceWorktree(dir, src string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return copyShapedTree(src, dir)
}
