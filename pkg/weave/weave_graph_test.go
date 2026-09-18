// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package weave

import (
	"testing"
)

func q(items ...*weaveItem) *weaveQueue {
	return &weaveQueue{NextID: int64(len(items) + 1), Items: items}
}

func todo(id int64) *weaveItem { return &weaveItem{ID: id, State: "todo", Priority: "p2"} }

// ---------------------------------------------------------------------------
// D1 — the stage
// ---------------------------------------------------------------------------

// An existing queue has no Stage on any item. It must keep working, and every one of
// those items must read as a CODING issue — which is not a guess, it is the truth
// about the data: before the field existed, a weave issue could not be anything else.
func TestStageDefaultsToCodeWithNoMigration(t *testing.T) {
	it := todo(1) // as loaded from a pre-stage queue.json: Stage == ""
	if got := itemStage(it); got != "code" {
		t.Fatalf("a legacy issue reads as stage %q, want \"code\" — every existing queue would be misfiled", got)
	}
}

// "cross" is a VERB stage, not a WORK stage. A command can serve every part of the
// lifecycle (git is used while planning, coding, testing and deploying); a unit of
// work cannot BE every part of it. Allowing "cross" on an issue would just be a
// spelling of "unclassified" — the silence this axis exists to break.
func TestStageRejectsCrossOnWork(t *testing.T) {
	if _, err := normalizeStage("cross"); err == nil {
		t.Fatal("stage \"cross\" was accepted on a work item — that is 'unclassified' wearing a costume")
	}
	if _, err := normalizeStage("shipit"); err == nil {
		t.Fatal("an unknown stage was accepted; the vocabulary must be closed or it is not a vocabulary")
	}
	for _, ok := range []string{"plan", "code", "test", "deploy", "PLAN", " deploy "} {
		if _, err := normalizeStage(ok); err != nil {
			t.Errorf("normalizeStage(%q) = %v, want accepted", ok, err)
		}
	}
}

// The vocabulary is the atlas's, not a second copy of it. If someone adds a stage to
// the atlas, weave must learn it for free — two lists drift within a release, and the
// entire point of the axis is that one word means one thing everywhere.
func TestStageVocabularyComesFromTheAtlas(t *testing.T) {
	got := weaveStages()
	want := []string{"plan", "code", "test", "deploy"} // atlas.Stages() minus "cross"
	if len(got) != len(want) {
		t.Fatalf("weave stages = %v, want %v (derived from atlas.Stages())", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("weave stages = %v, want %v", got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// D2 — the graph
// ---------------------------------------------------------------------------

// THE PAYOFF. "Schedule by parallel safety" was the conductor's central rule and it
// lived only in the conductor's head — so a resumed conductor, or a second agent,
// would hand out work whose prerequisite had not landed. Now the queue refuses.
func TestBlockedIssueIsNeverHandedOut(t *testing.T) {
	a, b := todo(1), todo(2)
	b.DependsOn = []int64{1}
	queue := q(a, b)

	// #2 waits on #1, so #1 goes first...
	if got := nextTodo(queue); got == nil || got.ID != 1 {
		t.Fatalf("nextTodo = %v, want #1", got)
	}
	// ...and #2 is not claimable at all while #1 is unfinished.
	if weaveClaimable(queue, b) {
		t.Fatal("a blocked issue was claimable — an agent would be handed work whose prerequisite is not in main")
	}
	a.State = "done"
	if !weaveClaimable(queue, b) {
		t.Fatal("the dependency landed and the dependent is still blocked")
	}
}

// The subtle one, and the reason "submitted" is not good enough.
//
// A dependent's workspace is cloned FROM MAIN. A "submitted" issue's branch exists but
// has not been merged, so main does not contain its code. Starting the dependent then
// hands an agent a tree WITHOUT the code it was told to build on — which is exactly
// the failure the dependency was declared to prevent.
func TestDependencySatisfiedOnlyWhenMerged(t *testing.T) {
	dep, it := todo(1), todo(2)
	it.DependsOn = []int64{1}
	queue := q(dep, it)

	for _, state := range []string{"working", "submitted", "failed", "killed", "abandoned"} {
		dep.State = state
		if weaveClaimable(queue, it) {
			t.Errorf("dependency in state %q was treated as satisfied; only \"done\" (merged into main) may unblock a dependent", state)
		}
	}
	dep.State = "done"
	if !weaveClaimable(queue, it) {
		t.Error("a merged dependency did not unblock its dependent")
	}
}

// A BLOCKED queue must not look like an EMPTY one. "Empty" means finished; a queue
// full of work all waiting on a dead dependency means stuck. If both render the same,
// a conductor reads a deadlock as success and walks away.
func TestBlockedQueueIsDistinguishableFromEmpty(t *testing.T) {
	dead, waiter := todo(1), todo(2)
	dead.State = "failed"
	waiter.DependsOn = []int64{1}
	queue := q(dead, waiter)

	if nextTodo(queue) != nil {
		t.Fatal("nextTodo handed out work that waits on a failed dependency")
	}
	blocked := weaveBlockedTodos(queue)
	if len(blocked) != 1 || blocked[0].ID != 2 {
		t.Fatalf("blocked = %v, want [#2] — otherwise the deadlock prints as \"queue empty\"", blocked)
	}
	if !weaveDepDead(weaveBlockers(queue, waiter)[0]) {
		t.Fatal("a failed dependency was not reported as dead; the human would never know to unlink it")
	}

	// An empty queue is genuinely empty — no false alarm.
	if len(weaveBlockedTodos(q())) != 0 {
		t.Fatal("an empty queue reported blocked work")
	}
}

// A cycle would make both issues permanently unclaimable, and the queue would print
// "nothing claimable" while holding work that can never run. Refuse at link time,
// where a human is present to fix it.
func TestCycleIsRefused(t *testing.T) {
	a, b, c := todo(1), todo(2), todo(3)
	b.DependsOn = []int64{1} // 2 -> 1
	c.DependsOn = []int64{2} // 3 -> 2
	queue := q(a, b, c)

	if !weaveWouldCycle(queue, 1, 3) {
		t.Fatal("1 depending on 3 closes the loop 1->3->2->1 and was not detected")
	}
	if !weaveWouldCycle(queue, 1, 1) {
		t.Fatal("an issue depending on itself is a cycle")
	}
	if weaveWouldCycle(queue, 3, 1) {
		t.Fatal("3 already reaches 1 transitively; adding the direct edge is redundant, not a cycle")
	}
}

// An epic is worked through its CHILDREN. Handing the parent to an agent would mean
// two agents doing the same work — the parent's whole scope plus each slice of it.
func TestContainerIsNeverClaimedButItsChildrenAre(t *testing.T) {
	parent, k1, k2 := todo(1), todo(2), todo(3)
	k1.Parent, k2.Parent = 1, 1
	queue := q(parent, k1, k2)

	if weaveClaimable(queue, parent) {
		t.Fatal("an epic was claimable — an agent would redo the whole scope its children already cover")
	}
	if !weaveClaimable(queue, k1) {
		t.Fatal("a child of an epic was not claimable")
	}
	if got := nextTodo(queue); got == nil || got.ID != 2 {
		t.Fatalf("nextTodo = %v, want the first child (#2)", got)
	}

	// The epic's state is DERIVED, never stored — so it cannot go stale as the
	// children move, and no transition (start/pull/salvage/abandon/prune) has to
	// remember to update it.
	if got := weaveDerivedState(queue, parent); got != "todo" {
		t.Fatalf("derived state = %q, want todo", got)
	}
	k1.State = "working"
	if got := weaveDerivedState(queue, parent); got != "working" {
		t.Fatalf("derived state = %q, want working", got)
	}
	k1.State, k2.State = "done", "done"
	if got := weaveDerivedState(queue, parent); got != "done" {
		t.Fatalf("all children done but the epic reads %q", got)
	}
}

// Back-compat: a queue with no stage, no parent and no deps behaves exactly as it did
// before this feature. Every existing queue on every machine is one of these.
func TestLegacyQueueIsUnchanged(t *testing.T) {
	a, b := todo(1), todo(2)
	a.Priority, b.Priority = "p2", "p0"
	queue := q(a, b)
	if got := nextTodo(queue); got == nil || got.ID != 2 {
		t.Fatalf("nextTodo = %v, want #2 (p0 outranks p2) — priority ordering regressed", got)
	}
	if len(weaveBlockedTodos(queue)) != 0 {
		t.Fatal("a legacy queue reported blocked work")
	}
}
