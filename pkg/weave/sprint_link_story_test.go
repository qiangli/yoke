package weave

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/ladder"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

type sprintLinkStoryFixture struct {
	home, storyRoot, runRepo, tag, board string
	story                                *issue.Issue
	now                                  time.Time
}

// newSprintLinkStoryFixture builds an isolated host: a sprint whose story
// lives in storyRoot and a weave queue for runRepo holding the given runs. A
// worker has claimed and submitted the story itself, so nothing but the link
// records which run delivered it.
func newSprintLinkStoryFixture(t *testing.T, crossRepo bool, runs ...*weaveItem) *sprintLinkStoryFixture {
	t.Helper()
	f := &sprintLinkStoryFixture{home: t.TempDir(), storyRoot: t.TempDir(), now: time.Now().UTC()}
	f.runRepo = f.storyRoot
	if crossRepo {
		f.runRepo = t.TempDir()
	}
	t.Setenv("HOME", f.home)
	t.Setenv("USERPROFILE", f.home)
	t.Setenv("BASHY_HOME", filepath.Join(f.home, ".bashy"))
	cat := pinFleetWith(t)
	if err := cat.SaveAgent(fleet.Agent{Name: "agent-a", Tool: "tool-a", Model: "model-a"}); err != nil {
		t.Fatal(err)
	}
	f.board = filepath.Join(f.home, "sprint")
	t.Setenv("BASHY_SPRINT_DIR", f.board)
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(f.home, "room"))
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("WEAVE_CONDUCTOR", "manager")
	t.Setenv("BASHY_LADDER_SEASON", "4")
	st := todopkg.RepoStore(f.storyRoot)
	it, err := todopkg.Add(st, "deliver", "", "p0", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	it.Sprint = 1
	it.Assignee = "worker"
	it.Status = todopkg.StatusAssigned
	if _, err = st.Save(it); err != nil {
		t.Fatal(err)
	}
	f.story = it
	f.tag = filepath.Base(f.runRepo) + "-test"
	if err = saveWeaveQueue(filepath.Join(weaveStateRoot(f.home), f.tag), &weaveQueue{Root: f.runRepo, Items: runs}); err != nil {
		t.Fatal(err)
	}
	s := &weaveStory{ID: 1, Title: "neutral", Column: "doing", Owner: "manager", Lease: &weaveStoryLease{Holder: "manager", At: f.now}, StoryRoots: []string{f.storyRoot}, Created: f.now}
	weaveStoryAppend(s, "worker", "decision", "worker submitted story "+shortSprintStoryID(it.ID)+" for merge/closure")
	if err = saveWeaveQueue(f.board, &weaveQueue{NextStoryID: 2, Stories: []*weaveStory{s}}); err != nil {
		t.Fatal(err)
	}
	return f
}

func sprintLinkStoryRun(id int64, created time.Time) *weaveItem {
	cap, _ := weavePointRuntimeCap(1)
	return &weaveItem{ID: id, Points: 1, State: "done", Owner: "agent-a", Tool: "tool-a", Created: created, StartedAt: created.Add(-time.Minute), FinishedAt: created,
		LaunchSpec: &weaveLaunchSpec{Tool: "tool-a", Model: "model-a", Agent: "agent-a", MaxRuntime: cap}}
}

func (f *sprintLinkStoryFixture) run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewSprintCmd()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), stderr.String(), err
}

func (f *sprintLinkStoryFixture) link(t *testing.T, task int64, extra ...string) (string, string, error) {
	t.Helper()
	return f.run(t, append([]string{"link", "1", "--repo", filepath.Base(f.runRepo), "--queue", f.tag, "--task", strconv.FormatInt(task, 10)}, extra...)...)
}

func (f *sprintLinkStoryFixture) accept(t *testing.T) string {
	t.Helper()
	out, stderr, err := f.run(t, "accept", "1", f.story.ID, "--repo", f.storyRoot)
	if err != nil {
		t.Fatalf("accept: %v out=%s stderr=%s", err, out, stderr)
	}
	return out
}

func (f *sprintLinkStoryFixture) queue(t *testing.T) *weaveQueue {
	t.Helper()
	q, err := loadWeaveQueue(filepath.Join(weaveStateRoot(f.home), f.tag))
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func (f *sprintLinkStoryFixture) sprint(t *testing.T) *weaveStory {
	t.Helper()
	q, err := loadWeaveQueue(f.board)
	if err != nil {
		t.Fatal(err)
	}
	return findWeaveStory(q, 1)
}

func sprintLinkStoryEvents(t *testing.T) []ladder.Event {
	t.Helper()
	if _, err := os.Stat(ladder.DefaultStorePath()); os.IsNotExist(err) {
		return nil
	}
	store, err := ladder.OpenStore("")
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestSprintLinkStoryRatesClaimedDelivery(t *testing.T) {
	for _, crossRepo := range []bool{false, true} {
		name := "same-repo"
		if crossRepo {
			name = "cross-repo"
		}
		t.Run(name, func(t *testing.T) {
			created := time.Now().UTC().Add(-time.Hour)
			f := newSprintLinkStoryFixture(t, crossRepo, sprintLinkStoryRun(7, created))
			out, stderr, err := f.link(t, 7, "--story", f.story.ID)
			if err != nil || !strings.Contains(out, "for story "+shortSprintStoryID(f.story.ID)) {
				t.Fatalf("link: %v out=%s stderr=%s", err, out, stderr)
			}
			if got := f.queue(t).Items[0].Register; got != f.story.ID {
				t.Fatalf("run register = %q, want %q", got, f.story.ID)
			}
			if links := f.sprint(t).Runs; len(links) != 1 || links[0].Story != f.story.ID || !links[0].Born.Equal(created) {
				t.Fatalf("links = %+v", links)
			}
			out, _, err = f.link(t, 7, "--story", f.story.ID)
			if err != nil || !strings.Contains(out, "already links") || len(f.sprint(t).Runs) != 1 {
				t.Fatalf("relink not idempotent: %v %s %+v", err, out, f.sprint(t).Runs)
			}
			out = f.accept(t)
			if !strings.Contains(out, "ladder: delivery recorded") {
				t.Fatalf("accept did not rate: %s", out)
			}
			events := sprintLinkStoryEvents(t)
			if len(events) != 1 {
				t.Fatalf("events = %+v", events)
			}
			ev := events[0]
			if ev.Agent != "tool-a:model-a" || ev.Points != 1 || ev.CapsUsed.WallSeconds != 60 || ev.Sprint != 1 || ev.Story != f.story.ID || !strings.Contains(ev.ID, ":run:7:") {
				t.Fatalf("delivery = %+v", ev)
			}
		})
	}
}

func TestSprintLinkStoryAmbiguityFailsClosed(t *testing.T) {
	created := time.Now().UTC().Add(-time.Hour)
	a, b := sprintLinkStoryRun(7, created), sprintLinkStoryRun(8, created.Add(time.Second))
	b.State = "running"
	f := newSprintLinkStoryFixture(t, true, a, b)
	// A finished earlier attempt does not block a new live one.
	for _, task := range []int64{7, 8} {
		if out, stderr, err := f.link(t, task, "--story", f.story.ID); err != nil {
			t.Fatalf("link #%d: %v %s %s", task, err, out, stderr)
		}
	}
	// A second live attempt at the same story is refused at link time.
	b2 := sprintLinkStoryRun(9, created.Add(2*time.Second))
	b2.State = "running"
	q := f.queue(t)
	q.Items = append(q.Items, b2)
	if err := saveWeaveQueue(filepath.Join(weaveStateRoot(f.home), f.tag), q); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := f.link(t, 9, "--story", f.story.ID)
	if !sprintLinkExit(err, weavecli.ExitInvalidArg) || !strings.Contains(stderr, "live linked run") {
		t.Fatalf("second live attempt not refused: %v %s", err, stderr)
	}
	if got := findWeaveItem(f.queue(t), 9).Register; got != "" {
		t.Fatalf("refused link mutated run: register=%q", got)
	}
	// Two linked attempts make the rating a guess: accept closes the story
	// unrated.
	out := f.accept(t)
	if !strings.Contains(out, "multiple linked attempts") {
		t.Fatalf("ambiguous accept rated: %s", out)
	}
	if events := sprintLinkStoryEvents(t); len(events) != 0 {
		t.Fatalf("ambiguous accept wrote ledger: %+v", events)
	}
}

func TestSprintLinkStoryRefusesOtherRegistration(t *testing.T) {
	run := sprintLinkStoryRun(7, time.Now().UTC().Add(-time.Hour))
	run.Register = "other-story"
	f := newSprintLinkStoryFixture(t, true, run)
	_, stderr, err := f.link(t, 7, "--story", f.story.ID)
	if !sprintLinkExit(err, weavecli.ExitInvalidArg) || !strings.Contains(stderr, "registered to story other-story") {
		t.Fatalf("link over another registration: %v %s", err, stderr)
	}
	if got := f.queue(t).Items[0].Register; got != "other-story" {
		t.Fatalf("register mutated: %q", got)
	}
	if links := f.sprint(t).Runs; len(links) != 0 {
		t.Fatalf("refused link recorded: %+v", links)
	}
	// A closed story is refused the same way.
	run.Register = ""
	if err := saveWeaveQueue(filepath.Join(weaveStateRoot(f.home), f.tag), &weaveQueue{Root: f.runRepo, Items: []*weaveItem{run}}); err != nil {
		t.Fatal(err)
	}
	f.story.Status = todopkg.StatusDone
	if _, err := todopkg.RepoStore(f.storyRoot).Save(f.story); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err = f.link(t, 7, "--story", f.story.ID); !sprintLinkExit(err, weavecli.ExitInvalidArg) || !strings.Contains(stderr, "already closed") {
		t.Fatalf("closed story not refused: %v %s", err, stderr)
	}
	if got := f.queue(t).Items[0].Register; got != "" {
		t.Fatalf("register mutated for closed story: %q", got)
	}
}

func TestSprintLinkWithoutStoryKeepsNotice(t *testing.T) {
	f := newSprintLinkStoryFixture(t, true, sprintLinkStoryRun(7, time.Now().UTC().Add(-time.Hour)))
	if out, stderr, err := f.link(t, 7); err != nil {
		t.Fatalf("link: %v %s %s", err, out, stderr)
	}
	if got := f.queue(t).Items[0].Register; got != "" {
		t.Fatalf("plain link registered run: %q", got)
	}
	out := f.accept(t)
	if !strings.Contains(out, "story has no linked run; link one with `sprint link 1 --repo R --task T --story "+shortSprintStoryID(f.story.ID)+"`") {
		t.Fatalf("missing no-run notice: %s", out)
	}
	if events := sprintLinkStoryEvents(t); len(events) != 0 {
		t.Fatalf("unlinked accept wrote ledger: %+v", events)
	}
}

func TestSprintUnlinkReleasesStoryRegistration(t *testing.T) {
	f := newSprintLinkStoryFixture(t, true, sprintLinkStoryRun(7, time.Now().UTC().Add(-time.Hour)))
	if out, stderr, err := f.link(t, 7, "--story", f.story.ID); err != nil {
		t.Fatalf("link: %v %s %s", err, out, stderr)
	}
	out, stderr, err := f.run(t, "unlink", "1", "--repo", filepath.Base(f.runRepo), "--task", "7")
	if err != nil || !strings.Contains(out, "released its registration") {
		t.Fatalf("unlink: %v %s %s", err, out, stderr)
	}
	if got := f.queue(t).Items[0].Register; got != "" {
		t.Fatalf("unlink kept registration: %q", got)
	}
}

func sprintLinkExit(err error, want int) bool {
	var code *exitCodeError
	return errors.As(err, &code) && code.code == want
}
