package weave

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// resolveSprintRunQueue binds a human repo name + run id to one physical
// queue. Historical same-named checkouts are harmless when only one contains
// the requested run. If more than one does, callers must provide the opaque
// queue tag; guessing would link (and later stop) the wrong worker.
func resolveSprintRunQueue(repo string, id int64, queueTag string) (string, error) {
	repo = strings.TrimSpace(repo)
	queueTag = strings.TrimSpace(queueTag)
	if repo == "" {
		return "", fmt.Errorf("repo is required")
	}
	if queueTag != "" && (filepath.Base(queueTag) != queueTag || queueTag == "." || queueTag == "..") {
		return "", fmt.Errorf("queue must be an opaque queue tag, not a path: %q", queueTag)
	}

	var candidates []string
	seen := map[string]bool{}
	for _, dir := range weaveAllQueueDirs() {
		dir = filepath.Clean(dir)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		tag := filepath.Base(dir)
		if queueTag != "" {
			if tag == queueTag {
				candidates = append(candidates, dir)
			}
			continue
		}
		if i := strings.LastIndex(tag, "-"); i > 0 && tag[:i] == repo {
			candidates = append(candidates, dir)
		}
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		if queueTag != "" {
			return "", fmt.Errorf("queue %q for repo %q does not exist", queueTag, repo)
		}
		return "", fmt.Errorf("no weave queue for repo %q", repo)
	}
	if queueTag != "" && len(candidates) != 1 {
		return "", fmt.Errorf("queue tag %q resolves to %d queue directories; refusing to guess", queueTag, len(candidates))
	}

	var containing []string
	for _, dir := range candidates {
		q, err := loadWeaveQueue(dir)
		if err != nil {
			return "", fmt.Errorf("load candidate queue %q for repo %q: %w", filepath.Base(dir), repo, err)
		}
		if q.Root != "" && filepath.Base(filepath.Clean(q.Root)) != repo {
			if queueTag != "" {
				return "", fmt.Errorf("queue %q belongs to repo %q, not %q", queueTag, filepath.Base(filepath.Clean(q.Root)), repo)
			}
			continue
		}
		if findWeaveItem(q, id) != nil {
			containing = append(containing, dir)
		}
	}
	if queueTag != "" {
		if len(containing) == 0 {
			return candidates[0], nil // validator reports the precise missing run
		}
		return containing[0], nil
	}
	switch len(containing) {
	case 1:
		return containing[0], nil
	case 0:
		if len(candidates) == 1 {
			return candidates[0], nil // validator reports the precise missing run
		}
		return "", fmt.Errorf("repo %q has %d queues, but none contains weave run #%d", repo, len(candidates), id)
	default:
		tags := make([]string, len(containing))
		for i, dir := range containing {
			tags[i] = filepath.Base(dir)
		}
		return "", fmt.Errorf("repo %q run #%d exists in multiple queues (%s); pass --queue <tag>", repo, id, strings.Join(tags, ", "))
	}
}

// weaveQueueDirForSprintRun resolves the stable identity written by new
// links. Legacy records without Queue retain a fail-closed run-aware fallback.
func weaveQueueDirForSprintRun(run sprintRun) (string, error) {
	return resolveSprintRunQueue(run.Repo, run.ID, run.Queue)
}

func sameSprintRun(a, b sprintRun) (bool, error) {
	// Issue numbers are queue-local. Historical sprint records commonly use
	// the same small numbers in unrelated repositories, so repository identity
	// must be rejected before attempting legacy queue resolution. Otherwise a
	// missing old queue for repo A can block linking a new repo B run merely
	// because both happened to be issue #3.
	if a.Repo != b.Repo || a.ID != b.ID {
		return false, nil
	}
	// Ids are QUEUE-LOCAL AND RECYCLED — prune a queue and the next `weave add`
	// reuses the freed numbers. So matching (repo, queue, id) proves only that
	// two records name the same SLOT, never the same unit of work. Born is the
	// generation: when both records carry one and they differ, this is a reused
	// id and the older link must not block the newer.
	if !a.Born.IsZero() && !b.Born.IsZero() && !a.Born.Equal(b.Born) {
		return false, nil
	}
	if a.Queue != "" && b.Queue != "" {
		return a.Queue == b.Queue, nil
	}
	// Resolve legacy records instead of treating every same basename as the
	// same checkout. Ambiguity blocks the mutation: it may be a duplicate.
	ad, err := weaveQueueDirForSprintRun(a)
	if err != nil {
		return false, err
	}
	bd, err := weaveQueueDirForSprintRun(b)
	if err != nil {
		return false, err
	}
	return filepath.Base(ad) == filepath.Base(bd), nil
}

func sprintRunKey(run sprintRun) string {
	if run.Queue != "" {
		return "queue:" + run.Queue
	}
	// Legacy records cannot safely distinguish same-named checkouts without
	// resolving the run. Conservatively group by repo for sharing checks.
	return "legacy-repo:" + run.Repo
}
