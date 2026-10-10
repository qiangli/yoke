package weave

import (
	"fmt"

	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// sprintInvalidArg marks a sprint mutation refused for its arguments, so
// runWeaveStoryMutate reports ExitInvalidArg instead of a generic failure.
type sprintInvalidArg struct{ error }

func (e sprintInvalidArg) Unwrap() error { return e.error }

func sprintInvalid(format string, args ...any) error {
	return sprintInvalidArg{fmt.Errorf(format, args...)}
}

// sprintLinkStory is `sprint link --story`: it links the run AND registers it
// to one story of this sprint, the explicit join `sprint accept` rates from.
//
// A worker that claims and submits a story itself never passes through
// `sprint assign`, so nothing else records which story a hand-linked run is
// for — and a run in another repo can never satisfy the same-root legacy join.
// Every refusal happens before any write: the run's queue is only touched once
// the story, the sprint's other links, and the other sprints' claims all
// agree, and the board is saved only if that queue write succeeds.
func sprintLinkStory(s *weaveStory, board []*weaveStory, linked sprintRun, ref string) (string, error) {
	_, story, err := resolveSprintStoryFor(s, "", ref)
	if err != nil {
		return "", sprintInvalidArg{err}
	}
	if todopkg.IsClosed(story.Status) {
		return "", sprintInvalid("story %s is already closed", story.ID)
	}
	label := fmt.Sprintf("%s#%d", linked.Repo, linked.ID)
	existing := -1
	for _, other := range board {
		if other.ID != s.ID && other.Column == "done" {
			continue
		}
		for i, r := range other.Runs {
			same, err := sameSprintRun(r, linked)
			if err != nil {
				return "", fmt.Errorf("cannot prove %s is not already linked: %w", label, err)
			}
			if !same {
				continue
			}
			if other.ID != s.ID {
				return "", sprintInvalid("%s is already linked to sprint #%d", label, other.ID)
			}
			existing = i
		}
	}
	if existing >= 0 && s.Runs[existing].Story != "" && s.Runs[existing].Story != story.ID {
		return "", sprintInvalid("%s is linked for story %s; unlink it before linking story %s", label, s.Runs[existing].Story, story.ID)
	}
	// Two live attempts at one story make the rating a guess: refuse, and let
	// the manager unlink one. A run that is gone or finished cannot be worked
	// again, so it does not block a new attempt.
	for i, r := range s.Runs {
		if i == existing {
			continue
		}
		run := sprintLinkedItem(r)
		if run == nil || isTerminalState(run.State) {
			continue
		}
		if r.Story == story.ID || run.Register == story.ID {
			return "", sprintInvalid("story %s already has live linked run %s#%d; unlink one attempt first", story.ID, r.Repo, r.ID)
		}
	}
	dir, err := weaveQueueDirForSprintRun(linked)
	if err != nil {
		return "", err
	}
	already := false
	err = withWeaveQueueLock(dir, func(q *weaveQueue) error {
		run := findWeaveItem(q, linked.ID)
		if run == nil || (!linked.Born.IsZero() && !linked.Born.Equal(run.Created)) {
			return sprintInvalid("%s changed while linking; retry", label)
		}
		if run.Register != "" && run.Register != story.ID {
			return sprintInvalid("%s is registered to story %s, not %s", label, run.Register, story.ID)
		}
		already = run.Register == story.ID
		run.Register = story.ID
		return nil
	})
	if err != nil {
		return "", err
	}
	short := shortSprintStoryID(story.ID)
	if existing >= 0 {
		if already && s.Runs[existing].Story == story.ID {
			return fmt.Sprintf("sprint #%d already links %s for story %s", s.ID, label, short), nil
		}
		s.Runs[existing].Story = story.ID
		return fmt.Sprintf("sprint #%d linked %s to story %s", s.ID, label, short), nil
	}
	linked.Story = story.ID
	s.Runs = append(s.Runs, linked)
	return fmt.Sprintf("sprint #%d linked %s for story %s", s.ID, label, short), nil
}

// sprintLinkedItem loads the run a link names, or nil when its queue or the
// same generation of the run is gone.
func sprintLinkedItem(link sprintRun) *weaveItem {
	dir, err := weaveQueueDirForSprintRun(link)
	if err != nil {
		return nil
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return nil
	}
	run := findWeaveItem(q, link.ID)
	if run == nil || (!link.Born.IsZero() && !link.Born.Equal(run.Created)) {
		return nil
	}
	return run
}

// sprintUnlinkStory releases the registration `link --story` wrote, so the
// run can be linked for another story. Best effort: a run that is gone has
// nothing to release, and a failure is reported rather than undoing the unlink.
func sprintUnlinkStory(removed sprintRun) string {
	if removed.Story == "" {
		return ""
	}
	dir, err := weaveQueueDirForSprintRun(removed)
	if err != nil {
		return ""
	}
	released := false
	err = withWeaveQueueLock(dir, func(q *weaveQueue) error {
		run := findWeaveItem(q, removed.ID)
		if run == nil || (!removed.Born.IsZero() && !removed.Born.Equal(run.Created)) || run.Register != removed.Story {
			return nil
		}
		run.Register = ""
		released = true
		return nil
	})
	if err != nil {
		return fmt.Sprintf("run registration to story %s not released: %v", shortSprintStoryID(removed.Story), err)
	}
	if released {
		return fmt.Sprintf("released its registration to story %s", shortSprintStoryID(removed.Story))
	}
	return ""
}
