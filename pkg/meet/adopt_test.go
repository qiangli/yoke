package meet

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/todo"
)

func adoptFixture(t *testing.T) (*State, *issue.Store, []agreedAction) {
	t.Helper()
	st := newTestSession(t)
	_, _ = record(st, "action", "alice", "", "bob: write the migration plan")
	_, _ = record(st, "action", "alice", "", "ship the plan")
	events, _ := readTranscript(st.ID)
	syn := &Synthesis{Actions: []string{"add a dashboard"}}
	store := &issue.Store{Root: t.TempDir(), Sub: "todo"}
	return st, store, agreedActions(st.ID, events, syn)
}

func TestAdoptionPreviewWritesNothingAndIsIdempotent(t *testing.T) {
	st, store, acts := adoptFixture(t)
	if acts[0].Owner != "bob" || acts[0].Source != sourceAgreed || acts[2].Source != sourceProposed {
		t.Fatalf("unexpected actions: %+v", acts)
	}
	specs := map[string]adoptSpec{acts[0].ID: {Accept: []string{"plan reviewed"}}}
	if _, err := adoptActions(st.ID, store, acts[:1], specs, "op", 0, false); err != nil {
		t.Fatal(err)
	}
	if all, _ := store.List(); len(all) != 0 {
		t.Fatalf("preview wrote %d stories", len(all))
	}
	for i := 0; i < 3; i++ {
		if _, err := adoptActions(st.ID, store, acts[:1], specs, "op", 0, true); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := store.List()
	if len(all) != 1 {
		t.Fatalf("repeated adoption made %d stories", len(all))
	}
	it := all[0]
	if it.Status != todo.StatusTodo || it.Assignee != "bob" || extString(it, extRoom) != st.ID {
		t.Fatalf("story not linked/owned without dispatch: %+v", it)
	}
	if got := extStrings(it, extAccept); len(got) != 1 {
		t.Fatalf("acceptance lost across round trip: %v", got)
	}
}

func TestAdoptionRequiresAcceptanceAndResolvableDependency(t *testing.T) {
	st, store, acts := adoptFixture(t)
	if _, err := adoptActions(st.ID, store, acts[:1], nil, "op", 0, true); err == nil {
		t.Fatal("adoption without acceptance criteria must fail")
	}
	specs := map[string]adoptSpec{acts[0].ID: {Accept: []string{"x"}, Depends: []string{"act-nope"}}}
	if _, err := adoptActions(st.ID, store, acts[:1], specs, "op", 0, true); err == nil {
		t.Fatal("unknown dependency must fail")
	}
	if all, _ := store.List(); len(all) != 0 {
		t.Fatal("failed validation left stories behind")
	}
}

func TestAdoptionDependenciesInOneBatch(t *testing.T) {
	st, store, acts := adoptFixture(t)
	specs := map[string]adoptSpec{
		acts[0].ID: {Accept: []string{"a"}, Depends: []string{acts[1].ID}},
		acts[1].ID: {Accept: []string{"b"}},
	}
	if _, err := adoptActions(st.ID, store, acts[:2], specs, "op", 0, true); err != nil {
		t.Fatal(err)
	}
	m, _ := adoptedStories(store, st.ID)
	deps := extStrings(m[acts[0].ID], extDepends)
	if len(deps) != 1 || deps[0] != m[acts[1].ID].ID {
		t.Fatalf("dependency not resolved to story ID: %v", deps)
	}
}

func TestChangedDecisionRequestsReconciliationOnce(t *testing.T) {
	st, store, acts := adoptFixture(t)
	specs := map[string]adoptSpec{
		acts[0].ID: {Accept: []string{"a"}},
		acts[1].ID: {Accept: []string{"b"}, Depends: []string{acts[0].ID}},
	}
	if _, err := adoptActions(st.ID, store, acts[:2], specs, "op", 0, true); err != nil {
		t.Fatal(err)
	}
	// The room re-decides: action 0 is replaced by a reworded one.
	current := []agreedAction{acts[1], {ID: "act-new", Task: "write the revised migration plan", Source: sourceAgreed}}
	f, err := reconcileAdopted(st.ID, store, current, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 2 || f[0].Replacement != "act-new" || !f[0].Requested {
		t.Fatalf("findings: %+v", f)
	}
	m, _ := adoptedStories(store, st.ID)
	if !hasLabel(m[acts[0].ID], labelReconcile) || !strings.Contains(m[acts[0].ID].Body, "Reconciliation requested") {
		t.Fatal("story not marked for reconciliation")
	}
	f, _ = reconcileAdopted(st.ID, store, current, true)
	for _, x := range f {
		if x.Requested {
			t.Fatalf("reconciliation requested twice: %+v", x)
		}
	}
}

func TestOutcomeReturnsVerifiedResultOnce(t *testing.T) {
	st, store, acts := adoptFixture(t)
	specs := map[string]adoptSpec{acts[0].ID: {Accept: []string{"a"}}}
	if _, err := adoptActions(st.ID, store, acts[:1], specs, "op", 0, true); err != nil {
		t.Fatal(err)
	}
	m, _ := adoptedStories(store, st.ID)
	story := m[acts[0].ID]
	var posts []string
	post := func(room, text string) error {
		if room != st.ID {
			t.Fatalf("posted to %s", room)
		}
		posts = append(posts, text)
		return nil
	}
	if _, _, err := recordOutcome(store, story.ID, []string{"commit:abc"}, post); err == nil {
		t.Fatal("an unverified (not done) story must not report")
	}
	story.Status = todo.StatusDone
	if _, err := store.Save(story); err != nil {
		t.Fatal(err)
	}
	if _, _, err := recordOutcome(store, story.ID, nil, post); err == nil {
		t.Fatal("outcome without evidence must fail")
	}
	for i := 0; i < 2; i++ {
		if _, _, err := recordOutcome(store, story.ID, []string{"commit:abc"}, post); err != nil {
			t.Fatal(err)
		}
	}
	if len(posts) != 1 || !strings.Contains(posts[0], "commit:abc") {
		t.Fatalf("posts: %v", posts)
	}
}
