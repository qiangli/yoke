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
	if !sprintGoalDone(only) || !sprintGoalWontfix(only) {
		t.Fatal("a goal whose stories are all wontfix must be closed and marked won't-do")
	}
	mixed := sprintGoalItem{ID: "T2", Stories: []sprintStoryRef{{Repo: root, ID: dropped}, {Repo: root, ID: shipped}}}
	if !sprintGoalDone(mixed) || sprintGoalWontfix(mixed) {
		t.Fatal("a goal with a delivered story is closed and delivered, not won't-do")
	}
	if !sprintOpenStory(sprintStoryState{Status: todopkg.StatusTodo}) || sprintOpenStory(sprintStoryState{Status: todopkg.StatusWontfix}) {
		t.Fatal("wontfix must not count as open work")
	}
	if next, err := nextSprintStory(s); err != nil || next != nil {
		t.Fatalf("next story with only closed stories = %#v, %v", next, err)
	}
}
