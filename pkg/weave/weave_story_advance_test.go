package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/issue"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// advanceFixture is one recurring sprint tracking one repo store, plus helpers
// to read the items back. Everything is private to the test: the sprint board
// and the repo both live under t.TempDir().
type advanceFixture struct {
	t     *testing.T
	root  string // the tracked repo root
	store *issue.Store
	id    int64
}

func newAdvanceFixture(t *testing.T) *advanceFixture {
	t.Helper()
	boardDir := t.TempDir()
	t.Setenv("BASHY_SPRINT_DIR", boardDir)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, todopkg.RepoSub), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &advanceFixture{t: t, root: root, store: todopkg.RepoStore(root), id: 7}

	now := time.Now().UTC()
	stopped := now.Add(-time.Hour)
	if err := withWeaveQueueLock(boardDir, func(q *weaveQueue) error {
		q.Stories = append(q.Stories, &weaveStory{
			ID: f.id, Title: "release train", Column: "doing", Owner: "conductor-a",
			StoryRoots: []string{root},
			Boxes: []weaveStoryBox{{
				StartedAt: now.Add(-3 * time.Hour), Cutoff: now, StoppedAt: &stopped,
				Planned: 2 * time.Hour, GateRan: true, GatePassed: true, GateCmd: "make test",
			}},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *advanceFixture) add(title, recurring, status string) *issue.Issue {
	f.t.Helper()
	it, err := todopkg.Add(f.store, title, "", "p1", nil, recurring, "")
	if err != nil {
		f.t.Fatal(err)
	}
	it.Sprint = f.id
	it.Status = status
	if _, err := f.store.Save(it); err != nil {
		f.t.Fatal(err)
	}
	return it
}

func (f *advanceFixture) reload(id string) *issue.Issue {
	f.t.Helper()
	it, err := todopkg.ResolveRef(f.store, id)
	if err != nil {
		f.t.Fatalf("reload %s: %v", id, err)
	}
	return it
}

func (f *advanceFixture) records() []*issue.Issue {
	f.t.Helper()
	all, err := f.store.List()
	if err != nil {
		f.t.Fatal(err)
	}
	var out []*issue.Issue
	for _, it := range all {
		if hasLabel(it, cycleLabel) {
			out = append(out, it)
		}
	}
	return out
}

func (f *advanceFixture) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewSprintCmd()
	out := &strings.Builder{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(append([]string{"advance"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

// The point of the whole feature: crossing the boundary files what happened
// and hands the same stories back, ready to run again.
func TestAdvanceFilesTheCycleAndResetsTheProcedure(t *testing.T) {
	f := newAdvanceFixture(t)
	step := f.add("tag and push", todopkg.CadenceSprint, todopkg.StatusDone)
	oneOff := f.add("note the schema change", "", todopkg.StatusDone)

	out, err := f.run(t, "7", "--set", "version=v0.9.5")
	if err != nil {
		t.Fatalf("advance failed: %v\n%s", err, out)
	}

	if got := f.reload(step.ID).Status; got != todopkg.StatusTodo {
		t.Errorf("recurring story status = %q, want %q — the procedure must be runnable again", got, todopkg.StatusTodo)
	}
	if got := f.reload(oneOff.ID).Status; got != todopkg.StatusDone {
		t.Errorf("one-off story status = %q, want it left closed", got)
	}

	recs := f.records()
	if len(recs) != 1 {
		t.Fatalf("cycle records = %d, want 1", len(recs))
	}
	m := parseManifest(recs[0].Body)
	if m["version"] != "v0.9.5" {
		t.Errorf("manifest version = %q, want v0.9.5", m["version"])
	}
	if m[cycleKey] != "1" {
		t.Errorf("manifest cycle = %q, want 1", m[cycleKey])
	}
	if !strings.Contains(recs[0].Body, "PASSED") {
		t.Errorf("cycle record lost the gate verdict:\n%s", recs[0].Body)
	}
}

// Iteration N+1 must be able to DELEGATE. `weave add --from-todo` refuses any
// item still carrying a run id, so a reset that leaves Weave set produces a
// procedure that can never be worked a second time.
func TestAdvanceClearsTheStaleRunLinkSoTheNextIterationCanDelegate(t *testing.T) {
	f := newAdvanceFixture(t)
	step := f.add("run the lanes", todopkg.CadenceSprint, todopkg.StatusDone)
	step.Weave = 42
	if _, err := f.store.Save(step); err != nil {
		t.Fatal(err)
	}

	if out, err := f.run(t, "7"); err != nil {
		t.Fatalf("advance failed: %v\n%s", err, out)
	}
	if got := f.reload(step.ID).Weave; got != 0 {
		t.Errorf("Weave = %d, want 0 — a stale run link blocks `weave add --from-todo`", got)
	}
}

// A standing assignment survives the boundary, so routine delegation costs
// nothing and only an exception needs a decision.
func TestAdvanceKeepsAStandingOwnerAndReturnsTheStoryToAssigned(t *testing.T) {
	f := newAdvanceFixture(t)
	step := f.add("promote", todopkg.CadenceSprint, todopkg.StatusDone)
	step.Assignee = "worker-a"
	if _, err := f.store.Save(step); err != nil {
		t.Fatal(err)
	}

	if out, err := f.run(t, "7"); err != nil {
		t.Fatalf("advance failed: %v\n%s", err, out)
	}
	got := f.reload(step.ID)
	if got.Assignee != "worker-a" || got.Status != todopkg.StatusAssigned {
		t.Errorf("owner=%q status=%q, want worker-a/%s", got.Assignee, got.Status, todopkg.StatusAssigned)
	}
}

// Advancing over a running box would file an iteration nobody judged.
func TestAdvanceRefusesWhileTheBoxIsStillRunning(t *testing.T) {
	f := newAdvanceFixture(t)
	f.add("step", todopkg.CadenceSprint, todopkg.StatusDone)

	dir := os.Getenv("BASHY_SPRINT_DIR")
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		s := findWeaveStory(q, 7)
		s.Boxes = append(s.Boxes, weaveStoryBox{
			StartedAt: time.Now().UTC(), Cutoff: time.Now().UTC().Add(time.Hour), Planned: time.Hour,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	out, err := f.run(t, "7")
	if err == nil {
		t.Fatalf("advance must refuse a running box, got success:\n%s", out)
	}
	if !strings.Contains(out, "sprint stop") {
		t.Errorf("refusal must name the fix (`sprint stop`):\n%s", out)
	}
	if len(f.records()) != 0 {
		t.Error("a refused advance wrote a cycle record")
	}
}

// An unfinished one-off carried forward would be filed under the next
// iteration's name. It must be finished or explicitly dropped.
func TestAdvanceRefusesAnOpenOneOffAndDropRecordsTheDisposition(t *testing.T) {
	f := newAdvanceFixture(t)
	f.add("step", todopkg.CadenceSprint, todopkg.StatusDone)
	stray := f.add("investigate the flake", "", todopkg.StatusDoing)

	out, err := f.run(t, "7")
	if err == nil {
		t.Fatalf("advance must refuse an open one-off, got success:\n%s", out)
	}
	if !strings.Contains(out, "investigate the flake") {
		t.Errorf("refusal must name the blocking story:\n%s", out)
	}

	if out, err := f.run(t, "7", "--drop", stray.ID); err != nil {
		t.Fatalf("--drop should unblock: %v\n%s", err, out)
	}
	got := f.reload(stray.ID)
	if got.Status != todopkg.StatusDone || got.Resolution != "obsolete" {
		t.Errorf("dropped story = %q/%q, want done/obsolete — never a silent `fixed`", got.Status, got.Resolution)
	}
}

// The script is where all domain knowledge lives; bashy only runs it and
// records what it printed. It must see the previous iteration's values.
func TestAdvanceScriptReceivesThePreviousValuesAndItsOutputBecomesTheManifest(t *testing.T) {
	f := newAdvanceFixture(t)
	f.add("step", todopkg.CadenceSprint, todopkg.StatusDone)

	scriptDir := filepath.Join(f.root, advanceScriptRel)
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "echo \"n=$((${SPRINT_PREV_N:-0} + 1))\"\necho \"cycleseen=$SPRINT_CYCLE\"\n"
	if err := os.WriteFile(filepath.Join(scriptDir, "7.advance"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	for want := 1; want <= 2; want++ {
		if out, err := f.run(t, "7"); err != nil {
			t.Fatalf("advance %d failed: %v\n%s", want, err, out)
		}
	}

	recs := f.records()
	if len(recs) != 2 {
		t.Fatalf("cycle records = %d, want 2", len(recs))
	}
	var second map[string]string
	for _, r := range recs {
		if m := parseManifest(r.Body); m[cycleKey] == "2" {
			second = m
		}
	}
	if second == nil {
		t.Fatalf("no cycle-2 record among %d", len(recs))
	}
	if second["n"] != "2" {
		t.Errorf("n = %q, want 2 — the script did not see SPRINT_PREV_N", second["n"])
	}
	if second["cycleseen"] != "2" {
		t.Errorf("cycleseen = %q, want 2 — the script did not see SPRINT_CYCLE", second["cycleseen"])
	}
}

// --set beats the script, so an operator can always name a value by hand.
// The cycle number is the exception: the mechanism owns its own index.
func TestSetOverridesTheScriptButNeverTheCycleNumber(t *testing.T) {
	f := newAdvanceFixture(t)
	f.add("step", todopkg.CadenceSprint, todopkg.StatusDone)

	if out, err := f.run(t, "7", "--exec", "echo v=fromscript", "--set", "v=fromflag", "--set", cycleKey+"=99"); err != nil {
		t.Fatalf("advance failed: %v\n%s", err, out)
	}
	m := parseManifest(f.records()[0].Body)
	if m["v"] != "fromflag" {
		t.Errorf("v = %q, want fromflag", m["v"])
	}
	if m[cycleKey] != "1" {
		t.Errorf("cycle = %q, want 1 — a record must not be able to lie about which iteration it is", m[cycleKey])
	}
}

// A failing script must stop the boundary rather than file a cycle with no
// values: a record that silently lost its manifest is worse than no record.
func TestAdvanceStopsWhenTheScriptFails(t *testing.T) {
	f := newAdvanceFixture(t)
	step := f.add("step", todopkg.CadenceSprint, todopkg.StatusDone)

	out, err := f.run(t, "7", "--exec", "exit 3")
	if err == nil {
		t.Fatalf("a failing script must stop advance:\n%s", out)
	}
	if len(f.records()) != 0 {
		t.Error("a failed advance wrote a cycle record")
	}
	if got := f.reload(step.ID).Status; got != todopkg.StatusDone {
		t.Errorf("a failed advance reset the stories anyway (status %q)", got)
	}
}

func TestAdvanceDryRunWritesNothing(t *testing.T) {
	f := newAdvanceFixture(t)
	step := f.add("step", todopkg.CadenceSprint, todopkg.StatusDone)

	out, err := f.run(t, "7", "--dry-run", "--set", "v=1")
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "would reset") {
		t.Errorf("dry run must say it changed nothing:\n%s", out)
	}
	if len(f.records()) != 0 {
		t.Error("dry run wrote a cycle record")
	}
	if got := f.reload(step.ID).Status; got != todopkg.StatusDone {
		t.Errorf("dry run reset a story (status %q)", got)
	}
}

// An ordinary sprint is not a recurring one, and advance must say so instead
// of silently doing nothing that looks like success.
func TestAdvanceRefusesASprintWithNoRecurringStories(t *testing.T) {
	f := newAdvanceFixture(t)
	f.add("just a task", "", todopkg.StatusDone)

	out, err := f.run(t, "7")
	if err == nil {
		t.Fatalf("advance must refuse a non-recurring sprint:\n%s", out)
	}
	if !strings.Contains(out, "--recurring") {
		t.Errorf("refusal must name how to make it recurring:\n%s", out)
	}
}
