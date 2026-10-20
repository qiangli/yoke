package watch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeRule emits whatever the test tells it to, so the engine's DELIVERY POLICY
// can be tested without a real condition behind it.
type fakeRule struct {
	name    string
	onFn    func(Event, time.Time) []Alert
	tickFn  func(time.Time) []Alert
	started bool
}

func (f *fakeRule) Name() string { return f.name }
func (f *fakeRule) On(ev Event, now time.Time) []Alert {
	if f.onFn == nil {
		return nil
	}
	return f.onFn(ev, now)
}
func (f *fakeRule) Tick(now time.Time) []Alert {
	if f.tickFn == nil {
		return nil
	}
	return f.tickFn(now)
}
func (f *fakeRule) Start(time.Time) { f.started = true }

type collectSink struct{ got []Alert }

func (c *collectSink) Deliver(a Alert) error { c.got = append(c.got, a); return nil }

// fakeSource replays canned event batches, one batch per poll, then nothing.
type fakeSource struct {
	name   string
	batch  [][]Event
	errs   map[int]error
	polled int
}

func (f *fakeSource) Name() string { return f.name }
func (f *fakeSource) Poll(_ context.Context, now time.Time) ([]Event, error) {
	i := f.polled
	f.polled++
	if err := f.errs[i]; err != nil {
		return nil, err
	}
	if i < len(f.batch) {
		return f.batch[i], nil
	}
	return nil, nil
}

func fire(reason string, at time.Time) Alert {
	return Alert{At: at, Rule: "r", State: StateFire, Reason: reason, Locator: "seat:worker", Message: "stalled"}
}

func TestEngineDeliversTransitionsOnlyAndRenotifies(t *testing.T) {
	sink := &collectSink{}
	now := at0
	rule := &fakeRule{name: "r", tickFn: func(tt time.Time) []Alert { return []Alert{fire(ReasonIdle, tt)} }}
	e := New(Config{
		Rules: []Rule{rule}, Sinks: []Sink{sink},
		Renotify: 10 * time.Minute,
		Now:      func() time.Time { return now },
	})
	if got := e.Tick(now); len(got) != 1 {
		t.Fatalf("first tick delivered %d, want 1", len(got))
	}
	now = at0.Add(time.Minute)
	if got := e.Tick(now); len(got) != 0 {
		t.Fatalf("a persisting condition was re-delivered: %+v", got)
	}
	now = at0.Add(10 * time.Minute)
	if got := e.Tick(now); len(got) != 1 {
		t.Fatalf("re-notify interval did not fire: %+v", got)
	}
	if len(sink.got) != 2 {
		t.Fatalf("sink saw %d alerts, want 2", len(sink.got))
	}
}

func TestEngineClearsOnlyWhatItWarnedAbout(t *testing.T) {
	sink := &collectSink{}
	var next []Alert
	rule := &fakeRule{name: "r", tickFn: func(time.Time) []Alert { return next }}
	e := New(Config{Rules: []Rule{rule}, Sinks: []Sink{sink}, Now: func() time.Time { return at0 }})

	clear := fire(ReasonIdle, at0)
	clear.State = StateClear
	next = []Alert{clear}
	if got := e.Tick(at0); len(got) != 0 {
		t.Fatalf("cleared a condition nobody was warned about: %+v", got)
	}
	next = []Alert{fire(ReasonIdle, at0)}
	if got := e.Tick(at0); len(got) != 1 {
		t.Fatalf("fire delivered %d, want 1", len(got))
	}
	next = []Alert{clear}
	if got := e.Tick(at0); len(got) != 1 || got[0].State != StateClear {
		t.Fatalf("clear = %+v, want delivered once", got)
	}
	if got := e.Tick(at0); len(got) != 0 {
		t.Fatalf("clear repeated: %+v", got)
	}
}

func TestEngineUntilExitsZeroOnTheFirstAlert(t *testing.T) {
	// This is the exit code an agent blocks on, so it is the one that must not
	// be ambiguous: 0 means the awaited thing happened.
	sink := &collectSink{}
	now := at0
	src := &fakeSource{name: "poll", batch: [][]Event{
		nil,
		{{At: at0, Source: "poll", Kind: KindChanged, Locator: "sprint:480", Field: "stage", After: "accepted"}},
	}}
	rule, err := NewMatch("accepted", Criteria{Field: "stage", After: "accepted"})
	if err != nil {
		t.Fatalf("rule: %v", err)
	}
	e := New(Config{
		Sources: []Source{src}, Rules: []Rule{rule}, Sinks: []Sink{sink},
		Until: true, Interval: time.Second,
		Now:   func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) bool { now = now.Add(d); return true },
	})
	if code := e.Run(context.Background()); code != ExitOK {
		t.Fatalf("code = %d, want %d", code, ExitOK)
	}
	if len(sink.got) != 1 || sink.got[0].Reason != ReasonMatch {
		t.Fatalf("sink = %+v", sink.got)
	}
	if src.polled != 2 {
		t.Fatalf("polled %d times, want 2 (it stops at the match)", src.polled)
	}
}

func TestEngineTimeoutExitsOneWithNothingToReport(t *testing.T) {
	// "It happened" and "time ran out" must never be the same answer.
	now := at0
	src := &fakeSource{name: "poll"}
	e := New(Config{
		Sources: []Source{src}, Until: true, Timeout: 5 * time.Minute, Interval: time.Minute,
		Now:   func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) bool { now = now.Add(d); return true },
	})
	if code := e.Run(context.Background()); code != ExitTimeout {
		t.Fatalf("code = %d, want %d", code, ExitTimeout)
	}
}

func TestEngineRunEndsOnTheCycleBudget(t *testing.T) {
	now := at0
	src := &fakeSource{name: "poll"}
	e := New(Config{
		Sources: []Source{src}, Interval: time.Minute, MaxCycles: 3,
		Now:   func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) bool { now = now.Add(d); return true },
	})
	if code := e.Run(context.Background()); code != ExitOK {
		t.Fatalf("code = %d, want %d", code, ExitOK)
	}
	if src.polled != 3 {
		t.Fatalf("polled %d times, want 3", src.polled)
	}
}

func TestEngineCancelExits130(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := New(Config{Sources: []Source{&fakeSource{name: "poll"}}, Interval: time.Minute, Now: func() time.Time { return at0 }})
	if code := e.Run(ctx); code != ExitInterrupted {
		t.Fatalf("code = %d, want %d", code, ExitInterrupted)
	}
}

func TestEngineSourceErrorIsReportedAndSurvived(t *testing.T) {
	// A source that cannot read its store is a fact to report, not a reason to
	// stop watching — the conductor's script exited here and watched nothing
	// for the rest of the night.
	now := at0
	boom := errors.New("snapshot failed")
	src := &fakeSource{name: "poll", errs: map[int]error{0: boom}, batch: [][]Event{nil, nil, nil}}
	var errs []string
	e := New(Config{
		Sources: []Source{src}, Interval: time.Minute, MaxCycles: 3,
		Now:     func() time.Time { return now },
		Sleep:   func(_ context.Context, d time.Duration) bool { now = now.Add(d); return true },
		OnError: func(src string, err error) { errs = append(errs, src+": "+err.Error()) },
	})
	if code := e.Run(context.Background()); code != ExitOK {
		t.Fatalf("code = %d", code)
	}
	if len(errs) != 1 || !strings.Contains(errs[0], "poll: snapshot failed") {
		t.Fatalf("errors = %v", errs)
	}
	if src.polled != 3 {
		t.Fatalf("polled %d times, want 3 — the run continued", src.polled)
	}
}

func TestEngineArmsTimeRulesAtStart(t *testing.T) {
	now := at0
	rule := &fakeRule{name: "r"}
	e := New(Config{Rules: []Rule{rule}, Interval: time.Minute, MaxCycles: 1, Now: func() time.Time { return now }})
	e.Run(context.Background())
	if !rule.started {
		t.Fatal("a liveness rule never told when the clock started cannot fire")
	}
}

func TestSinksRenderAlerts(t *testing.T) {
	a := Alert{
		At: at0, Rule: "idle", State: StateFire, Reason: ReasonIdle, Locator: "run:repo-7",
		Message: "no output for 14m0s \x00\xff",
		Fields:  map[string]any{"idle_for": "14m0s"},
	}
	var buf strings.Builder
	if err := (ConsoleSink{W: &buf}).Deliver(a); err != nil {
		t.Fatalf("console: %v", err)
	}
	line := buf.String()
	for _, want := range []string{"idle", "run:repo-7", "14m0s"} {
		if !strings.Contains(line, want) {
			t.Fatalf("console line %q missing %q", line, want)
		}
	}
	if strings.ContainsRune(line, 0) || !strings.HasSuffix(line, "\n") {
		t.Fatalf("console line = %q", line)
	}

	buf.Reset()
	if err := (JSONSink{W: &buf}).Deliver(a); err != nil {
		t.Fatalf("json: %v", err)
	}
	var rec struct {
		SchemaVersion string         `json:"schema_version"`
		Rule          string         `json:"rule"`
		State         string         `json:"state"`
		Fields        map[string]any `json:"fields"`
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 {
		t.Fatalf("ndjson = %q, want exactly one line", out)
	}
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("ndjson: %v (%q)", err, out)
	}
	if rec.SchemaVersion != SchemaVersion || rec.Rule != "idle" || rec.State != StateFire || rec.Fields["idle_for"] != "14m0s" {
		t.Fatalf("record = %+v", rec)
	}
}

type expectingSource struct{ fakeSource }

func (e *expectingSource) Expected() []string { return []string{"run:repo-7"} }

func TestEngineArmsExpectedLocatorsFromSources(t *testing.T) {
	now := at0
	sink := &collectSink{}
	rule := NewInverse(Inverse{RuleName: "idle", Within: 2 * time.Minute})
	e := New(Config{
		Sources: []Source{&expectingSource{fakeSource{name: "poll"}}},
		Rules:   []Rule{rule}, Sinks: []Sink{sink},
		Interval: time.Minute, MaxCycles: 4,
		Now:   func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) bool { now = now.Add(d); return true },
	})
	if code := e.Run(context.Background()); code != ExitOK {
		t.Fatalf("code = %d", code)
	}
	if len(sink.got) != 1 || sink.got[0].Locator != "run:repo-7" || sink.got[0].Reason != ReasonIdle {
		t.Fatalf("sink = %+v, want one idle alert for the expected locator", sink.got)
	}
}
