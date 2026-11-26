// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package loom

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/qiangli/yoke/pkg/artifact"
)

// PublishedRefPrefix is the canonical immutable Git ref namespace under which
// Loom retains source commits for published internal modules and artifacts.
const PublishedRefPrefix = "refs/bashy/published/"

// ErrImmutablePublishedRef is returned when a caller or Git push attempts to
// delete, overwrite, or move an existing refs/bashy/published/* reference.
var ErrImmutablePublishedRef = errors.New("loom: published artifact ref is immutable")

const (
	managedUpdateHookMarker = "# Managed by bashy loom: chains hooks/update.d/*"
	publishedRefGuardName   = "bashy-published-refs"
	chainedLegacyUpdateName = "00-existing-update"
)

const updateHookDispatcherScript = `#!/bin/sh
# Managed by bashy loom: chains hooks/update.d/*
set -eu
hook_dir=$(CDPATH= cd -P "$(dirname "$0")" && pwd)
if [ -d "$hook_dir/update.d" ]; then
  for h in "$hook_dir/update.d"/*; do
    if [ -f "$h" ] && [ -x "$h" ]; then
      "$h" "$@" || exit $?
    fi
  done
fi
exit 0
`

const publishedRefGuardScript = `#!/bin/sh
# Rejects deletion or rewrite of immutable refs/bashy/published/* references
# while permitting normal branch updates and deletions on refs/heads/*.
set -eu
refname=${1:-}
oldrev=${2:-}
newrev=${3:-}
case "$refname" in
  refs/bashy/published/*)
    if [ -z "$newrev" ] || [ -z "$(printf '%s' "$newrev" | tr -d '0')" ]; then
      echo "loom: refusing to delete immutable published artifact ref $refname" >&2
      exit 1
    fi
    if [ -n "$oldrev" ] && [ -n "$(printf '%s' "$oldrev" | tr -d '0')" ] && [ "$oldrev" != "$newrev" ]; then
      echo "loom: refusing to overwrite immutable published artifact ref $refname ($oldrev -> $newrev)" >&2
      exit 1
    fi
    ;;
esac
exit 0
`

// IsPublishedArtifactRef reports whether name is under refs/bashy/published/.
func IsPublishedArtifactRef(name plumbing.ReferenceName) bool {
	s := string(name)
	return strings.HasPrefix(s, PublishedRefPrefix) && len(s) > len(PublishedRefPrefix)
}

// CheckRefMutationAllowed enforces immutability on refs/bashy/published/*:
//   - Creation (oldHash.IsZero() && !newHash.IsZero()) is allowed.
//   - Idempotent no-op (!newHash.IsZero() && oldHash == newHash) is allowed.
//   - Deletion (newHash.IsZero()) or rewrite/move (oldHash != newHash) fails
//     closed with ErrImmutablePublishedRef.
//   - Non-published refs (such as refs/heads/*) are allowed to move or delete.
func CheckRefMutationAllowed(refName string, oldHash, newHash plumbing.Hash) error {
	name := plumbing.ReferenceName(strings.TrimSpace(refName))
	if !strings.HasPrefix(string(name), PublishedRefPrefix) {
		return nil
	}
	if !IsPublishedArtifactRef(name) {
		return fmt.Errorf("%w: empty published ref suffix in %q", ErrImmutablePublishedRef, refName)
	}
	if err := name.Validate(); err != nil {
		return fmt.Errorf("%w: invalid ref %q: %v", ErrImmutablePublishedRef, refName, err)
	}
	if newHash.IsZero() {
		return fmt.Errorf("%w: refusing deletion of %s (old=%s)", ErrImmutablePublishedRef, name, oldHash)
	}
	if oldHash.IsZero() || oldHash == newHash {
		return nil
	}
	return fmt.Errorf("%w: refusing mutation of %s (%s -> %s)", ErrImmutablePublishedRef, name, oldHash, newHash)
}

func writeExecutableHookAtomic(dstPath string, content []byte) error {
	dir := filepath.Dir(dstPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dstPath)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dstPath); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func hookAlreadyDelegatesToUpdateD(raw string) bool {
	if strings.Contains(raw, managedUpdateHookMarker) {
		return true
	}
	return strings.Contains(raw, "update.d") || strings.Contains(raw, "${hookname}.d") || strings.Contains(raw, "$hookname.d")
}

// InstallPublishedRefProtectionHook installs an executable POSIX hooks/update
// guard inside bareRepoDir (chaining via hooks/update.d/bashy-published-refs
// when hooks/update already exists) so git push --delete and git push --force
// against refs/bashy/published/* are rejected while branches on refs/heads/*
// remain free to advance, force-push, or delete.
func InstallPublishedRefProtectionHook(bareRepoDir string) error {
	dir := strings.TrimSpace(bareRepoDir)
	if dir == "" {
		return errors.New("loom: bare repository directory is required")
	}
	repo, err := git.PlainOpen(dir)
	if err != nil {
		return fmt.Errorf("loom: open repository %s: %w", dir, err)
	}
	if cfg, cfgErr := repo.Config(); cfgErr == nil && !cfg.Core.IsBare {
		return fmt.Errorf("loom: repository %s must be bare to install update hook", dir)
	}

	hooksDir := filepath.Join(dir, "hooks")
	updateD := filepath.Join(hooksDir, "update.d")
	if err := os.MkdirAll(updateD, 0o755); err != nil {
		return err
	}

	guardPath := filepath.Join(updateD, publishedRefGuardName)
	if err := writeExecutableHookAtomic(guardPath, []byte(publishedRefGuardScript)); err != nil {
		return fmt.Errorf("loom: write %s: %w", guardPath, err)
	}

	updateHookPath := filepath.Join(hooksDir, "update")
	existingBytes, err := os.ReadFile(updateHookPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("loom: read %s: %w", updateHookPath, err)
		}
		return writeExecutableHookAtomic(updateHookPath, []byte(updateHookDispatcherScript))
	}

	existingText := string(existingBytes)
	if hookAlreadyDelegatesToUpdateD(existingText) {
		// Ensure the existing delegating hook is executable; preserve Gitea's own dispatcher intact.
		if err := os.Chmod(updateHookPath, 0o755); err != nil {
			return err
		}
		return nil
	}

	// Chain a standalone pre-existing hooks/update into hooks/update.d/00-existing-update.
	chainedPath := filepath.Join(updateD, chainedLegacyUpdateName)
	if _, statErr := os.Stat(chainedPath); errors.Is(statErr, os.ErrNotExist) {
		if err := writeExecutableHookAtomic(chainedPath, existingBytes); err != nil {
			return fmt.Errorf("loom: preserve existing update hook at %s: %w", chainedPath, err)
		}
	}
	return writeExecutableHookAtomic(updateHookPath, []byte(updateHookDispatcherScript))
}

func verifyGitTreeAndBlobs(s storer.EncodedObjectStorer, treeHash plumbing.Hash, visited map[plumbing.Hash]bool) error {
	if visited[treeHash] {
		return nil
	}
	tree, err := object.GetTree(s, treeHash)
	if err != nil {
		return fmt.Errorf("read tree %s: %w", treeHash, err)
	}
	for _, entry := range tree.Entries {
		switch entry.Mode {
		case filemode.Submodule:
			continue
		case filemode.Dir:
			if err := verifyGitTreeAndBlobs(s, entry.Hash, visited); err != nil {
				return fmt.Errorf("%s: %w", entry.Name, err)
			}
		default:
			if visited[entry.Hash] {
				continue
			}
			blob, err := object.GetBlob(s, entry.Hash)
			if err != nil {
				return fmt.Errorf("missing blob %s (%s): %w", entry.Name, entry.Hash, err)
			}
			rc, err := blob.Reader()
			if err != nil {
				return fmt.Errorf("open blob %s (%s): %w", entry.Name, entry.Hash, err)
			}
			n, copyErr := io.Copy(io.Discard, rc)
			closeErr := rc.Close()
			if copyErr != nil {
				return fmt.Errorf("corrupt blob %s (%s): %w", entry.Name, entry.Hash, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close blob %s (%s): %w", entry.Name, entry.Hash, closeErr)
			}
			if n != blob.Size {
				return fmt.Errorf("blob %s (%s) size %d != %d", entry.Name, entry.Hash, n, blob.Size)
			}
			visited[entry.Hash] = true
		}
	}
	visited[treeHash] = true
	return nil
}

// VerifyPublishedCommitsIntact opens bareRepoDir and verifies that every
// PublicationRecord's LoomRef resolves to its exact Commit and that its full
// Git tree and all referenced blobs are intact and readable, even after
// branches have moved or been deleted.
func VerifyPublishedCommitsIntact(bareRepoDir string, pubs []artifact.PublicationRecord) error {
	dir := strings.TrimSpace(bareRepoDir)
	if dir == "" {
		return errors.New("loom: bare repository directory is required")
	}
	repo, err := git.PlainOpen(dir)
	if err != nil {
		return fmt.Errorf("loom: open repository %s: %w", dir, err)
	}

	visited := make(map[plumbing.Hash]bool)
	for _, pub := range pubs {
		refName := plumbing.ReferenceName(pub.LoomRef)
		if !IsPublishedArtifactRef(refName) {
			return fmt.Errorf("loom: invalid published ref %q for %s@%s", pub.LoomRef, pub.Module, pub.Version)
		}
		if len(pub.Commit) != 40 {
			return fmt.Errorf("loom: invalid commit hash %q for %s@%s", pub.Commit, pub.Module, pub.Version)
		}
		ref, err := repo.Reference(refName, true)
		if err != nil {
			return fmt.Errorf("loom: missing published ref %s for %s@%s: %w", pub.LoomRef, pub.Module, pub.Version, err)
		}
		if ref.Hash().String() != pub.Commit {
			return fmt.Errorf("loom: published ref %s points to %s, want %s for %s@%s",
				pub.LoomRef, ref.Hash(), pub.Commit, pub.Module, pub.Version)
		}
		commitObj, err := repo.CommitObject(ref.Hash())
		if err != nil {
			return fmt.Errorf("loom: missing commit %s for %s: %w", pub.Commit, pub.LoomRef, err)
		}
		if err := verifyGitTreeAndBlobs(repo.Storer, commitObj.TreeHash, visited); err != nil {
			return fmt.Errorf("loom: corrupt tree for %s@%s (%s): %w", pub.Module, pub.Version, pub.Commit, err)
		}
		if pub.Subdir != "" && pub.Subdir != "." {
			rootTree, err := commitObj.Tree()
			if err != nil {
				return fmt.Errorf("loom: load root tree %s: %w", pub.Commit, err)
			}
			if _, err := rootTree.Tree(pub.Subdir); err != nil {
				return fmt.Errorf("loom: missing publication subdir %q in %s: %w", pub.Subdir, pub.Commit, err)
			}
		}
	}
	return nil
}
