package weave

// Sprint: #301; Story: #959; Story-ID: cb38b7c214d0

import (
	"testing"

	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// A wontfix story closes its goal item and stops being open work, but the
// goal is marked closed by decision, never as delivered.
func TestSprintWontfixStoryClosesGoalWithoutDelivery(t *testing.T) {
	root := t.TempDir()
	s := &weaveStory{ID: 99130, StoryRoots: []string{root}}
	dropped := sprintTestStory(t, root, s.ID, "dropped", "p1", todopkg.StatusWontfix)
	shipped := sprintTestStory(t, root, s.ID, "shipped", "p1", todopkg.StatusDone)
	only := sprintGoalItem{ID: "T7", Stories: []sprintStoryRef{{Repo: root, ID: dropped}}}
	if !sprintGoalDone(s, only) || !sprintGoalWontfix(s, only) {
		t.Fatal("a goal whose stories are all wontfix must be closed and marked won't-do")
	}
	mixed := sprintGoalItem{ID: "T2", Stories: []sprintStoryRef{{Repo: root, ID: dropped}, {Repo: root, ID: shipped}}}
	if !sprintGoalDone(s, mixed) || sprintGoalWontfix(s, mixed) {
		t.Fatal("a goal with a delivered story is closed and delivered, not won't-do")
	}
	if !sprintOpenStory(sprintStoryState{Status: todopkg.StatusTodo}) || sprintOpenStory(sprintStoryState{Status: todopkg.StatusWontfix}) || sprintOpenStory(sprintStoryState{Status: todopkg.StatusKnown}) {
		t.Fatal("wontfix and known must not count as open work")
	}
	if next, err := nextSprintStory(s); err != nil || next != nil {
		t.Fatalf("next story with only closed stories = %#v, %v", next, err)
	}
}

func TestSprintKnownStoryDoesNotCountAsOpenWork(t *testing.T) {
	root := t.TempDir()
	s := &weaveStory{ID: 99131, StoryRoots: []string{root}}
	ki := sprintTestStory(t, root, s.ID, "known issue", "p0", todopkg.StatusKnown)
	goal := sprintGoalItem{ID: "KI", Stories: []sprintStoryRef{{Repo: root, ID: ki}}}
	if !sprintGoalDone(s, goal) {
		t.Fatal("a goal whose only story is a tracked known issue must not remain open")
	}
	if sprintGoalWontfix(s, goal) {
		t.Fatal("a known issue must not be reported as wontfix")
	}
	if !sprintGoalKnown(s, goal) {
		t.Fatal("a goal whose stories are all known issues must be marked known, not delivered")
	}
	shipped := sprintTestStory(t, root, s.ID, "shipped", "p1", todopkg.StatusDone)
	if sprintGoalKnown(s, sprintGoalItem{ID: "M", Stories: []sprintStoryRef{{Repo: root, ID: ki}, {Repo: root, ID: shipped}}}) {
		t.Fatal("a goal with a delivered story is delivered, not known")
	}
	if next, err := nextSprintStory(s); err != nil || next != nil {
		t.Fatalf("next story with only known stories = %#v, %v", next, err)
	}
}
