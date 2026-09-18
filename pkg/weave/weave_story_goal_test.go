package weave

import (
	"io"
	"strings"
	"testing"
	"time"

	todopkg "github.com/qiangli/yoke/pkg/todo"
)

func sprintTestStory(t *testing.T, root string, sprint int64, title, priority, status string) string {
	t.Helper()
	st := todopkg.RepoStore(root)
	it, err := todopkg.Add(st, title, "", priority, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	it.Sprint = sprint
	it.Status = status
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	return it.ID
}

func TestSprintNextDerivesPriorityFromStories(t *testing.T) {
	root := t.TempDir()
	s := &weaveStory{ID: 99123, StoryRoots: []string{root}}
	low := sprintTestStory(t, root, s.ID, "low", "p2", todopkg.StatusTodo)
	high := sprintTestStory(t, root, s.ID, "high", "p0", todopkg.StatusTodo)
	next, err := nextSprintStory(s)
	if err != nil {
		t.Fatal(err)
	}
	if next == nil || next.Ref.ID != high {
		t.Fatalf("next = %#v, want p0 %s ahead of p2 %s", next, high, low)
	}
	// Priority remains authoritative on the story: editing it changes the index
	// without rewriting the sprint card.
	st := todopkg.RepoStore(root)
	it, _ := todopkg.ResolveRef(st, low)
	it.Priority = "p0"
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	next, err = nextSprintStory(s)
	if err != nil || next == nil || next.Ref.ID != low {
		t.Fatalf("next after priority edit = %#v, err=%v, want %s", next, err, low)
	}
}

func TestSprintGoalCompletionFollowsStoryClosureAndReopen(t *testing.T) {
	root := t.TempDir()
	s := &weaveStory{ID: 99124, StoryRoots: []string{root}}
	id := sprintTestStory(t, root, s.ID, "deliver", "p0", todopkg.StatusTodo)
	g := sprintGoalItem{ID: "delivery", Text: "deliver it", Stories: []sprintStoryRef{{Repo: root, ID: id}}}
	if sprintGoalDone(g) {
		t.Fatal("open story checked the goal")
	}
	st := todopkg.RepoStore(root)
	it, err := todopkg.ResolveRef(st, id)
	if err != nil {
		t.Fatal(err)
	}
	it.Status = todopkg.StatusDone
	now := time.Now().UTC()
	it.Closed = &now
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	if !sprintGoalDone(g) {
		t.Fatal("closed story did not check the goal")
	}
	it.Status = todopkg.StatusTodo
	it.Closed = nil
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	if sprintGoalDone(g) {
		t.Fatal("reopened story did not uncheck the goal")
	}
}

func TestSprintGateEvidence(t *testing.T) {
	root := t.TempDir()
	id := sprintTestStory(t, root, 99125, "gated", "p0", todopkg.StatusDone)
	g := sprintGoalItem{ID: "gate", Stories: []sprintStoryRef{{Repo: root, ID: id}}, GateRequired: true}
	if sprintGoalDone(g) {
		t.Fatal("gate-required goal checked without evidence")
	}
	g.Evidence = "go test ./... PASS"
	if !sprintGoalDone(g) {
		t.Fatal("closed story plus evidence did not check goal")
	}
}

// A sprint whose remaining stories moved to a successor used to be permanently
// unclosable: the goal item still points at a story that now belongs to the
// other card, sprintGoalDone can never be true here, and `sprint move done`
// refuses over it with a check --force does not cover. `sprint goal rm` is the
// exit, and it must lose nothing on the way out.
func TestSprintGoalRmRetiresAnOutcomeMovedToASuccessor(t *testing.T) {
	root := t.TempDir()
	s := &weaveStory{ID: 99126, StoryRoots: []string{root}}
	moved := sprintTestStory(t, root, s.ID, "carry me to the next sprint", "p0", todopkg.StatusTodo)
	s.Goal = []sprintGoalItem{
		{ID: "kept", Text: "still required here"},
		{ID: "moved", Text: "activation reaches production", GateRequired: true,
			Stories:  []sprintStoryRef{{Repo: root, ID: moved}},
			Evidence: ""},
	}

	// The story moves to the successor sprint, exactly as `todo edit --sprint`
	// leaves it. The goal it was covering is now unsatisfiable on this card.
	st := todopkg.RepoStore(root)
	it, err := todopkg.ResolveRef(st, moved)
	if err != nil {
		t.Fatal(err)
	}
	it.Sprint = s.ID + 1
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	if got := sprintUncheckedGoals(s); len(got) != 2 {
		t.Fatalf("unchecked = %v, want both items blocking before the retirement", got)
	}

	idx := -1
	for i := range s.Goal {
		if s.Goal[i].ID == "moved" {
			idx = i
		}
	}
	retired := s.Goal[idx]
	s.Goal = append(s.Goal[:idx], s.Goal[idx+1:]...)
	weaveStoryAppend(s, "tester", "decision", sprintGoalEpitaph(retired, "carried to sprint #99127"))

	if got := sprintUncheckedGoals(s); len(got) != 1 || got[0] != "kept" {
		t.Fatalf("unchecked after retiring = %v, want only the item that really is still required", got)
	}

	// Removing the index entry must not remove the record. Everything the
	// checklist knew has to survive on the thread, or the retirement is a way
	// to hide an outcome rather than relocate one.
	if len(s.Thread) == 0 {
		t.Fatal("retiring a goal wrote nothing to the thread")
	}
	body := s.Thread[len(s.Thread)-1].Body
	for _, want := range []string{"moved", "activation reaches production", "carried to sprint #99127", moved, "gate-required: yes"} {
		if !strings.Contains(body, want) {
			t.Errorf("thread record is missing %q:\n%s", want, body)
		}
	}
}

// --reason is not decoration. A retirement with no stated destination is
// indistinguishable from deleting an outcome you failed to meet.
func TestSprintGoalRmRequiresAReason(t *testing.T) {
	cmd := newSprintGoalRmCmd()
	cmd.SetArgs([]string{"99128", "some-goal"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--reason is required") {
		t.Fatalf("err = %v, want a refusal naming --reason", err)
	}
}
