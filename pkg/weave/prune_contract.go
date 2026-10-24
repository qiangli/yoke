package weave

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/prune"
	"github.com/spf13/cobra"
)

// WeavePruner uses the same queue and settlement proofs as weave prune.
// It is scoped to the current repository, as the existing command is.
type WeavePruner struct{ Stale bool }

func (WeavePruner) Name() string { return "weave" }

func (p WeavePruner) DryRun(ctx context.Context, opt prune.Options) ([]prune.Item, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return nil, err
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		return nil, err
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return nil, err
	}
	base := weaveBaseBranch(root)
	weaveReconcileMerged(root, base, q) // in-memory only
	var items []prune.Item
	for _, it := range q.Items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !weavePrunableForSweep(it.State, p.Stale) {
			continue
		}
		if opt.Older > 0 && (it.Created.IsZero() || time.Since(it.Created) < opt.Older) {
			continue
		}
		held := false
		if it.Workspace != "" {
			if st, statErr := os.Lstat(it.Workspace); statErr == nil && st.IsDir() && weaveContainedArtifact(dir, it.Workspace) == nil {
				status, statusErr := exec.Command(gitBin(), "-C", it.Workspace, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none").Output()
				ahead := 0
				if !weaveItemMerged(root, base, it) {
					ahead, _ = weaveUnmergedAhead(root, base, it)
				}
				if statusErr == nil && len(status) == 0 && ahead == 0 && len(weaveUnsettledSideRefs(root, base, it)) == 0 {
					size, sizeErr := prune.ApparentSize(it.Workspace)
					if sizeErr != nil {
						return nil, sizeErr
					}
					items = append(items, prune.Item{Noun: p.Name(), ID: "workspace:" + it.Workspace,
						Reason: "terminal run #" + fmt.Sprint(it.ID) + " has a clean workspace", Size: size})
				} else {
					held = true
				}
			} else if !os.IsNotExist(statErr) {
				held = true
			}
		}
		if held {
			continue
		}
		if it.Branch != "" && gitBranchIntegrated(root, base, it.Branch) {
			if _, exists := gitBranchTip(root, it.Branch); exists {
				items = append(items, prune.Item{Noun: p.Name(), ID: "branch:" + it.Branch,
					Reason: "terminal run #" + fmt.Sprint(it.ID) + " branch is integrated"})
			}
		}
		for _, a := range weavePlanOwnedRun(dir, filepath.Base(root), it) {
			if a.Err != "" {
				continue
			}
			items = append(items, prune.Item{Noun: p.Name(), ID: a.Kind + ":" + a.Target,
				Reason: "terminal run #" + fmt.Sprint(it.ID) + " (" + it.State + ") owns redundant " + a.Kind,
				Size:   int64(a.ExpectedBytes)})
		}
	}
	orphans, err := weaveOrphanWorkspaceTargets(dir, q)
	if err != nil {
		return nil, err
	}
	for _, o := range orphans {
		if o.Hold != "" || !oldEnough(o.Path, opt.Older) {
			continue
		}
		size, err := prune.ApparentSize(o.Path)
		if err != nil {
			return nil, err
		}
		items = append(items, prune.Item{Noun: p.Name(), ID: "workspace:" + o.Path,
			Reason: "unclaimed clean workspace", Size: size})
	}
	caches, err := weaveManagedGOCacheSweepTargets(dir, q, p.Stale)
	if err != nil {
		return nil, err
	}
	for _, cache := range caches {
		if !oldEnough(cache.Path, opt.Older) {
			continue
		}
		size, err := prune.ApparentSize(cache.Path)
		if err != nil {
			return nil, err
		}
		items = append(items, prune.Item{Noun: p.Name(), ID: "cache:" + cache.Path,
			Reason: "managed GOCACHE for terminal or missing run", Size: size})
	}
	seen := map[string]bool{}
	unique := items[:0]
	for _, item := range items {
		if !seen[item.ID] {
			seen[item.ID] = true
			unique = append(unique, item)
		}
	}
	return unique, nil
}

func oldEnough(path string, older time.Duration) bool {
	if older == 0 {
		return true
	}
	st, err := os.Lstat(path)
	return err == nil && time.Since(st.ModTime()) >= older
}

func (p WeavePruner) Apply(ctx context.Context, items []prune.Item) (prune.Counts, error) {
	var counts prune.Counts
	if len(items) == 0 {
		return counts, nil
	}
	current, err := p.DryRun(ctx, prune.Options{})
	if err != nil {
		return counts, err
	}
	allowed := make(map[string]bool, len(items))
	for _, item := range items {
		if item.Noun != p.Name() {
			return counts, fmt.Errorf("weave prune: foreign item %q", item.ID)
		}
		allowed[item.ID] = true
	}
	if len(current) != len(items) || len(allowed) != len(items) {
		return counts, fmt.Errorf("weave prune: candidate set changed; run dry-run again")
	}
	for _, item := range current {
		if !allowed[item.ID] {
			return counts, fmt.Errorf("weave prune: candidate set changed; run dry-run again")
		}
	}
	cmd := &cobra.Command{Use: "prune"}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetContext(ctx)
	flags := &weaveOutputFlags{}
	flags.attach(cmd)
	if err := runWeavePrune(cmd, true, p.Stale, false, flags, 0); err != nil {
		return counts, err
	}
	cwd, _ := os.Getwd()
	root, _ := weaveRepoRoot(cwd)
	for _, item := range items {
		if strings.HasPrefix(item.ID, "branch:") {
			if _, exists := gitBranchTip(root, strings.TrimPrefix(item.ID, "branch:")); !exists {
				counts.Removed++
			}
			continue
		}
		path := item.ID[strings.IndexByte(item.ID, ':')+1:]
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			counts.Removed++
			counts.Bytes += item.Size
		}
	}
	return counts, nil
}
