package hostsrc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/room"
	"github.com/qiangli/yoke/pkg/watch"
)

// THE PRESET INTEGRATION GATE.
//
// This is the test that says the shipped `sprint` preset — the file, as it
// ships, parsed and built through the ordinary rule-file path — produces
// EXACTLY the expected events against a fixture card thread and a fixture
// inbox. It is the check the hand-written poll script never had: each of its
// four failures was a rule that looked right in the file and watched nothing in
// practice.

// useFixtures points the registered source factories at in-memory stores, so
// the preset under test is wired exactly as it is in production.
func useFixtures(t *testing.T, board Board, read Timeline) {
	t.Helper()
	prevBoard, prevRead := DefaultBoard, DefaultTimeline
	DefaultBoard, DefaultTimeline = board, read
	t.Cleanup(func() { DefaultBoard, DefaultTimeline = prevBoard, prevRead })
}

type alertRow struct{ rule, reason, locator string }

func rows(alerts []watch.Alert) []alertRow {
	out := make([]alertRow, 0, len(alerts))
	for _, a := range alerts {
		out = append(out, alertRow{a.Rule, a.Reason, a.Locator})
	}
	return out
}

func (r alertRow) String() string { return fmt.Sprintf("%s/%s/%s", r.rule, r.reason, r.locator) }

func showRows(got []alertRow) string {
	parts := make([]string, 0, len(got))
	for _, r := range got {
		parts = append(parts, r.String())
	}
	return strings.Join(parts, " ")
}

func TestSprintPresetAgainstFixtureCardAndInbox(t *testing.T) {
	board := newFakeBoard()
	// Older than the preset's 2h backfill: history, not news.
	board.add(bus.Post{ID: "old", From: "conductor", To: "operator", Body: "last night", At: ts(-3 * time.Hour)})
	board.add(bus.Post{ID: "ask", From: "manager", To: "operator", Topic: "sprint",
		Body: "decision needed: extend the window?", At: ts(-2 * time.Minute)})

	timeline := []room.Event{
		sprintEvent(1, -3*time.Hour, "sprint:#480", "stage", "manager", "created"),
		sprintEvent(2, -10*time.Minute, "sprint:#480", "stage", "manager", "started"),
		sprintEvent(3, -5*time.Minute, "sprint:#480", "decision", "manager", "accepted story cd49"),
		sprintEvent(4, -4*time.Minute, "sprint:#472", "progress", "worker", "checkpoint"),
	}
	useFixtures(t, board, func(int) ([]room.Event, error) { return timeline, nil })

	preset, err := watch.LoadPreset("sprint")
	if err != nil {
		t.Fatalf("preset: %v", err)
	}
	plan, err := preset.File.Build(watch.BuildOptions{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if plan.Interval != time.Minute || plan.Renotify != time.Hour {
		t.Fatalf("plan timings = %v / %v", plan.Interval, plan.Renotify)
	}

	var got []watch.Alert
	inbox := &InboxSink{BoardAPI: board, From: "watcher", To: []string{"conductor"}, Topic: "watch"}
	now := at0
	engine := watch.New(watch.Config{
		Sources:  plan.Sources,
		Rules:    plan.Rules,
		Sinks:    []watch.Sink{watch.FuncSink(func(a watch.Alert) error { got = append(got, a); return nil }), inbox},
		Interval: plan.Interval,
		Renotify: plan.Renotify,
		// 50 cycles of one minute: long enough for the preset's 45m idle
		// window to expire on a thread that stopped moving.
		MaxCycles: 50,
		Now:       func() time.Time { return now },
		Sleep:     func(_ context.Context, d time.Duration) bool { now = now.Add(d); return true },
	})
	if code := engine.Run(context.Background()); code != watch.ExitOK {
		t.Fatalf("code = %d", code)
	}

	want := []alertRow{
		// The two thread entries inside the backfill, and nothing from before it.
		{"stage", watch.ReasonMatch, "sprint:480"},
		{"judgement", watch.ReasonMatch, "sprint:480"},
		// The message to the operator — the source the hand-written script did
		// not poll at all, which is why a sprint sat idle for most of a night.
		{"operator-mail", watch.ReasonMatch, "mb:ask"},
		// Both threads then go quiet, 45 minutes after their last entry.
		{"sprint-idle", watch.ReasonIdle, "sprint:480"},
		{"sprint-idle", watch.ReasonIdle, "sprint:472"},
	}
	if g := rows(got); len(g) != len(want) || showRows(g) != showRows(want) {
		t.Fatalf("alerts = %s\nwant       = %s", showRows(g), showRows(want))
	}

	// The 45m windows are measured from each thread's own last entry, not from
	// the start of the run.
	if d := got[3].At.Sub(at0); d != 40*time.Minute {
		t.Fatalf("sprint:480 idle fired at +%v, want +40m (last entry was 5m before the run)", d)
	}
	if d := got[4].At.Sub(at0); d != 41*time.Minute {
		t.Fatalf("sprint:472 idle fired at +%v, want +41m", d)
	}

	// Every alert reached the fixture inbox once, and nothing else did.
	if len(board.sent) != len(want) {
		t.Fatalf("inbox got %d posts, want %d: %+v", len(board.sent), len(want), board.sent)
	}
	for i, p := range board.sent {
		if p.From != "watcher" || p.To != "conductor" || p.Topic != "watch" {
			t.Fatalf("post %d = %+v", i, p)
		}
		if !strings.Contains(p.Body, want[i].rule) {
			t.Fatalf("post %d body %q does not name %q", i, p.Body, want[i].rule)
		}
	}
}

func TestAgentReplyPresetTracksTheLadder(t *testing.T) {
	board := newFakeBoard()
	useFixtures(t, board, func(int) ([]room.Event, error) { return nil, nil })

	preset, err := watch.LoadPreset("agent-reply")
	if err != nil {
		t.Fatalf("preset: %v", err)
	}
	plan, err := preset.File.Build(watch.BuildOptions{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Two directed messages. One is read and answered; the other is read and
	// ignored, which is the case a read receipt alone would call healthy.
	board.add(bus.Post{ID: "q1", From: "conductor", To: "worker-a", Topic: "story-1",
		Body: "status?", At: ts(0)})
	board.add(bus.Post{ID: "q2", From: "conductor", To: "worker-b", Topic: "story-2",
		Body: "status?", At: ts(0)})

	var got []watch.Alert
	now := at0
	cycle := 0
	engine := watch.New(watch.Config{
		Sources:  plan.Sources,
		Rules:    plan.Rules,
		Sinks:    []watch.Sink{watch.FuncSink(func(a watch.Alert) error { got = append(got, a); return nil })},
		Interval: plan.Interval,
		Renotify: plan.Renotify,
		// 20 cycles of a minute, past the preset's 15m deadline.
		MaxCycles: 20,
		Now:       func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) bool {
			cycle++
			switch cycle {
			case 1:
				// Both recipients read their message.
				board.read("worker-a", 1)
				board.read("worker-b", 2)
			case 5:
				board.add(bus.Post{ID: "a1", From: "worker-a", To: "conductor", Topic: "story-1",
					Body: "done", At: ts(5 * time.Minute)})
			}
			now = now.Add(d)
			return true
		},
	})
	if code := engine.Run(context.Background()); code != watch.ExitOK {
		t.Fatalf("code = %d", code)
	}

	want := []alertRow{
		{"agent-reply", watch.ReasonResponded, "mb:q1"},
		{"agent-reply", watch.ReasonUnanswered, "mb:q2"},
	}
	if g := rows(got); showRows(g) != showRows(want) {
		t.Fatalf("alerts = %s\nwant       = %s", showRows(g), showRows(want))
	}
	if got[0].Fields["level"] != "replied" || got[0].Fields["latency"] != "5m0s" {
		t.Fatalf("responded fields = %v", got[0].Fields)
	}
	// Read and still silent: the alert has to say so, or the follow-up is wrong.
	if got[1].Fields["level"] != "read" || got[1].Fields["waited"] != "15m0s" {
		t.Fatalf("unanswered fields = %v", got[1].Fields)
	}
}

func TestShippedPresetsAllParseAndBuild(t *testing.T) {
	useFixtures(t, newFakeBoard(), func(int) ([]room.Event, error) { return nil, nil })
	names := watch.PresetNames()
	for _, want := range []string{"agent-reply", "idle", "sprint"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("preset %q missing from %v", want, names)
		}
	}
	for _, name := range names {
		preset, err := watch.LoadPreset(name)
		if err != nil {
			t.Fatalf("preset %q: %v", name, err)
		}
		if preset.File.Description == "" {
			t.Fatalf("preset %q has no description — a preset nobody can read is not a rod", name)
		}
		if _, err := preset.File.Build(watch.BuildOptions{}); err != nil {
			t.Fatalf("preset %q build: %v", name, err)
		}
	}
}

// THE NARROWING GATE.
//
// `--preset sprint --source sprint-thread:472` is a request to watch ONE card.
// Before the narrowing, the preset's own unfiltered sprint-thread source was
// kept beside the explicit one, so the watcher reported every card on the host
// — the narrower request was silently widened.
func TestExplicitSourceNarrowsThePresetToOneCard(t *testing.T) {
	t.Setenv("BASHY_WATCH_DIR", t.TempDir())
	board := newFakeBoard()
	timeline := []room.Event{
		sprintEvent(1, -30*time.Minute, "sprint:#450", "stage", "manager", "started"),
		sprintEvent(2, -20*time.Minute, "sprint:#472", "stage", "worker", "started"),
		sprintEvent(3, -10*time.Minute, "sprint:#472", "decision", "worker", "accepted"),
		sprintEvent(4, -5*time.Minute, "sprint:#481", "stage", "manager", "started"),
	}
	useFixtures(t, board, func(int) ([]room.Event, error) { return timeline, nil })

	preset, err := watch.LoadPreset("sprint")
	if err != nil {
		t.Fatalf("preset: %v", err)
	}
	narrowed := preset.File.NarrowSources([]watch.SourceSpec{{Type: "sprint-thread", Target: "472"}})
	plan, err := narrowed.Build(watch.BuildOptions{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// The preset's 2h backfill survives the narrowing: the command line said
	// WHICH card, not how far back to read.
	if w := watch.BackfillWindow(plan.Sources); w != 2*time.Hour {
		t.Fatalf("backfill window = %v, want the preset's 2h", w)
	}

	var got []watch.Alert
	now := at0
	engine := watch.New(watch.Config{
		Sources:  plan.Sources,
		Rules:    plan.Rules,
		Sinks:    []watch.Sink{watch.FuncSink(func(a watch.Alert) error { got = append(got, a); return nil })},
		Interval: plan.Interval,
		Renotify: plan.Renotify,
		// Past the preset's 45m idle window, so a card that went quiet reports.
		MaxCycles: 50,
		Now:       func() time.Time { return now },
		Sleep:     func(_ context.Context, d time.Duration) bool { now = now.Add(d); return true },
	})
	if code := engine.Run(context.Background()); code != watch.ExitOK {
		t.Fatalf("code = %d", code)
	}

	want := []alertRow{
		{"stage", watch.ReasonMatch, "sprint:472"},
		{"judgement", watch.ReasonMatch, "sprint:472"},
		{"sprint-idle", watch.ReasonIdle, "sprint:472"},
	}
	if g := rows(got); showRows(g) != showRows(want) {
		t.Fatalf("alerts = %s\nwant       = %s\n(450 and 481 are other cards: naming one must not report the others)",
			showRows(g), showRows(want))
	}
}

// An explicit source of ONE kind leaves the preset's other kinds alone.
func TestExplicitMBSourceKeepsThePresetSprintThread(t *testing.T) {
	t.Setenv("BASHY_WATCH_DIR", t.TempDir())
	board := newFakeBoard()
	board.add(bus.Post{ID: "ask", From: "manager", To: "operator", Topic: "sprint",
		Body: "decision needed", At: ts(-2 * time.Minute)})
	timeline := []room.Event{
		sprintEvent(1, -5*time.Minute, "sprint:#472", "stage", "worker", "started"),
	}
	useFixtures(t, board, func(int) ([]room.Event, error) { return timeline, nil })

	preset, err := watch.LoadPreset("sprint")
	if err != nil {
		t.Fatalf("preset: %v", err)
	}
	narrowed := preset.File.NarrowSources([]watch.SourceSpec{{Type: "mb", Target: "operator"}})
	plan, err := narrowed.Build(watch.BuildOptions{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	var got []watch.Alert
	now := at0
	engine := watch.New(watch.Config{
		Sources:   plan.Sources,
		Rules:     plan.Rules,
		Sinks:     []watch.Sink{watch.FuncSink(func(a watch.Alert) error { got = append(got, a); return nil })},
		Interval:  plan.Interval,
		Renotify:  plan.Renotify,
		MaxCycles: 2,
		Now:       func() time.Time { return now },
		Sleep:     func(_ context.Context, d time.Duration) bool { now = now.Add(d); return true },
	})
	if code := engine.Run(context.Background()); code != watch.ExitOK {
		t.Fatalf("code = %d", code)
	}

	want := []alertRow{
		{"stage", watch.ReasonMatch, "sprint:472"},
		{"operator-mail", watch.ReasonMatch, "mb:ask"},
	}
	if g := rows(got); showRows(g) != showRows(want) {
		t.Fatalf("alerts = %s\nwant       = %s\n(an mb selection must not drop the preset's sprint-thread source)",
			showRows(g), showRows(want))
	}
}
