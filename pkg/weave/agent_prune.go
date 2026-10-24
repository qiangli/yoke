package weave

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/assetring"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/prune"
)

// AgentPruner archives aged local clones and ended sprint seats. Base bindings
// and lower-ring identities never become candidates.
type AgentPruner struct {
	Catalog *fleet.Catalog
	Now     func() time.Time
}

func (AgentPruner) Name() string { return "agent" }

func (p AgentPruner) catalog() *fleet.Catalog {
	if p.Catalog != nil {
		return p.Catalog
	}
	return fleetCatalog()
}

func (p AgentPruner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func agentPruneOwner(a fleet.Agent) (string, *weaveQueue, bool) {
	if a.Lifecycle == nil {
		board, err := sprintStoreDir()
		if err != nil {
			return "", nil, false
		}
		q, err := readWeaveQueue(board)
		return board, q, err == nil
	}
	dir := a.Lifecycle.WeaveQueue
	if dir == "" && a.Lifecycle.SprintID != "" {
		var err error
		dir, err = sprintStoreDir()
		if err != nil {
			return "", nil, false
		}
	}
	if dir == "" {
		return "", nil, false
	}
	q, err := readWeaveQueue(dir)
	return dir, q, err == nil
}

func (p AgentPruner) DryRun(ctx context.Context, opts prune.Options) ([]prune.Item, error) {
	cat := p.catalog()
	agents, errs := cat.Agents()
	if len(errs) > 0 {
		return nil, errs[0]
	}
	instances, err := fleet.NewInstanceStore(fleet.InstanceDir()).List()
	if err != nil {
		return nil, err
	}
	older := opts.Older
	if older == 0 {
		older = 30 * 24 * time.Hour
	}
	var items []prune.Item
	for _, a := range agents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if a.Ring != assetring.RingLocal || (a.ClonedFrom == "" && (a.Lifecycle == nil || !a.Ephemeral)) {
			continue
		}
		created, err := time.Parse(time.RFC3339, a.ClonedAt)
		if err != nil && a.Ephemeral && a.Lifecycle != nil {
			path := filepath.Join(fleet.NounDir(cat.Root(), "agents"), a.Name+".yaml")
			if st, statErr := os.Lstat(path); statErr == nil && st.Mode().IsRegular() {
				created, err = st.ModTime(), nil
			}
		}
		if err != nil || p.now().Sub(created) < older {
			continue // unknown creation time is not proof of old age
		}
		usedRecently := false
		for _, inst := range instances {
			if inst.Family != a.Name {
				continue
			}
			if inst.Retired == "" {
				usedRecently = true // active or parked context is retained
				break
			}
			retiredAt, err := time.Parse(time.RFC3339, inst.Retired)
			if err != nil || p.now().Sub(retiredAt) < older {
				usedRecently = true
				break
			}
		}
		if usedRecently {
			continue
		}
		dir, q, ok := agentPruneOwner(a)
		if !ok {
			continue
		}
		if a.Lifecycle != nil && !weaveAgentWorkEnded(a, dir, q) {
			continue
		}
		if err := weaveAgentIdle(a, dir, q); err != nil {
			continue
		}
		reason := "local clone unused for at least " + older.String()
		if a.Lifecycle != nil {
			reason = "owned seat's work ended; unused for at least " + older.String()
		}
		items = append(items, prune.Item{Noun: p.Name(), ID: a.Name, Reason: reason})
	}
	return items, nil
}

func (p AgentPruner) Apply(ctx context.Context, items []prune.Item) (prune.Counts, error) {
	var counts prune.Counts
	cat := p.catalog()
	for _, item := range items {
		if item.Noun != p.Name() {
			return counts, fmt.Errorf("agent prune: foreign item %q", item.ID)
		}
		current, err := p.DryRun(ctx, prune.Options{Older: time.Nanosecond})
		if err != nil {
			return counts, err
		}
		eligible := false
		for _, candidate := range current {
			if candidate.ID == item.ID {
				eligible = true
				break
			}
		}
		if !eligible {
			continue
		}
		a, ok := cat.Agent(item.ID)
		if !ok {
			continue
		}
		dir, _, ok := agentPruneOwner(a)
		if !ok {
			continue
		}
		queueLock, err := lockfile.TryAcquire(filepath.Join(dir, "queue.lock"), lockfile.Holder{
			Name: "agent-prune", PID: os.Getpid(), Intent: "guard agent archive",
		})
		if err != nil {
			return counts, err
		}
		err = weaveGuardAgentLifecycle(filepath.Clean(dir), a, func() error {
			_, err := cat.ArchiveAgent(a.Name, func(current fleet.Agent) error {
				if a.ClonedAt != current.ClonedAt || a.ClonedFrom != current.ClonedFrom || !reflect.DeepEqual(a.Lifecycle, current.Lifecycle) {
					return fmt.Errorf("agent ownership changed")
				}
				ownerDir, owner, ok := agentPruneOwner(current)
				if !ok || ownerDir != dir {
					return fmt.Errorf("agent owner is unavailable")
				}
				if current.Lifecycle != nil && !weaveAgentWorkEnded(current, ownerDir, owner) {
					return fmt.Errorf("agent work is active")
				}
				return weaveAgentIdle(current, ownerDir, owner)
			})
			return err
		})
		releaseErr := queueLock.Release()
		if err != nil {
			return counts, err
		}
		if releaseErr != nil {
			return counts, releaseErr
		}
		counts.Removed++
	}
	return counts, nil
}
