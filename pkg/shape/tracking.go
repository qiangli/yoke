package shape

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/shape/evidence"
	"github.com/qiangli/yoke/pkg/src"
	"github.com/qiangli/yoke/pkg/todo"
)

// Tracker writes shaping origin into the ordinary todo/story store supplied by
// the caller. It never chooses a sprint or creates a separate shaping backlog.
type Tracker struct{ Store *issue.Store }

// Record creates or updates reciprocal tracking for a candidate. An itemRef
// selects an existing todo/story; an empty ref creates an ordinary todo. Repeated
// calls for the same app/candidate recover its existing item, including a prior
// save whose response could not be persisted to the candidate record.
func (t Tracker) Record(ctx context.Context, change issue.ShapeChange, request, itemRef string, story bool) (string, error) {
	if t.Store == nil {
		return "", errors.New("shape tracking: no todo store configured")
	}
	if strings.TrimSpace(change.App) == "" || strings.TrimSpace(change.Candidate) == "" || strings.TrimSpace(change.Actor) == "" || strings.TrimSpace(change.RepresentedUser) == "" {
		return "", errors.New("shape tracking: app, candidate, actor and represented user are required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	lock, err := lockfile.AcquireWithin(filepath.Join(t.Store.Dir(), ".shape-tracking.lock"), 5*time.Second, lockfile.Holder{Name: change.Actor, Intent: "record shaping origin"})
	if err != nil {
		return "", fmt.Errorf("shape tracking: %w", err)
	}
	defer lock.Release()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var item *issue.Issue
	if itemRef != "" {
		ref := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(itemRef), "todo:"), "story:")
		item, err = todo.ResolveRef(t.Store, ref)
		if err != nil {
			return "", fmt.Errorf("shape tracking: linked item: %w", err)
		}
		if story && item.Sprint == 0 && item.SprintID == "" {
			return "", errors.New("shape tracking: --story requires a sprint story")
		}
	} else {
		if story {
			return "", errors.New("shape tracking: --story requires an item reference")
		}
		items, listErr := t.Store.List()
		if listErr != nil {
			return "", fmt.Errorf("shape tracking: %w", listErr)
		}
		for _, existing := range items {
			for _, previous := range existing.Shape {
				if previous.App == change.App && previous.Candidate == change.Candidate {
					item = existing
					break
				}
			}
			if item != nil {
				break
			}
		}
		if item == nil {
			seq, seqErr := todo.MaxSeq(t.Store)
			if seqErr != nil {
				return "", seqErr
			}
			title := strings.Join(strings.Fields(request), " ")
			if title == "" {
				title = "Change " + change.App
			}
			if text := []rune(title); len(text) > 120 {
				title = string(text[:120])
			}
			item = &issue.Issue{ID: issue.NewID(), Kind: issue.KindTask, Title: title, Status: todo.StatusTodo, Seq: seq + 1, Created: time.Now().UTC()}
		}
	}
	if item.Origin != "" && item.Origin != "shape" {
		return "", fmt.Errorf("shape tracking: linked item already has origin %q", item.Origin)
	}
	item.Origin = "shape"
	issue.AddLabels(item, []string{"shaping"})
	found := false
	for i, previous := range item.Shape {
		if previous.App == change.App && previous.Candidate == change.Candidate {
			if previous.Actor != change.Actor || previous.RepresentedUser != change.RepresentedUser {
				return "", errors.New("shape tracking: candidate caller identity changed")
			}
			if change.Version == "" {
				change.Version = previous.Version
			}
			if change.Evidence == "" {
				change.Evidence = previous.Evidence
			}
			if change.Evidence != "" {
				var err error
				change.Evidence, err = ShapeEvidenceReference(change.Version)
				if err != nil {
					return "", err
				}
			}
			item.Shape[i] = change
			found = true
			break
		}
	}
	if !found {
		if change.Evidence != "" {
			var err error
			change.Evidence, err = ShapeEvidenceReference(change.Version)
			if err != nil {
				return "", err
			}
		}
		item.Shape = append(item.Shape, change)
	}
	if _, err = t.Store.Save(item); err != nil {
		return "", fmt.Errorf("shape tracking: save: %w", err)
	}
	return "todo:" + item.ID, nil
}

// NewTracking supplies the production lifecycle callback. A resolved store path
// travels with the candidate so later invocations do not select another store
// merely because the process working directory changed.
func NewTracking(home string) func(context.Context, *Candidate) error {
	return func(ctx context.Context, c *Candidate) error {
		if c == nil {
			return errors.New("shape tracking: nil candidate")
		}
		if c.Story != "" && c.Todo != "" {
			return errors.New("shape tracking: --story and --todo are mutually exclusive")
		}
		var store *issue.Store
		if c.TrackingRoot != "" {
			root, err := filepath.Abs(c.TrackingRoot)
			if err != nil {
				return err
			}
			store = &issue.Store{Root: root, Sub: "."}
		} else if root, ok := todo.FindGitRoot(); ok && (c.Story != "" || c.Todo != "") {
			store = todo.RepoStore(root)
		} else if home != "" && strings.TrimSpace(os.Getenv("BASHY_TODO_DIR")) == "" {
			store = &issue.Store{Root: filepath.Join(home, "todo"), Sub: todo.SanitizeOwner(c.Actor)}
		} else {
			var err error
			store, err = todo.UserStore(c.Actor)
			if err != nil {
				return err
			}
		}
		ref := c.TrackingItem
		if ref == "" {
			ref = c.Story
			if ref == "" {
				ref = c.Todo
			}
		}
		evidence := ""
		if c.Verdict != nil {
			evidence = c.Verdict.Evidence
		}
		got, err := (Tracker{Store: store}).Record(ctx, issue.ShapeChange{App: c.App, Candidate: c.ID, Version: c.Rev, Actor: c.Actor, RepresentedUser: c.RepresentedUser, Evidence: evidence}, c.Request, ref, c.Story != "")
		if err != nil {
			return err
		}
		root, err := filepath.Abs(store.Dir())
		if err != nil {
			return err
		}
		c.TrackingItem, c.TrackingRoot = got, root
		return nil
	}
}

// TrackCandidate uses the ordinary default todo-store resolution.
func TrackCandidate(ctx context.Context, c *Candidate) error { return NewTracking("")(ctx, c) }

// ShapeEvidenceReference is a public-safe locator for a revision's host-local
// verifier record. Store paths and user-specific directories are never encoded.
func ShapeEvidenceReference(revision string) (string, error) {
	if revision == "" || revision == "." || revision == ".." || strings.ContainsAny(revision, "/\\: \t\r\n") {
		return "", errors.New("shape tracking: evidence needs a revision identity")
	}
	return "shape-evidence:" + revision, nil
}

// ResolveShapeEvidence resolves a stable locator against a selected local home.
// Resolving a location is not verification; readers must still check its seal.
func ResolveShapeEvidence(home, reference string) (string, error) {
	const prefix = "shape-evidence:"
	if !strings.HasPrefix(reference, prefix) {
		return "", errors.New("shape tracking: unsupported evidence reference")
	}
	revision := strings.TrimPrefix(reference, prefix)
	if _, err := ShapeEvidenceReference(revision); err != nil {
		return "", err
	}
	if home == "" {
		home = src.Home()
	}
	return evidence.Path(home, revision), nil
}
