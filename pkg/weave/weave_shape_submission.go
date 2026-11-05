package weave

import (
	"fmt"
	"github.com/qiangli/coreutils/pkg/lockfile"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/issue"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// ShapeRun is an ordinary, unstarted weave run linked to a tracked item.
type ShapeRun struct {
	ID    int64  `json:"id"`
	Repo  string `json:"repo"`
	State string `json:"state"`
}

// LinkShapeSubmission reuses the ordinary register/queue bridge. It records
// references and a review brief only; it never starts an agent or merges code.
// A retry recovers the existing queue link even if the item's save failed.
func LinkShapeSubmission(root string, store *issue.Store, itemID, key, brief string) (ShapeRun, error) {
	if store == nil || key == "" {
		return ShapeRun{}, fmt.Errorf("weave: shaping submission needs item store and identity")
	}
	root, err := weaveRepoRoot(root)
	if err != nil {
		return ShapeRun{}, err
	}
	lock, err := lockfile.AcquireWithin(filepath.Join(store.Dir(), ".shape-tracking.lock"), 5*time.Second, lockfile.Holder{Intent: "link shaping submission to ordinary run"})
	if err != nil {
		return ShapeRun{}, err
	}
	defer lock.Release()
	item, err := store.Resolve(itemID)
	if err != nil {
		return ShapeRun{}, err
	}
	if todopkg.IsClosed(item.Status) {
		return ShapeRun{}, fmt.Errorf("weave: linked item is closed")
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		return ShapeRun{}, err
	}
	marker := "Shape-Submission: " + key
	var run *weaveItem
	err = withWeaveQueueLock(dir, func(q *weaveQueue) error {
		if q.Root != "" && weaveCanonicalRepoRoot(q.Root) != weaveCanonicalRepoRoot(root) {
			return fmt.Errorf("weave: queue belongs to another repository")
		}
		q.Root = root
		for _, candidate := range q.Items {
			if candidate.Register == item.ID && (candidate.ID == item.Weave || strings.Contains(candidate.Body, marker)) {
				run = candidate
				break
			}
		}
		if item.Weave != 0 && run == nil {
			return fmt.Errorf("weave: linked item run %d is unavailable in this repository", item.Weave)
		}
		if run == nil {
			priority := item.Priority
			if priority == "" {
				priority = "p2"
			}
			run = &weaveItem{ID: q.NextID, Title: item.Title, Priority: priority, Stage: item.Stage, Register: item.ID, State: "todo", Created: timeNowUTC()}
			q.NextID++
			q.Items = append(q.Items, run)
		}
		if !strings.Contains(run.Body, marker) {
			switch run.State {
			case "merged", "done", "abandoned":
				return fmt.Errorf("weave: linked run %d is already %s", run.ID, run.State)
			}
			run.Body = strings.TrimSpace(run.Body) + "\n\n" + marker + "\n" + brief
		}
		return nil
	})
	if err != nil {
		return ShapeRun{}, err
	}
	item.Weave = run.ID
	if item.Status == todopkg.StatusTodo || item.Status == todopkg.StatusBlocked || item.Status == issue.StatusOpen {
		item.Status = todopkg.StatusAssigned
	}
	if _, err = store.Save(item); err != nil {
		return ShapeRun{}, err
	}
	return ShapeRun{ID: run.ID, Repo: root, State: run.State}, nil
}
