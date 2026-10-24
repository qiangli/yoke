package binmgr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/prune"
)

// SelfPruner reclaims superseded, versioned bashy and managed-tool caches.
// It keeps the two newest versions of every component and the running binary.
type SelfPruner struct {
	Home     string
	Cache    string
	ExecPath string // injectable for tests; empty uses os.Executable
}

func (SelfPruner) Name() string { return "self" }

func (p SelfPruner) roots() ([]string, error) {
	home := p.Home
	if home == "" {
		home = prune.Home()
	}
	cache := p.Cache
	if cache == "" {
		var err error
		cache, err = CacheDir()
		if err != nil {
			return nil, err
		}
	}
	return []string{filepath.Join(home, "versions"), cache}, nil
}

func (p SelfPruner) DryRun(ctx context.Context, opts prune.Options) ([]prune.Item, error) {
	roots, err := p.roots()
	if err != nil {
		return nil, err
	}
	exe := p.ExecPath
	if exe == "" {
		exe, _ = os.Executable()
	}
	exe, _ = filepath.EvalSymlinks(exe)
	var items []prune.Item
	for _, root := range roots {
		rootInfo, rootErr := os.Lstat(root)
		if os.IsNotExist(rootErr) {
			continue
		}
		if rootErr != nil {
			return nil, rootErr
		}
		if !rootInfo.IsDir() { // a redirected cache is not owned by this root
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		for _, component := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !component.IsDir() {
				continue
			}
			componentDir := filepath.Join(root, component.Name())
			pinned := map[string]bool{}
			prefix := component.Name() + "@"
			suffix := "/" + Platform()
			for key := range pinnedDigests {
				if strings.HasPrefix(key, prefix) && strings.HasSuffix(key, suffix) {
					pinned[strings.TrimSuffix(strings.TrimPrefix(key, prefix), suffix)] = true
				}
			}
			versions, err := os.ReadDir(componentDir)
			if err != nil {
				return nil, err
			}
			type version struct {
				name string
				mod  time.Time
			}
			var dirs []version
			for _, v := range versions {
				if !v.IsDir() {
					continue
				}
				st, err := os.Lstat(filepath.Join(componentDir, v.Name()))
				if err != nil || !st.IsDir() {
					continue
				}
				dirs = append(dirs, version{v.Name(), st.ModTime()})
			}
			sort.Slice(dirs, func(i, j int) bool {
				if dirs[i].mod.Equal(dirs[j].mod) {
					return dirs[i].name > dirs[j].name
				}
				return dirs[i].mod.After(dirs[j].mod)
			})
			for i, v := range dirs {
				if i < 2 || pinned[v.name] {
					continue
				}
				path := filepath.Join(componentDir, v.name)
				if exe != "" && (exe == path || strings.HasPrefix(exe, path+string(os.PathSeparator))) {
					continue
				}
				if opts.Older > 0 && time.Since(v.mod) < opts.Older {
					continue
				}
				size, err := prune.ApparentSize(path)
				if err != nil {
					return nil, err
				}
				items = append(items, prune.Item{Noun: p.Name(), ID: path, Reason: "older version; current and previous kept", Size: size})
			}
		}
	}
	return items, nil
}

func (p SelfPruner) Apply(ctx context.Context, items []prune.Item) (prune.Counts, error) {
	var counts prune.Counts
	for _, item := range items {
		if item.Noun != p.Name() {
			return counts, fmt.Errorf("self prune: foreign item %q", item.ID)
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
		if err := os.RemoveAll(item.ID); err != nil {
			return counts, err
		}
		counts.Removed++
		counts.Bytes += item.Size
	}
	return counts, nil
}
