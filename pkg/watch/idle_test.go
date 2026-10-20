package watch

import (
	"context"
	"strings"
	"testing"
	"time"
)

// IDLE IS A POLICY, NOT JUST A TIMER. The operator's refinement is four
// statements, and each one is a test here:
//
//   - the steps are quarter-hours: 15m warning, 30m critical, re-notify 15m;
//   - a task may declare its own expected quiet window, and that window wins;
//   - a task YOUNGER than its window is never flagged — a run that started a
//     minute ago has not been quiet for fifteen;
//   - idle applies only to LONG-RUNNING tasks. A short foreground command is
//     not idle, it is finished.
//
// Every test runs on a fake clock; nothing here sleeps.

func TestIdleDefaultsAreQuarterHourSteps(t *testing.T) {
	if DefaultIdle != 15*time.Minute {
		t.Fatalf("DefaultIdle = %s, want 15m (warning)", DefaultIdle)
	}
	if DefaultStalled != 30*time.Minute {
		t.Fatalf("DefaultStalled = %s, want 30m (critical)", DefaultStalled)
	}
	if DefaultRenotify != 15*time.Minute {
		t.Fatalf("DefaultRenotify = %s, want 15m", DefaultRenotify)
	}
}

func TestIdleWarnsAtFifteenAndEscalatesToCriticalAtThirty(t *testing.T) {
	r := NewInverse(Inverse{RuleName: "idle", Class: ClassRun})
	r.Arm([]string{"run:repo-7"})
	r.Start(at0)

	if got := r.Tick(at0.Add(14 * time.Minute)); len(got) != 0 {
		t.Fatalf("fired inside the warning window: %+v", got)
	}
	warn := r.Tick(at0.Add(15 * time.Minute))
	if len(warn) != 1 || warn[0].State != StateFire || warn[0].Level != LevelWarning {
		t.Fatalf("at 15m: %+v, want one warning", warn)
	}
	if warn[0].Fields["level"] != LevelWarning || warn[0].Fields["idle_for"] != "15m0s" {
		t.Fatalf("warning fields = %+v", warn[0].Fields)
	}
	if still := r.Tick(at0.Add(29 * time.Minute)); len(still) != 1 || still[0].Level != LevelWarning {
		t.Fatalf("at 29m: %+v, want the warning restated", still)
	}
	crit := r.Tick(at0.Add(30 * time.Minute))
	if len(crit) != 1 || crit[0].Level != LevelCritical {
		t.Fatalf("at 30m: %+v, want the critical escalation", crit)
	}
	// "Alive but idle is stalled, never healthy": the critical line says so.
	if !strings.Contains(crit[0].Message, "stalled") {
		t.Fatalf("critical message = %q, want it to say stalled", crit[0].Message)
	}
}

func TestIdleEscalationIsDeliveredNotDedupedAsTheWarning(t *testing.T) {
	// The engine suppresses an identical fire. An escalation is not identical:
	// a critical that the dedupe swallowed would leave an operator looking at a
	// warning for a task that has been dead for an hour.
	sink := &collectSink{}
	r := NewInverse(Inverse{RuleName: "idle", Class: ClassRun, Expect: []string{"run:repo-7"}})
	now := at0
	e := New(Config{
		Rules: []Rule{r}, Sinks: []Sink{sink},
		Renotify: DefaultRenotify,
		Now:      func() time.Time { return now },
	})
	r.Start(at0)

	now = at0.Add(15 * time.Minute)
	e.Tick(now)
	now = at0.Add(20 * time.Minute)
	e.Tick(now)
	now = at0.Add(30 * time.Minute)
	e.Tick(now)

	if len(sink.got) != 2 {
		t.Fatalf("delivered %d alerts (%+v), want the warning then the critical", len(sink.got), sink.got)
	}
	if sink.got[0].Level != LevelWarning || sink.got[1].Level != LevelCritical {
		t.Fatalf("levels = %q, %q", sink.got[0].Level, sink.got[1].Level)
	}

	// Resume clears whatever level was firing — the clear is not level-specific,
	// because nobody wants "the critical cleared" while the warning stays.
	now = at0.Add(31 * time.Minute)
	e.Observe(Event{At: now, Locator: "run:repo-7", Kind: KindChanged, Field: "stdout_bytes",
		Attrs: map[string]string{AttrTask: ClassRun}})
	if len(sink.got) != 3 || sink.got[2].State != StateClear {
		t.Fatalf("alerts = %+v, want a clear", sink.got)
	}
	now = at0.Add(60 * time.Minute)
	e.Tick(now)
	if len(sink.got) != 4 || sink.got[3].Level != LevelWarning {
		t.Fatalf("after the clear the ladder restarts at warning: %+v", sink.got)
	}
}

func TestIdleRenotifiesOnTheConfiguredCadence(t *testing.T) {
	sink := &collectSink{}
	// Critical far away, so this measures the re-notify cadence and not the
	// escalation.
	r := NewInverse(Inverse{RuleName: "idle", Class: ClassRun, Critical: 4 * time.Hour,
		Expect: []string{"run:repo-7"}})
	now := at0
	e := New(Config{Rules: []Rule{r}, Sinks: []Sink{sink}, Renotify: DefaultRenotify,
		Now: func() time.Time { return now }})
	r.Start(at0)

	for _, d := range []time.Duration{15, 20, 25, 30, 44, 45} {
		now = at0.Add(d * time.Minute)
		e.Tick(now)
	}
	if len(sink.got) != 3 {
		t.Fatalf("delivered %d (%+v), want 15m, 30m and 45m", len(sink.got), sink.got)
	}
	for i, want := range []time.Duration{15, 30, 45} {
		if got := sink.got[i].At.Sub(at0); got != want*time.Minute {
			t.Fatalf("alert %d at %s, want %s", i, got, want*time.Minute)
		}
	}
}

func TestIdleDeclaredQuietWindowOverridesTheDefault(t *testing.T) {
	// A task that is EXPECTED to be quiet for 45 minutes is not stalled at 15.
	r := NewInverse(Inverse{RuleName: "idle", Class: ClassRun})
	r.Declare(Task{Locator: "job:nightly", Class: ClassJob, Quiet: 45 * time.Minute, Started: at0.Add(-2 * time.Hour)})
	r.Start(at0)

	if got := r.Tick(at0.Add(44 * time.Minute)); len(got) != 0 {
		t.Fatalf("fired inside the declared window: %+v", got)
	}
	warn := r.Tick(at0.Add(45 * time.Minute))
	if len(warn) != 1 || warn[0].Level != LevelWarning {
		t.Fatalf("at 45m: %+v, want the warning at the declared window", warn)
	}
	if warn[0].Fields["within"] != "45m0s" {
		t.Fatalf("within = %v, want the declared window", warn[0].Fields["within"])
	}
	// The critical keeps the configured ratio, so a declared window scales both
	// rungs instead of collapsing them onto each other.
	if got := r.Tick(at0.Add(89 * time.Minute)); len(got) != 1 || got[0].Level != LevelWarning {
		t.Fatalf("at 89m: %+v, want still the warning", got)
	}
	crit := r.Tick(at0.Add(90 * time.Minute))
	if len(crit) != 1 || crit[0].Level != LevelCritical {
		t.Fatalf("at 90m: %+v, want the critical at 2x the declared window", crit)
	}
}

func TestIdleDeclaredQuietWindowFromAnEventAttr(t *testing.T) {
	// A journal source declares per record, not per watcher: a run that says
	// "my quiet window is 30m" must be read off the event.
	r := NewInverse(Inverse{RuleName: "idle", Class: ClassRun})
	r.Start(at0)
	r.On(Event{At: at0, Locator: "run:slow", Kind: KindChanged,
		Attrs: map[string]string{AttrTask: ClassRun, AttrQuiet: "30m"}}, at0)

	if got := r.Tick(at0.Add(20 * time.Minute)); len(got) != 0 {
		t.Fatalf("fired inside the declared 30m window: %+v", got)
	}
	got := r.Tick(at0.Add(30 * time.Minute))
	if len(got) != 1 || got[0].Fields["within"] != "30m0s" {
		t.Fatalf("alerts = %+v, want the warning at the declared 30m", got)
	}
}

func TestIdleNeverFlagsATaskYoungerThanItsWindow(t *testing.T) {
	// The back-dated-event trap: a journal entry an hour old does NOT mean the
	// task is an hour old. A run that started a minute ago is young, and a
	// young task is never idle.
	r := NewInverse(Inverse{RuleName: "idle", Class: ClassRun})
	r.Declare(Task{Locator: "run:fresh", Class: ClassRun, Started: at0})
	r.Start(at0)
	r.On(Event{At: at0.Add(-2 * time.Hour), Locator: "run:fresh", Kind: KindChanged,
		Attrs: map[string]string{AttrTask: ClassRun}}, at0)

	if got := r.Tick(at0.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("flagged a task one minute old: %+v", got)
	}
	if got := r.Tick(at0.Add(14 * time.Minute)); len(got) != 0 {
		t.Fatalf("flagged a task younger than its window: %+v", got)
	}
	if got := r.Tick(at0.Add(15 * time.Minute)); len(got) != 1 {
		t.Fatalf("alerts = %+v, want the warning once the task is old enough", got)
	}
}

func TestIdleNeverFlagsAShortForegroundCommand(t *testing.T) {
	// Idle is for weave runs, managed sessions, services, background and
	// schedule jobs. A foreground `go build` that printed nothing for twenty
	// minutes is not stalled — it is a command, and the operator is watching it.
	r := NewInverse(Inverse{RuleName: "idle", Class: ClassRun})
	r.Declare(Task{Locator: "cmd:go-build", Class: ClassCommand})
	r.Start(at0)
	r.On(Event{At: at0, Locator: "cmd:go-build", Kind: KindChanged,
		Attrs: map[string]string{AttrTask: ClassCommand}}, at0)

	if got := r.Tick(at0.Add(2 * time.Hour)); len(got) != 0 {
		t.Fatalf("flagged a short foreground command: %+v", got)
	}
	if !LongRunning(ClassRun) || !LongRunning(ClassSession) || !LongRunning(ClassService) ||
		!LongRunning(ClassJob) || !LongRunning(ClassSchedule) {
		t.Fatal("the long-running classes are weave runs, sessions, services, jobs and schedules")
	}
	if LongRunning(ClassCommand) || LongRunning(ClassForeground) {
		t.Fatal("a short foreground command is never long-running")
	}
}

func TestIdlePollSourceDeclaresItsTask(t *testing.T) {
	// The CLI's `--task`/`--quiet-window` have to reach the rule, and the poll
	// source is where they land: it is the one thing that knows what it polls.
	p := NewPollSource("poll", "cmd:ls", func(context.Context) ([]byte, int, error) {
		return []byte("still here\n"), 0, nil
	})
	p.Class, p.Quiet = ClassCommand, 7*time.Minute
	tasks := p.Tasks()
	if len(tasks) != 1 || tasks[0].Locator != "cmd:ls" || tasks[0].Class != ClassCommand ||
		tasks[0].Quiet != 7*time.Minute {
		t.Fatalf("tasks = %+v", tasks)
	}

	// And the engine hands them to the rule, so a declaration reaches an idle
	// rule that never named the locator.
	r := NewInverse(Inverse{RuleName: "idle"})
	e := New(Config{Sources: []Source{p}, Rules: []Rule{r}, MaxCycles: 1,
		Now: func() time.Time { return at0 }})
	e.Run(nil)
	if got := r.Tick(at0.Add(2 * time.Hour)); len(got) != 0 {
		t.Fatalf("flagged the declared short command: %+v", got)
	}
}

func TestIdleRuleSpecCarriesTheCriticalRung(t *testing.T) {
	f, err := ParseRuleFile([]byte(`{
	  "schema_version": "bashy-watch-rules-v1",
	  "rules": [{"type": "inverse", "name": "idle", "within": "15m", "critical": "30m", "class": "run"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := f.Build(BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r, ok := plan.Rules[0].(*InverseRule)
	if !ok {
		t.Fatalf("rule = %T", plan.Rules[0])
	}
	r.Arm([]string{"run:x"})
	r.Start(at0)
	if got := r.Tick(at0.Add(30 * time.Minute)); len(got) != 1 || got[0].Level != LevelCritical {
		t.Fatalf("alerts = %+v, want the critical rung from the rule file", got)
	}
}

func TestIdlePresetIsTheRefinedPolicy(t *testing.T) {
	p, err := LoadPreset("idle")
	if err != nil {
		t.Fatal(err)
	}
	if p.File.Renotify.D() != DefaultRenotify {
		t.Fatalf("preset renotify = %s, want %s", p.File.Renotify.D(), DefaultRenotify)
	}
	if len(p.File.Rules) != 1 {
		t.Fatalf("preset rules = %+v", p.File.Rules)
	}
	rule := p.File.Rules[0]
	if rule.Within.D() != DefaultIdle {
		t.Fatalf("preset within = %s, want %s", rule.Within.D(), DefaultIdle)
	}
	if rule.Critical.D() != DefaultStalled {
		t.Fatalf("preset critical = %s, want %s", rule.Critical.D(), DefaultStalled)
	}
}
