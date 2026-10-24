package weave

import (
	"context"
	"fmt"

	"github.com/qiangli/yoke/pkg/prune"
)

// SprintPruner reports redundant artifacts owned by linked runs in open
// sprints. The existing sprint prune command additionally reports hygiene.
type SprintPruner struct{}

func (SprintPruner) Name() string { return "sprint" }

func (p SprintPruner) DryRun(ctx context.Context, opt prune.Options) ([]prune.Item, error) {
	dir, err := sprintStoreDir()
	if err != nil {
		return nil, err
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return nil, err
	}
	var items []prune.Item
	for _, s := range q.Stories {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !sprintColumnOpen(s.Column) {
			continue
		}
		for _, a := range sprintPlanRunArtifacts(s) {
			if a.Err != "" {
				continue
			}
			if opt.Older > 0 && a.Kind != "branch" && !oldEnough(a.Target, opt.Older) {
				continue
			}
			items = append(items, prune.Item{Noun: p.Name(),
				ID:     fmt.Sprintf("%d:%s:%s", s.ID, a.Kind, a.Target),
				Reason: fmt.Sprintf("sprint #%d owns redundant %s", s.ID, a.Kind),
				Size:   int64(a.ExpectedBytes)})
		}
	}
	return items, nil
}

func (p SprintPruner) Apply(ctx context.Context, items []prune.Item) (prune.Counts, error) {
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
			return counts, fmt.Errorf("sprint prune: foreign item %q", item.ID)
		}
		allowed[item.ID] = true
	}
	if len(current) != len(items) || len(allowed) != len(items) {
		return counts, fmt.Errorf("sprint prune: candidate set changed; run dry-run again")
	}
	for _, item := range current {
		if !allowed[item.ID] {
			return counts, fmt.Errorf("sprint prune: candidate set changed; run dry-run again")
		}
	}
	dir, err := sprintStoreDir()
	if err != nil {
		return counts, err
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return counts, err
	}
	for _, s := range q.Stories {
		if !sprintColumnOpen(s.Column) {
			continue
		}
		for _, a := range sprintPruneRunArtifacts(s) {
			if a.Done && a.Err == "" {
				counts.Removed++
				counts.Bytes += int64(a.ActualBytes)
			}
		}
	}
	return counts, nil
}
