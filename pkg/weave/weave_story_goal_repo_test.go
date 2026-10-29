package weave

import (
	"strings"
	"testing"

	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// Sprint #531 shape: a goal linked while the wrong checkout was current pins a
// stale repo on the stored ref. The story itself is accepted in another repo
// the sprint tracks, but the checklist stays unchecked with a dangling
// warning, and re-linking with --repo reports "already links" without healing.
func TestSprintGoalStaleRepoStillChecksWhenTrackedElsewhere(t *testing.T) {
	actual := t.TempDir()
	stale := t.TempDir()
	s := &weaveStory{ID: 99201, StoryRoots: []string{stale, actual}}
	id := sprintTestStory(t, actual, s.ID, "register", "p0", todopkg.StatusDone)
	g := sprintGoalItem{ID: "d1", Text: "register ships", Stories: []sprintStoryRef{{Repo: stale, ID: id}}}

	if !sprintGoalDone(s, g) {
		t.Fatalf("goal with a closed story under a stale repo did not check (dangling=%v)", sprintGoalDangling(s, g))
	}
	if dangling := sprintGoalDangling(s, g); len(dangling) != 0 {
		t.Fatalf("dangling = %v, want none: the story is tracked at %s", dangling, actual)
	}
	if got := sprintUncheckedGoals(s); len(got) != 0 {
		t.Fatalf("unchecked = %v, want none after the fallback resolved the story", got)
	}
}

// The fallback must not check what is not there: an open story under a stale
// repo stays unchecked, and a story in no tracked root stays dangling.
func TestSprintGoalStaleRepoDoesNotHideOpenOrMissing(t *testing.T) {
	actual := t.TempDir()
	stale := t.TempDir()
	s := &weaveStory{ID: 99202, StoryRoots: []string{stale, actual}}
	open := sprintTestStory(t, actual, s.ID, "open work", "p0", todopkg.StatusTodo)
	g := sprintGoalItem{ID: "d1", Text: "open work ships", Stories: []sprintStoryRef{{Repo: stale, ID: open}}}
	if sprintGoalDone(s, g) {
		t.Fatal("open story under a stale repo checked the goal")
	}
	missing := sprintGoalItem{ID: "d2", Text: "never filed", Stories: []sprintStoryRef{{Repo: stale, ID: "deadbeefcafe"}}}
	if sprintGoalDone(s, missing) {
		t.Fatal("missing story checked the goal")
	}
	if dangling := sprintGoalDangling(s, missing); len(dangling) != 1 {
		t.Fatalf("dangling = %v, want the one truly missing link", dangling)
	}
}

func TestSprintGoalLinkHealsStaleRepoForSameStory(t *testing.T) {
	actual := t.TempDir()
	stale := t.TempDir()
	s := &weaveStory{ID: 99203, StoryRoots: []string{stale}}
	id := sprintTestStory(t, actual, s.ID, "register", "p0", todopkg.StatusDone)
	s.Goal = []sprintGoalItem{{ID: "d1", Text: "register ships", Stories: []sprintStoryRef{{Repo: stale, ID: id}}}}
	it, err := todopkg.ResolveRef(todopkg.RepoStore(actual), id)
	if err != nil {
		t.Fatal(err)
	}

	msg, err := linkSprintGoalStory(s, "d1", actual, it)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "corrected link") {
		t.Fatalf("re-link answered %q, want the repo correction, not a silent already-links", msg)
	}
	got := findSprintGoal(s, "d1").Stories
	if len(got) != 1 || got[0].Repo != actual || got[0].ID != id {
		t.Fatalf("stories = %+v, want the single healed ref %s#%s", got, actual, id)
	}
	if !sprintGoalDone(s, *findSprintGoal(s, "d1")) {
		t.Fatal("healed goal did not check")
	}

	msg, err = linkSprintGoalStory(s, "d1", actual, it)
	if err != nil || !strings.Contains(msg, "already links") {
		t.Fatalf("re-link = %q, %v; want idempotent already-links", msg, err)
	}
	if len(findSprintGoal(s, "d1").Stories) != 1 {
		t.Fatalf("stories = %+v, want no duplicate ref stacked", findSprintGoal(s, "d1").Stories)
	}
}

// Duplicates the old exact-ref check stacked (one story id under two repos)
// collapse to the healed ref instead of leaving a permanently dangling twin.
func TestSprintGoalLinkCollapsesStackedDuplicates(t *testing.T) {
	actual := t.TempDir()
	stale := t.TempDir()
	other := t.TempDir()
	s := &weaveStory{ID: 99204, StoryRoots: []string{stale, other}}
	id := sprintTestStory(t, actual, s.ID, "register", "p0", todopkg.StatusDone)
	s.Goal = []sprintGoalItem{{ID: "d1", Text: "register ships", Stories: []sprintStoryRef{
		{Repo: stale, ID: id},
		{Repo: other, ID: id},
	}}}
	it, err := todopkg.ResolveRef(todopkg.RepoStore(actual), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := linkSprintGoalStory(s, "d1", actual, it); err != nil {
		t.Fatal(err)
	}
	got := findSprintGoal(s, "d1").Stories
	if len(got) != 1 || got[0].Repo != actual {
		t.Fatalf("stories = %+v, want one healed ref", got)
	}
}
