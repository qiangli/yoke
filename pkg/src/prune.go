package src

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/prune"
)

// Pruner only considers derived builds under src/.builds/<repo>/<commit>.
// Branches, PRs, and checkouts are outside this path and cannot be candidates.
type Pruner struct{ Home string }

func (Pruner) Name() string { return "src" }

var buildCommitName = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

func (p Pruner) home() string {
	if p.Home != "" {
		return p.Home
	}
	return Home()
}

func (p Pruner) DryRun(ctx context.Context, opts prune.Options) ([]prune.Item, error) {
	srcDir := SrcDir(p.home())
	if st, err := os.Lstat(srcDir); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	} else if !st.IsDir() {
		return nil, nil
	}
	root := filepath.Join(srcDir, ".builds")
	if st, err := os.Lstat(root); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	} else if !st.IsDir() {
		return nil, nil
	}
	var items []prune.Item
	for _, repo := range AllowedRepos() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		check := filepath.Join(srcDir, repo)
		if st, err := os.Lstat(check); err != nil || !st.IsDir() {
			continue
		}
		tip, err := ygit.ResolveCommit(check, localBranch)
		if err != nil {
			continue // no local tip means there is no safe age comparison
		}
		dir := filepath.Join(root, repo)
		if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() || !buildCommitName.MatchString(e.Name()) || strings.EqualFold(e.Name(), tip) {
				continue
			}
			path := filepath.Join(dir, e.Name())
			st, err := os.Lstat(path)
			if err != nil || !st.IsDir() {
				continue
			}
			if opts.Older > 0 && time.Since(st.ModTime()) < opts.Older {
				continue
			}
			ancestor, err := ygit.IsAncestor(check, e.Name(), tip)
			if err != nil || !ancestor {
				continue
			}
			size, err := prune.ApparentSize(path)
			if err != nil {
				return nil, err
			}
			items = append(items, prune.Item{Noun: p.Name(), ID: repo + "/" + e.Name(), Reason: "build commit is behind local tip " + tip, Size: size})
		}
	}
	return items, nil
}

func (p Pruner) Apply(ctx context.Context, items []prune.Item) (prune.Counts, error) {
	var counts prune.Counts
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return counts, err
		}
		if item.Noun != p.Name() {
			return counts, fmt.Errorf("src prune: foreign item %q", item.ID)
		}
		current, err := p.DryRun(ctx, prune.Options{})
		if err != nil {
			return counts, err
		}
		eligible := false
		for _, candidate := range current {
			if candidate.ID == item.ID {
				eligible = true
				item.Size = candidate.Size
				break
			}
		}
		if !eligible {
			continue
		}
		path := filepath.Join(SrcDir(p.home()), ".builds", filepath.FromSlash(item.ID))
		if err := os.RemoveAll(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return counts, err
		}
		counts.Removed++
		counts.Bytes += item.Size
	}
	return counts, nil
}
