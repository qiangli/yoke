package coord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/qiangli/yoke/pkg/prune"
)

// ClaimPruner preserves claim's existing lapsed, unattached eligibility rule.
type ClaimPruner struct {
	Dir string
	Now func() time.Time
}

func (ClaimPruner) Name() string { return "claim" }

func (p ClaimPruner) dir() string {
	if p.Dir != "" {
		return p.Dir
	}
	return DefaultDir()
}

func (p ClaimPruner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p ClaimPruner) DryRun(ctx context.Context, opt prune.Options) ([]prune.Item, error) {
	now := p.now()
	claims, err := Lapsed(p.dir(), now)
	if err != nil {
		return nil, err
	}
	var items []prune.Item
	for _, c := range claims {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if opt.Older > 0 && now.Sub(c.Heartbeat) < opt.Older {
			continue
		}
		items = append(items, prune.Item{
			Noun: "claim", ID: c.key(),
			Reason: fmt.Sprintf("%s hold by %s lapsed; last heartbeat %s ago", c.Mode, c.Holder.Name, now.Sub(c.Heartbeat).Round(time.Second)),
		})
	}
	return items, nil
}

func (p ClaimPruner) Apply(ctx context.Context, items []prune.Item) (prune.Counts, error) {
	var counts prune.Counts
	dir := p.dir()
	keys := make(map[string]bool, len(items))
	for _, item := range items {
		if item.Noun != "claim" {
			return counts, fmt.Errorf("claim prune: foreign item %q", item.ID)
		}
		keys[item.ID] = true
	}
	b := fileBackend{dir: dir}
	err := withDirLock(dir, func() error {
		lapsed, err := Lapsed(dir, p.now())
		if err != nil {
			return err
		}
		for _, c := range lapsed {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !keys[c.key()] {
				continue
			}
			if err := b.commitLocked(c.key(), c.Rev, nil); err != nil {
				if errors.Is(err, ErrEpochMismatch) {
					continue
				}
				return err
			}
			counts.Removed++
		}
		return nil
	})
	return counts, err
}
