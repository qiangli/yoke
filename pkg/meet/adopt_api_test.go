package meet

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/todo"
)

// tickingClock gives every event a distinct timestamp, which retractions need
// (a retraction names its target by timestamp).
func tickingClock(t *testing.T) {
	t.Helper()
	var mu sync.Mutex
	base := fixedNow()
	n := 0
	nowFn = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		n++
		return base.Add(time.Duration(n) * time.Millisecond)
	}
}

func TestRetractedActionLeavesActiveCandidates(t *testing.T) {
	st := newTestSession(t)
	tickingClock(t)
	ev, _ := record(st, "action", "qiangli", "", "bob: write the migration plan")
	_, _ = record(st, "action", "qiangli", "", "ship the plan")
	// The secretary still lists the retracted action: it must not revive it.
	syn := &Synthesis{Actions: []string{"bob: write the migration plan", "add a dashboard"}}
	if got := agreedActions(st.ID, adoptTranscript(t, st.ID), syn); len(got) != 3 {
		t.Fatalf("before retraction: %+v", got)
	}
	if _, err := retractRecord(st, ev, "qiangli"); err != nil {
		t.Fatal(err)
	}
	got := agreedActions(st.ID, adoptTranscript(t, st.ID), syn)
	for _, a := range got {
		if strings.Contains(a.Text, "migration") {
			t.Fatalf("retracted action still a candidate: %+v", got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("active candidates: %+v", got)
	}
}

func adoptTranscript(t *testing.T, id string) []Event {
	t.Helper()
	ev, err := readTranscript(id)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestConcurrentRepeatedAdoptionFilesOneStory(t *testing.T) {
	st, store, acts := adoptFixture(t)
	specs := map[string]adoptSpec{acts[0].ID: {Accept: []string{"a"}}}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := adoptActions(st.ID, store, acts[:1], specs, "op", 0, true)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if all, _ := store.List(); len(all) != 1 {
		t.Fatalf("concurrent adoption filed %d stories", len(all))
	}
}

func TestInterruptedAdoptionIsCompletedNotDuplicated(t *testing.T) {
	st, store, acts := adoptFixture(t)
	// What the old Add→Save sequence left behind when the second write failed:
	// a story with the derived ID but no room link.
	left := &issue.Issue{ID: storyIDFor(st.ID, acts[0].ID), Kind: issue.KindTask, Title: acts[0].Task, Status: todo.StatusTodo, Created: time.Now().UTC(), Seq: 1}
	if _, err := store.Save(left); err != nil {
		t.Fatal(err)
	}
	specs := map[string]adoptSpec{acts[0].ID: {Accept: []string{"a"}}}
	res, err := adoptActions(st.ID, store, acts[:1], specs, "op", 0, true)
	if err != nil || res[0].Note == "" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	all, _ := store.List()
	if len(all) != 1 || extString(all[0], extAction) != acts[0].ID {
		t.Fatalf("not completed in place: %d stories", len(all))
	}
}

func TestSameBatchDependencyPreviewsWithoutCreation(t *testing.T) {
	st, store, acts := adoptFixture(t)
	specs := map[string]adoptSpec{
		acts[0].ID: {Accept: []string{"a"}, Depends: []string{acts[1].ID}},
		acts[1].ID: {Accept: []string{"b"}},
	}
	res, err := adoptActions(st.ID, store, acts[:2], specs, "op", 0, false)
	if err != nil || len(res) != 2 {
		t.Fatalf("preview: %+v %v", res, err)
	}
	if all, _ := store.List(); len(all) != 0 {
		t.Fatal("preview wrote")
	}
	// A genuine cycle is still refused.
	specs[acts[1].ID] = adoptSpec{Accept: []string{"b"}, Depends: []string{acts[0].ID}}
	if _, err := adoptActions(st.ID, store, acts[:2], specs, "op", 0, false); err == nil {
		t.Fatal("cycle accepted")
	}
}

func TestAdoptionNeedsHumanAuthority(t *testing.T) {
	st := newTestSession(t)
	_, _ = record(st, "action", "qiangli", "", "bob: write the plan")
	store := &issue.Store{Root: t.TempDir(), Sub: "todo"}
	acts, _ := RoomActions(st.ID, store)
	req := AdoptRequest{Store: store, Confirm: true, Actions: []ActionSpec{{Action: acts[0].ID, Accept: []string{"x"}}}}
	for _, by := range []string{"", "claude", "codex", "@opencode"} { // none, secretary, participants
		req.By = by
		if _, err := AdoptActions(st.ID, req); !errors.Is(err, ErrNoAuthority) {
			t.Fatalf("by %q: want ErrNoAuthority, got %v", by, err)
		}
	}
	if all, _ := store.List(); len(all) != 0 {
		t.Fatal("refused adoption wrote a story")
	}
	// A proposal alone, with no confirmation, is only a preview.
	req.Confirm, req.By = false, "claude"
	if _, err := AdoptActions(st.ID, req); err != nil {
		t.Fatal(err)
	}
	if all, _ := store.List(); len(all) != 0 {
		t.Fatal("preview wrote a story")
	}
	req.Confirm, req.By = true, "qiangli"
	if _, err := AdoptActions(st.ID, req); err != nil {
		t.Fatal(err)
	}
}

// The full workflow through the browser routes over a real room: preview, adopt,
// repeat, retract the decision, reconcile, accept, return the outcome.
func TestActionRoutesRealRoomWorkflow(t *testing.T) {
	st := newTestSession(t)
	tickingClock(t)
	t.Setenv("USER", "qiangli")
	store := &issue.Store{Root: t.TempDir(), Sub: "todo"}
	old := ActionStore
	ActionStore = func() (*issue.Store, error) { return store, nil }
	t.Cleanup(func() { ActionStore = old })
	todo.DefaultSprint, todo.PrepareSprint = nil, nil

	ev, _ := record(st, "action", "qiangli", "", "bob: write the migration plan")
	srv := serveTest(t)
	base := "/api/rooms/" + st.ID + "/actions"

	var list struct{ Actions []ActionView }
	resp, body := doJSON(t, srv, "GET", base, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}
	_ = json.Unmarshal(body, &list)
	if len(list.Actions) != 1 || list.Actions[0].Adopted {
		t.Fatalf("list: %s", body)
	}
	id := list.Actions[0].ID
	adopt := map[string]any{"actions": []ActionSpec{{Action: id, Accept: []string{"plan reviewed"}}}}

	// Preview, then adopt twice.
	adopt["confirm"] = false
	if _, b := doJSON(t, srv, "POST", base+"/adopt", adopt); !strings.Contains(string(b), `"preview":true`) {
		t.Fatalf("preview: %s", b)
	}
	if all, _ := store.List(); len(all) != 0 {
		t.Fatal("route preview wrote")
	}
	adopt["confirm"] = true
	for i := 0; i < 2; i++ {
		if r, b := doJSON(t, srv, "POST", base+"/adopt", adopt); r.StatusCode != http.StatusOK {
			t.Fatalf("adopt: %d %s", r.StatusCode, b)
		}
	}
	all, _ := store.List()
	if len(all) != 1 || all[0].Assignee != "bob" || all[0].Status != todo.StatusTodo {
		t.Fatalf("stories: %+v", all)
	}
	story := all[0]

	// An agent identity cannot confirm through the route either.
	t.Setenv("USER", "claude")
	if r, _ := doJSON(t, srv, "POST", base+"/adopt", adopt); r.StatusCode != http.StatusForbidden {
		t.Fatalf("agent adoption status %d, want 403", r.StatusCode)
	}
	t.Setenv("USER", "qiangli")

	// The decision is withdrawn: the story is surfaced for reconciliation, once.
	if _, err := retractRecord(st, ev, "qiangli"); err != nil {
		t.Fatal(err)
	}
	_, b := doJSON(t, srv, "GET", base, nil)
	if strings.Contains(string(b), id) {
		t.Fatalf("retracted action still active: %s", b)
	}
	for i, wantRequested := range []bool{true, false} {
		_, b = doJSON(t, srv, "POST", base+"/reconcile", map[string]any{"confirm": true})
		var rec struct{ Affected []ReconcileFinding }
		_ = json.Unmarshal(b, &rec)
		if len(rec.Affected) != 1 || rec.Affected[0].Requested != wantRequested || rec.Affected[0].Story != story.ID {
			t.Fatalf("reconcile %d: %s", i, b)
		}
	}
	if n := countNotes(t, st.ID, "reconcile: story "+story.ID); n != 1 {
		t.Fatalf("room got %d reconcile requests", n)
	}

	// Outcome: refused until accepted (done), then returned once with evidence.
	out := map[string]any{"story": story.ID, "evidence": []string{"commit:abc123"}}
	if r, _ := doJSON(t, srv, "POST", base+"/outcome", out); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("unverified outcome status %d", r.StatusCode)
	}
	reload, _ := store.Resolve(story.ID)
	reload.Status = todo.StatusDone
	if _, err := store.Save(reload); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if r, b := doJSON(t, srv, "POST", base+"/outcome", out); r.StatusCode != http.StatusOK {
			t.Fatalf("outcome: %d %s", r.StatusCode, b)
		}
	}
	if n := countNotes(t, st.ID, "commit:abc123"); n != 1 {
		t.Fatalf("room got %d outcome posts", n)
	}
}

func countNotes(t *testing.T, room, substr string) int {
	t.Helper()
	n := 0
	for _, e := range adoptTranscript(t, room) {
		if e.Kind == "note" && strings.Contains(e.Text, substr) {
			n++
		}
	}
	return n
}

func TestAdoptionAuthorityBindsToRoomPrincipal(t *testing.T) {
	st := newTestSession(t)
	_, _ = record(st, "action", "qiangli", "", "bob: write the plan")
	store := &issue.Store{Root: t.TempDir(), Sub: "todo"}
	acts, _ := RoomActions(st.ID, store)
	req := AdoptRequest{Store: store, Confirm: true, Actions: []ActionSpec{{Action: acts[0].ID, Accept: []string{"x"}}}}
	// An unknown, spoofed "human" name and an observer are not the room's person.
	st.Observers = []string{"watcher"}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	for _, by := range []string{"mallory", "root", "the-human", "watcher"} {
		req.By = by
		if _, err := AdoptActions(st.ID, req); !errors.Is(err, ErrNoAuthority) {
			t.Fatalf("by %q: want ErrNoAuthority, got %v", by, err)
		}
	}
	if all, _ := store.List(); len(all) != 0 {
		t.Fatal("refused adoption wrote a story")
	}
	req.By = "qiangli"
	if _, err := AdoptActions(st.ID, req); err != nil {
		t.Fatal(err)
	}
	// The CLI cannot borrow the room's human through --by.
	clearAgentEnv(t)
	t.Setenv("USER", "mallory")
	if _, err := callerPrincipal("qiangli"); !errors.Is(err, ErrNoAuthority) {
		t.Fatalf("spoofed --by accepted: %v", err)
	}
	if p, err := callerPrincipal(""); err != nil || p != "mallory" {
		t.Fatalf("principal %q %v", p, err)
	}
	// The outsider as the real principal is refused by the room check.
	req.By = "mallory"
	if _, err := AdoptActions(st.ID, req); !errors.Is(err, ErrNoAuthority) {
		t.Fatalf("outsider: %v", err)
	}
}

func TestOutcomeRetryAfterPartialFailureDoesNotDuplicate(t *testing.T) {
	st := newTestSession(t)
	tickingClock(t)
	_, _ = record(st, "action", "qiangli", "", "bob: write the plan")
	store := &issue.Store{Root: t.TempDir(), Sub: "todo"}
	todo.DefaultSprint, todo.PrepareSprint = nil, nil
	acts, _ := RoomActions(st.ID, store)
	if _, err := AdoptActions(st.ID, AdoptRequest{Store: store, Confirm: true, By: "qiangli", Actions: []ActionSpec{{Action: acts[0].ID, Accept: []string{"x"}}}}); err != nil {
		t.Fatal(err)
	}
	all, _ := store.List()
	story := all[0]
	story.Status = todo.StatusDone
	if _, err := store.Save(story); err != nil {
		t.Fatal(err)
	}
	req := OutcomeRequest{Story: story.ID, Evidence: []string{"commit:abc123"}, By: "qiangli", Store: store}
	// The room post lands, then the story mark fails.
	afterNotify = func() error { return errors.New("crash") }
	if _, _, err := ReturnOutcome(st.ID, req); err == nil {
		t.Fatal("fault not injected")
	}
	afterNotify = nil
	t.Cleanup(func() { afterNotify = nil })
	for i := 0; i < 2; i++ {
		if _, _, err := ReturnOutcome(st.ID, req); err != nil {
			t.Fatal(err)
		}
	}
	if n := countNotes(t, st.ID, "commit:abc123"); n != 1 {
		t.Fatalf("room got %d outcome posts", n)
	}
	reload, _ := store.Resolve(story.ID)
	if extString(reload, extReportedAt) == "" {
		t.Fatal("retry did not mark the story reported")
	}
}

func TestReconcileCrashBetweenPostAndMarkStillTellsRoomOnce(t *testing.T) {
	st := newTestSession(t)
	tickingClock(t)
	ev, _ := record(st, "action", "qiangli", "", "bob: write the plan")
	store := &issue.Store{Root: t.TempDir(), Sub: "todo"}
	todo.DefaultSprint, todo.PrepareSprint = nil, nil
	acts, _ := RoomActions(st.ID, store)
	if _, err := AdoptActions(st.ID, AdoptRequest{Store: store, Confirm: true, By: "qiangli", Actions: []ActionSpec{{Action: acts[0].ID, Accept: []string{"x"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := retractRecord(st, ev, "qiangli"); err != nil {
		t.Fatal(err)
	}
	rr := ReconcileRequest{Confirm: true, By: "qiangli", Store: store}
	afterNotify = func() error { return errors.New("crash") }
	t.Cleanup(func() { afterNotify = nil })
	if _, err := ReconcileActions(st.ID, rr); err == nil {
		t.Fatal("fault not injected")
	}
	afterNotify = nil
	// Story is still unmarked, so the retry reports it as requested and marks it
	// without a second room notice; a further repeat is quiet.
	for i, want := range []bool{true, false} {
		f, err := ReconcileActions(st.ID, rr)
		if err != nil || len(f) != 1 || f[0].Requested != want {
			t.Fatalf("retry %d: %+v %v", i, f, err)
		}
	}
	if n := countNotes(t, st.ID, "reconcile: story"); n != 1 {
		t.Fatalf("room got %d reconcile notices", n)
	}
}

// clearAgentEnv removes every marker that makes the process an agent session, so
// a test can stand in for a bare human shell even when run under a harness.
func clearAgentEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"BASHY_PRINCIPAL", "BASHY_AGENT_ID", "BASHY_AGENT", "WEAVE_AGENT", "AGENT", "AI_AGENT",
		"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_SANDBOX", "CODEX_THREAD_ID", "GEMINI_CLI", "GOOSE_TERMINAL",
		"OPENCODE_CLIENT", "CLINE_ACTIVE", "HERMES_AGENT", "KIMI_CODE", "OPENCLAW"} {
		t.Setenv(k, "")
	}
}

// Agents share the operator's OS account: a worker launched with BASHY_AGENT is
// not the room's human even though $USER names them and --by is omitted.
func TestAgentSessionUnderSharedAccountHasNoAuthority(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("USER", "room-human")
	if p, err := callerPrincipal(""); err != nil || p != "room-human" {
		t.Fatalf("human shell: %q %v", p, err)
	}
	t.Setenv("BASHY_AGENT", "worker")
	if _, err := callerPrincipal(""); !errors.Is(err, ErrNoAuthority) {
		t.Fatalf("agent with USER=room-human accepted: %v", err)
	}
	if _, err := callerPrincipal("room-human"); !errors.Is(err, ErrNoAuthority) {
		t.Fatalf("agent with matching --by accepted: %v", err)
	}
}
