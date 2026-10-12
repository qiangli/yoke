// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package weave

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// weavePreserveNestedWork commits every initialized submodule's uncommitted
// work (innermost first) and imports each submodule tip that differs from the
// gitlink its parent records into root as refs/salvage/abandoned-<id>-sub-<path>.
// Submodules that were never populated (including an interrupted hydration's
// empty directories) hold nothing and are skipped. A populated submodule whose
// state cannot be read is an error: the caller must refuse to destroy it.
// It returns the refs created or reused.
func weavePreserveNestedWork(root, workspace string, id int64) ([]string, error) {
	return weavePreserveNestedLevel(root, workspace, "", id)
}

func weavePreserveNestedLevel(root, workspace, rel string, id int64) ([]string, error) {
	dir := filepath.Join(workspace, rel)
	staged, err := gitOut(dir, "ls-files", "--stage")
	if err != nil {
		return nil, fmt.Errorf("list gitlinks in %s: %w", dirLabel(rel), err)
	}
	var refs []string
	for _, line := range strings.Split(strings.TrimSpace(staged), "\n") {
		meta, path, ok := strings.Cut(line, "\t")
		if !ok || !strings.HasPrefix(meta, "160000 ") {
			continue
		}
		subRel := filepath.Join(rel, path)
		subDir := filepath.Join(workspace, subRel)
		if _, err := os.Stat(filepath.Join(subDir, ".git")); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // never populated (interrupted hydration): nothing to lose
			}
			return refs, fmt.Errorf("stat .git of %s: %w", subRel, err)
		}
		inner, err := weavePreserveNestedLevel(root, workspace, subRel, id)
		refs = append(refs, inner...)
		if err != nil {
			return refs, err
		}
		if st, err := gitOut(subDir, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none"); err != nil {
			return refs, fmt.Errorf("read status of %s: %w", subRel, err)
		} else if strings.TrimSpace(st) != "" {
			if out, err := exec.Command(gitBin(), "-C", subDir, "add", "-A").CombinedOutput(); err != nil {
				return refs, fmt.Errorf("stage %s: %w: %s", subRel, err, strings.TrimSpace(string(out)))
			}
			msg := fmt.Sprintf("weave: forced salvage of nested submodule work in %s (run #%d)", subRel, id)
			if out, err := exec.Command(gitBin(), "-C", subDir, "-c", "user.name=weave", "-c", "user.email=weave@localhost",
				"commit", "-m", msg).CombinedOutput(); err != nil {
				return refs, fmt.Errorf("commit %s: %w: %s", subRel, err, strings.TrimSpace(string(out)))
			}
		}
		head, err := gitOut(subDir, "rev-parse", "HEAD")
		if err != nil {
			if entries, _ := os.ReadDir(subDir); len(entries) > 1 {
				return refs, fmt.Errorf("submodule %s has files but no readable HEAD: %w", subRel, err)
			}
			continue // interrupted clone with no checkout
		}
		head = strings.TrimSpace(head)
		recorded, _ := gitOut(dir, "rev-parse", "HEAD:"+filepath.ToSlash(path))
		if head == strings.TrimSpace(recorded) {
			continue
		}
		base := "refs/salvage/abandoned-" + fmt.Sprint(id) + "-sub-" + weaveSalvageSlug(subRel)
		ref, err := weavePreserveTipAs(root, subDir, base, head)
		if err != nil {
			return refs, fmt.Errorf("preserve %s as %s: %w", subRel, ref, err)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func dirLabel(rel string) string {
	if rel == "" {
		return "workspace"
	}
	return rel
}

func weaveSalvageSlug(rel string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '-'
	}, filepath.ToSlash(rel))
}
