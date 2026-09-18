package weave

// THE PLAN MUST KEEP DESCRIBING THE SPRINT.
//
// A sprint's Goal list is its master plan: curated items, each linking the
// stories that satisfy it, with completion DERIVED from story closure and
// evidence rather than hand-ticked. It deliberately covers only the prioritized
// work, not every story — that part is right and stays.
//
// What was missing is the reverse direction. `sprintGoalDangling` catches a
// goal that points at a story which no longer exists. Nothing caught a STORY
// that no goal covers. So the flow was:
//
//	new bug filed with --sprint N  ->  enters the derived index (sprint next sees it)
//	                               ->  the plan does not mention it
//	                               ->  nothing says so
//
// and the plan silently stopped describing the sprint at exactly the moment it
// mattered — when unplanned work arrived.
//
// Worse, the completion gate was on the PLAN rather than the WORK:
// `sprint move <id> done` checked only sprintUncheckedGoals, which iterates
// s.Goal alone. A story filed into a sprint and never linked was invisible to
// it, so a sprint could close clean over open work. That is a success state
// reached because nothing looked — the failure shape the fleet evidence
// invariant exists to forbid.
//
// The fix is one function and two gates. Uncovered OPEN stories are reported
// everywhere the plan is shown, and they block closing until somebody makes an
// explicit decision: link them, close them, or move them off the sprint.
// Silence is never one of the options.

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/issue"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// sprintStoryCovered reports whether any goal item references this story.
func sprintStoryCovered(s *weaveStory, ref sprintStoryRef) bool {
	for _, g := range s.Goal {
		for _, have := range g.Stories {
			if sameSprintStoryRef(have, ref) {
				return true
			}
		}
	}
	return false
}

// sameSprintStoryRef compares two story refs.
//
// The ID is compared with a PREFIX rule in both directions because the surfaces
// disagree about width on purpose: goal items are shown with an 8-character id
// while a story ref carries the full 12. Requiring equality would report every
// correctly-linked story as uncovered.
func sameSprintStoryRef(a, b sprintStoryRef) bool {
	if strings.TrimSpace(a.Repo) != "" && strings.TrimSpace(b.Repo) != "" &&
		strings.TrimSpace(a.Repo) != strings.TrimSpace(b.Repo) {
		return false
	}
	x, y := strings.TrimSpace(a.ID), strings.TrimSpace(b.ID)
	if x == "" || y == "" {
		return false
	}
	return strings.HasPrefix(x, y) || strings.HasPrefix(y, x)
}

// sprintOpenStory reports whether a story still needs work.
func sprintOpenStory(st sprintStoryState) bool {
	return st.Status != todopkg.StatusDone &&
		st.Status != issue.StatusClosed &&
		st.Status != todopkg.StatusBlocked
}

// sprintGoalUncovered returns the sprint's OPEN stories that no goal item
// references, in the same priority-first order the execution queue uses.
//
// Only OPEN stories count. A story that was completed without ever being in the
// plan is a documentation gap, not a delivery risk, and blocking a close on it
// would punish the honest case where work got done.
func sprintGoalUncovered(s *weaveStory) ([]sprintStoryState, error) {
	if s == nil {
		return nil, nil
	}
	stories, err := loadSprintStories(s)
	if err != nil {
		return nil, err
	}
	var out []sprintStoryState
	for _, st := range stories {
		if !sprintOpenStory(st) {
			continue
		}
		if sprintStoryCovered(s, st.Ref) {
			continue
		}
		out = append(out, st)
	}
	return out, nil
}

// sprintCoverageProblems states, in one line each, why the plan is out of date.
//
// It swallows the load error into a reported problem rather than returning it:
// a sprint whose story roots cannot be read is exactly as unclosable as one
// with uncovered work, and a close gate that fails OPEN on an unreadable store
// would be the same absence-of-evidence bug in a new place.
func sprintCoverageProblems(s *weaveStory) []string {
	uncovered, err := sprintGoalUncovered(s)
	if err != nil {
		return []string{fmt.Sprintf("story index unreadable, so coverage is unknown: %v", err)}
	}
	if len(uncovered) == 0 {
		return nil
	}
	var out []string
	for _, st := range uncovered {
		out = append(out, fmt.Sprintf("%s [%s] %s", shortSprintStoryID(st.Ref.ID), st.Priority, st.Title))
	}
	sort.Strings(out)
	return out
}

// shortSprintStoryID renders a story id at the width the goal checklist uses.
func shortSprintStoryID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// renderSprintCoverage prints the uncovered-story block, or nothing when the
// plan is current. Silence means the plan describes the sprint.
func renderSprintCoverage(w io.Writer, s *weaveStory) {
	problems := sprintCoverageProblems(s)
	if len(problems) == 0 {
		return
	}
	fmt.Fprintf(w, "  ── UNCOVERED by the plan (%d open) ──\n", len(problems))
	for _, p := range problems {
		fmt.Fprintf(w, "  (!) %s\n", p)
	}
	fmt.Fprintf(w, "  link with `bashy sprint goal add %d --story <id>`, close them, or move them off the sprint\n", s.ID)
}

// sprintCoverageGate refuses a close while open stories sit outside the plan.
//
// The three ways out are all EXPLICIT, which is the point: the conductor states
// what the unplanned work was, rather than a gate quietly not looking at it.
func sprintCoverageGate(s *weaveStory, force bool, reason string) error {
	return sprintCoverageGateFor(s, sprintCoverageProblems(s), force, reason)
}

// sprintCoverageGateFor is the decision half of sprintCoverageGate, taking the
// problem set directly so a test can exercise the rule without a story store.
func sprintCoverageGateFor(s *weaveStory, problems []string, force bool, reason string) error {
	if len(problems) == 0 {
		return nil
	}
	if force {
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("sprint #%d has %d open story(s) the plan does not cover; --force needs --reason to say why closing over them is right",
				s.ID, len(problems))
		}
		return nil
	}
	return fmt.Errorf("sprint #%d has %d open story(s) the plan does not cover:\n  %s\n"+
		"  link them:  bashy sprint goal add %d --story <id>\n"+
		"  or close them, or move them off the sprint\n"+
		"  or, on the record: --force --reason \"<why>\"",
		s.ID, len(problems), strings.Join(problems, "\n  "), s.ID)
}
