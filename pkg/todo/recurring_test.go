// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package todo

import (
	"strings"
	"testing"
	"time"
)

func TestParseDue(t *testing.T) {
	// relative days
	t1, err := parseDue("+3d")
	if err != nil || t1 == nil {
		t.Fatalf("failed to parse +3d: %v", err)
	}
	expected := time.Now().UTC().AddDate(0, 0, 3)
	if t1.YearDay() != expected.YearDay() {
		t.Fatalf("expected yearday %d, got %d", expected.YearDay(), t1.YearDay())
	}

	// relative hours
	t2, err := parseDue("+2h")
	if err != nil || t2 == nil {
		t.Fatalf("failed to parse +2h: %v", err)
	}

	// absolute
	t3, err := parseDue("2026-07-20T15:00")
	if err != nil || t3 == nil {
		t.Fatalf("failed to parse absolute: %v", err)
	}
	if t3.Year() != 2026 || t3.Month() != 7 || t3.Day() != 20 {
		t.Fatalf("wrong absolute date: %v", t3)
	}
}

func TestRecurringBehavior(t *testing.T) {
	t.Setenv("BASHY_TODO_DIR", t.TempDir())
	pinTodoAgents(t, "alice")
	st, _ := UserStore("steward")

	due := time.Now().UTC()
	it, err := Add(st, "daily task", "body", "p1", &due, "daily", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if it.Assignee != "alice" {
		t.Fatalf("expected assignee alice, got %s", it.Assignee)
	}

	// mark done
	it, err = SetStatus(st, it.ID, StatusDone)
	if err != nil {
		t.Fatal(err)
	}

	// should reopen
	if it.Status != StatusTodo {
		t.Fatalf("recurring item should reopen as todo, got %s", it.Status)
	}
	if it.Closed != nil {
		t.Fatalf("recurring item should not be closed")
	}

	// due should be advanced
	if it.Due == nil {
		t.Fatalf("due should not be nil")
	}
	if it.Due.YearDay() != due.AddDate(0, 0, 1).YearDay() {
		t.Fatalf("due not advanced correctly")
	}
}

// A SPRINT-BOUND recurring story takes its cadence from the sprint's cycle,
// not from the clock. Reopening it here would erase the very completion the
// cycle record has to attest — this branch never stamps Closed — so the tenth
// iteration would be indistinguishable from the first.
func TestSprintBoundRecurringStoryClosesHonestlyInsteadOfReopening(t *testing.T) {
	t.Setenv("BASHY_TODO_DIR", t.TempDir())
	pinTodoAgents(t, "alice")
	st, _ := UserStore("steward")

	it, err := Add(st, "tag and push", "", "p1", nil, CadenceSprint, "")
	if err != nil {
		t.Fatal(err)
	}
	it.Sprint = 129
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}

	if _, err := SetStatus(st, it.ID, StatusDone); err == nil || !strings.Contains(err.Error(), "sprint accept 129") {
		t.Fatalf("generic completion error = %v", err)
	}
	got, _ := ResolveRef(st, it.ID)
	if got.Status != StatusTodo || got.Closed != nil {
		t.Errorf("refused generic completion mutated story: status=%q closed=%v", got.Status, got.Closed)
	}
}

// An UNBOUND recurring todo is a personal chore on its own clock, and keeps
// the original behaviour: it reopens and its due date advances.
func TestUnboundRecurringTodoStillReopensOnDone(t *testing.T) {
	t.Setenv("BASHY_TODO_DIR", t.TempDir())
	pinTodoAgents(t, "alice")
	st, _ := UserStore("steward")

	due := time.Now().UTC()
	it, err := Add(st, "water the plants", "", "p2", &due, "weekly", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := SetStatus(st, it.ID, StatusDone)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusTodo {
		t.Errorf("status = %q, want %q", got.Status, StatusTodo)
	}
	if got.Due == nil || !got.Due.After(due) {
		t.Errorf("due did not advance: %v", got.Due)
	}
}

// A cadence nothing can interpret used to be indistinguishable from an
// on-demand one: Add never checked it and SetStatus discarded the parse error,
// so a typo silently became "repeats, but never becomes due".
func TestInvalidCadenceIsRejectedAtWriteTime(t *testing.T) {
	t.Setenv("BASHY_TODO_DIR", t.TempDir())
	pinTodoAgents(t, "alice")
	st, _ := UserStore("steward")

	for _, ok := range []string{"", CadenceSprint, "daily", "weekly", "monthly", "24h", "*/15 * * * *"} {
		if err := ValidateCadence(ok); err != nil {
			t.Errorf("ValidateCadence(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"wekly", "every monday", "24hours"} {
		if err := ValidateCadence(bad); err == nil {
			t.Errorf("ValidateCadence(%q) = nil, want an error", bad)
		}
	}
	if _, err := Add(st, "typo", "", "p1", nil, "wekly", ""); err == nil {
		t.Error("Add accepted an uninterpretable cadence")
	}
}
