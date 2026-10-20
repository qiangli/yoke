package watch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// BASH# SCRIPT HOOKS — a rod, not a fish.
//
// Every test here runs the hook through an INJECTED exec: the gate is that the
// engine applies its own rules to a hook's output and its own policy to a
// hook's failure, and neither needs a real bashy to establish. The one thing
// only the real runner can get wrong — exec'ing the test binary instead of the
// installed bashy — has its own test below.

// fakeExec records what it was handed and replays a canned result.
type fakeExec struct {
	mu     sync.Mutex
	stdin  [][]byte
	argv   [][]string
	calls  int
	fn     func(call int) ([]byte, int, error)
	block  chan struct{}
	inside chan struct{}
}

func (f *fakeExec) exec(ctx context.Context, h Hook, stdin []byte) ([]byte, int, error) {
	f.mu.Lock()
	call := f.calls
	f.calls++
	f.stdin = append(f.stdin, append([]byte(nil), stdin...))
	f.argv = append(f.argv, append([]string(nil), h.Args...))
	f.mu.Unlock()
	if f.inside != nil {
		f.inside <- struct{}{}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
	if f.fn == nil {
		return nil, 0, nil
	}
	return f.fn(call)
}

func (f *fakeExec) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func testHooks(fe *fakeExec) *Hooks {
	hs := NewHooks()
	hs.Exec = fe.exec
	return hs
}

func writeHook(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+hookExt)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHookActionReceivesTheEventOnStdin(t *testing.T) {
	// The event goes on STDIN and the argv is FIXED. Nothing from the alert is
	// ever interpolated into the script or its arguments: that is the whole
	// reason a hook is a rod and not a shell-string builder.
	fe := &fakeExec{}
	hs := testHooks(fe)
	dir := t.TempDir()
	writeHook(t, filepath.Join(dir, "hooks"), "reclaim", "echo hi\n")
	t.Setenv("BASHY_WATCH_DIR", dir)

	sink, err := NewHookSink(hs, HookSpec{Script: "reclaim", Args: []string{"--apply"}})
	if err != nil {
		t.Fatal(err)
	}
	alert := Alert{
		At: at0, Rule: "idle", State: StateFire, Reason: ReasonIdle, Level: LevelCritical,
		Locator: "disk:/tmp", Message: "disk:/tmp: stalled", Fields: map[string]any{"level": LevelCritical},
	}
	if err := sink.Deliver(alert); err != nil {
		t.Fatal(err)
	}
	if fe.count() != 1 {
		t.Fatalf("exec calls = %d, want 1", fe.count())
	}
	var got struct {
		SchemaVersion string `json:"schema_version"`
		Alert
	}
	if err := json.Unmarshal(fe.stdin[0], &got); err != nil {
		t.Fatalf("stdin is not the alert record: %v\n%s", err, fe.stdin[0])
	}
	if got.SchemaVersion != SchemaVersion || got.Locator != "disk:/tmp" || got.Level != LevelCritical {
		t.Fatalf("stdin alert = %+v", got)
	}
	if len(fe.argv[0]) != 1 || fe.argv[0][0] != "--apply" {
		t.Fatalf("argv = %q, want exactly the configured argv", fe.argv[0])
	}
}

func TestHookActionOnlyRunsForTheAlertsItNames(t *testing.T) {
	fe := &fakeExec{}
	hs := testHooks(fe)
	dir := t.TempDir()
	writeHook(t, filepath.Join(dir, "hooks"), "page", "echo hi\n")
	t.Setenv("BASHY_WATCH_DIR", dir)

	sink, err := NewHookSink(hs, HookSpec{Script: "page", When: Criteria{Attrs: map[string]string{"level": LevelCritical}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = sink.Deliver(Alert{Rule: "idle", State: StateFire, Level: LevelWarning})
	if fe.count() != 0 {
		t.Fatalf("ran for a warning the hook did not ask for")
	}
	_ = sink.Deliver(Alert{Rule: "idle", State: StateFire, Level: LevelCritical})
	if fe.count() != 1 {
		t.Fatalf("exec calls = %d, want the critical to run it", fe.count())
	}
}

func TestHookNonZeroExitIsItselfAnAlarm(t *testing.T) {
	// A hook that failed silently is worse than no hook: the operator believes
	// the reclaim ran.
	fe := &fakeExec{fn: func(int) ([]byte, int, error) { return []byte("no space left\n"), 3, nil }}
	hs := testHooks(fe)
	dir := t.TempDir()
	writeHook(t, filepath.Join(dir, "hooks"), "reclaim", "exit 3\n")
	t.Setenv("BASHY_WATCH_DIR", dir)

	var raised []Alert
	sink, err := NewHookSink(hs, HookSpec{Script: "reclaim"})
	if err != nil {
		t.Fatal(err)
	}
	// With an Alarm wired the failure goes to the ALERT STREAM, which is both
	// where a watcher looks and where it gets deduped.
	sink.Alarm = func(a Alert) { raised = append(raised, a) }
	if err := sink.Deliver(Alert{At: at0, Rule: "idle", State: StateFire, Locator: "disk:/tmp"}); err != nil {
		t.Fatalf("Deliver = %v, want the alarm to carry it", err)
	}
	if len(raised) != 1 || raised[0].Reason != ReasonError || raised[0].State != StateFire {
		t.Fatalf("alarms = %+v, want one error alert", raised)
	}
	if raised[0].Fields["exit"] != 3 {
		t.Fatalf("fields = %+v, want the exit status", raised[0].Fields)
	}
	if !strings.Contains(raised[0].Message, "reclaim") {
		t.Fatalf("message = %q, want the hook named", raised[0].Message)
	}

	// With NO alarm wired it must still never be silent: the engine reports a
	// sink error.
	bare, err := NewHookSink(hs, HookSpec{Script: "reclaim"})
	if err != nil {
		t.Fatal(err)
	}
	if err := bare.Deliver(Alert{At: at0, Rule: "idle", State: StateFire}); err == nil {
		t.Fatal("Deliver returned nil for a failing hook with no alarm wired")
	}
}

func TestHookTimeoutIsItselfAnAlarm(t *testing.T) {
	fe := &fakeExec{block: make(chan struct{})}
	defer close(fe.block)
	hs := testHooks(fe)
	dir := t.TempDir()
	writeHook(t, filepath.Join(dir, "hooks"), "slow", "sleep 600\n")
	t.Setenv("BASHY_WATCH_DIR", dir)

	var raised []Alert
	sink, err := NewHookSink(hs, HookSpec{Script: "slow", Timeout: Duration(20 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	sink.Alarm = func(a Alert) { raised = append(raised, a) }
	if err := sink.Deliver(Alert{At: at0, Rule: "idle", State: StateFire}); err != nil {
		t.Fatalf("Deliver = %v, want the alarm to carry it", err)
	}
	if len(raised) != 1 || raised[0].Reason != ReasonError {
		t.Fatalf("alarms = %+v", raised)
	}
	if raised[0].Fields["timeout"] != "20ms" {
		t.Fatalf("fields = %+v, want the timeout reported", raised[0].Fields)
	}
}

func TestHookDefaultTimeoutsAreKindSpecific(t *testing.T) {
	if DefaultProbeTimeout != 60*time.Second {
		t.Fatalf("DefaultProbeTimeout = %s, want 60s", DefaultProbeTimeout)
	}
	if DefaultActionTimeout != 5*time.Minute {
		t.Fatalf("DefaultActionTimeout = %s, want 5m", DefaultActionTimeout)
	}
	dir := t.TempDir()
	writeHook(t, filepath.Join(dir, "hooks"), "h", "true\n")
	t.Setenv("BASHY_WATCH_DIR", dir)
	hs := NewHooks()
	probe, err := hs.Resolve("h", HookProbe)
	if err != nil {
		t.Fatal(err)
	}
	action, err := hs.Resolve("h", HookAction)
	if err != nil {
		t.Fatal(err)
	}
	if probe.Timeout != DefaultProbeTimeout || action.Timeout != DefaultActionTimeout {
		t.Fatalf("timeouts = %s / %s", probe.Timeout, action.Timeout)
	}
}

func TestHookConcurrencyIsOnePerHook(t *testing.T) {
	// An overlapping invocation is SKIPPED, not queued: a reclaim that queued
	// one run per tick would still be working through the night's backlog in
	// the morning.
	fe := &fakeExec{block: make(chan struct{}), inside: make(chan struct{}, 1)}
	hs := testHooks(fe)
	dir := t.TempDir()
	writeHook(t, filepath.Join(dir, "hooks"), "reclaim", "sleep 1\n")
	t.Setenv("BASHY_WATCH_DIR", dir)

	h, err := hs.Resolve("reclaim", HookAction)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan HookResult, 1)
	go func() { done <- hs.Run(context.Background(), h, []byte("{}")) }()
	<-fe.inside

	second := hs.Run(context.Background(), h, []byte("{}"))
	if !second.Skipped {
		t.Fatalf("second run = %+v, want it skipped while the first is in flight", second)
	}
	close(fe.block)
	if first := <-done; first.Skipped {
		t.Fatal("the first run was skipped")
	}
	if fe.count() != 1 {
		t.Fatalf("exec calls = %d, want the overlapping run never to have started", fe.count())
	}
	// And once it is finished the hook is runnable again.
	fe.block = nil
	if third := hs.Run(context.Background(), h, []byte("{}")); third.Skipped {
		t.Fatal("still skipping after the first run finished")
	}
}

func TestHookMasterNeverRunsUserHooks(t *testing.T) {
	// A4. The master's job is to decide, not to execute a user's script.
	fe := &fakeExec{}
	hs := testHooks(fe)
	hs.Master = true
	dir := t.TempDir()
	writeHook(t, filepath.Join(dir, "hooks"), "reclaim", "true\n")
	t.Setenv("BASHY_WATCH_DIR", dir)

	h, err := hs.Resolve("reclaim", HookAction)
	if err != nil {
		t.Fatal(err)
	}
	res := hs.Run(context.Background(), h, nil)
	if res.Err == nil || !errors.Is(res.Err, ErrHookMaster) {
		t.Fatalf("result = %+v, want the master refusal", res)
	}
	if fe.count() != 0 {
		t.Fatal("the master executed a user hook")
	}
	// The refusal is detected from the environment too, so a master that did
	// not set the field in code is still refused.
	hs2 := testHooks(fe)
	t.Setenv(EnvWatchRole, "master")
	if res := hs2.Run(context.Background(), h, nil); !errors.Is(res.Err, ErrHookMaster) {
		t.Fatalf("result = %+v, want the refusal from %s", res, EnvWatchRole)
	}
}

func TestHookDestructiveNeedsExplicitConfirmation(t *testing.T) {
	// A6. A hook that deletes is not run because a threshold was crossed; it
	// is run because somebody configured that it may.
	fe := &fakeExec{}
	hs := testHooks(fe)
	dir := t.TempDir()
	writeHook(t, filepath.Join(dir, "hooks"), "reclaim", "rm -rf x\n")
	t.Setenv("BASHY_WATCH_DIR", dir)

	h, err := hs.Resolve("reclaim", HookAction)
	if err != nil {
		t.Fatal(err)
	}
	h.Destructive = true
	if res := hs.Run(context.Background(), h, nil); !errors.Is(res.Err, ErrHookUnconfirmed) {
		t.Fatalf("result = %+v, want the unconfirmed refusal", res)
	}
	if fe.count() != 0 {
		t.Fatal("ran an unconfirmed destructive hook")
	}
	h.Confirm = true
	if res := hs.Run(context.Background(), h, nil); res.Err != nil {
		t.Fatalf("result = %+v, want the confirmed hook to run", res)
	}
}

func TestHookLocalAssetBeatsEmbedded(t *testing.T) {
	// The ring precedence is the whole sharing story: a user overrides a
	// shipped example by dropping a file of the same name into the local ring.
	names := HookNames()
	if len(names) == 0 {
		t.Fatal("no embedded hook examples")
	}
	var shipped string
	for _, n := range names {
		shipped = n
		break
	}
	hs := NewHooks()
	embedded, err := hs.Resolve(shipped, HookAction)
	if err != nil {
		t.Fatal(err)
	}
	if embedded.Ring.String() != "embedded" {
		t.Fatalf("%s came from %s, want embedded", shipped, embedded.Ring)
	}

	dir := t.TempDir()
	writeHook(t, filepath.Join(dir, "hooks"), shipped, "echo mine\n")
	t.Setenv("BASHY_WATCH_DIR", dir)
	local, err := NewHooks().Resolve(shipped, HookAction)
	if err != nil {
		t.Fatal(err)
	}
	if local.Ring.String() != "local" {
		t.Fatalf("%s came from %s, want local to win", shipped, local.Ring)
	}
	if !strings.Contains(string(local.Body), "echo mine") {
		t.Fatalf("body = %q, want the local script", local.Body)
	}
}

func TestHookUnknownScriptIsAnError(t *testing.T) {
	// A hook that silently did not exist is a watch that silently does nothing.
	t.Setenv("BASHY_WATCH_DIR", t.TempDir())
	if _, err := NewHooks().Resolve("nope", HookAction); err == nil {
		t.Fatal("resolved a hook that does not exist")
	}
}

func TestHookProbeSamplesBecomeEvents(t *testing.T) {
	fe := &fakeExec{fn: func(int) ([]byte, int, error) {
		return []byte(`{"resource":"gpu:0","field":"utilization","value":91,"unit":"percent"}
{"resource":"gpu:0","field":"memory_used","value":7.5,"unit":"GiB"}
`), 0, nil
	}}
	hs := testHooks(fe)
	dir := t.TempDir()
	writeHook(t, filepath.Join(dir, "hooks"), "gpu", "echo\n")
	t.Setenv("BASHY_WATCH_DIR", dir)

	src, err := NewHookSource(hs, HookSpec{Script: "gpu"})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := src.Poll(context.Background(), at0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("events = %+v, want one per sample", evs)
	}
	if evs[0].Kind != KindSample || evs[0].Locator != "gpu:0" || evs[0].Field != "utilization" {
		t.Fatalf("event = %+v", evs[0])
	}
	if scalar(evs[0].After) != "91" {
		t.Fatalf("value = %v, want 91", evs[0].After)
	}
	if evs[0].Attrs["unit"] != "percent" {
		t.Fatalf("attrs = %+v, want the unit", evs[0].Attrs)
	}
}

func TestHookProbeValuesCrossWarningCriticalAndClear(t *testing.T) {
	// THE PROBE GATE. Threshold rules are Phase 2, so the crossing is the
	// INVERSE rule's warning -> critical -> clear ladder over the probe's own
	// samples, plus a change rule on the values themselves: the engine applies
	// its rules to a user's probe exactly as to a built-in sampler.
	cycle := 0
	fe := &fakeExec{fn: func(int) ([]byte, int, error) {
		cycle++
		switch cycle {
		case 1:
			return []byte(`{"resource":"gpu:0","field":"utilization","value":10}` + "\n"), 0, nil
		case 2, 3:
			// The probe reports nothing: the GPU tool is there, the value is not.
			return nil, 0, nil
		default:
			return []byte(`{"resource":"gpu:0","field":"utilization","value":90}` + "\n"), 0, nil
		}
	}}
	hs := testHooks(fe)
	dir := t.TempDir()
	writeHook(t, filepath.Join(dir, "hooks"), "gpu", "echo\n")
	t.Setenv("BASHY_WATCH_DIR", dir)

	src, err := NewHookSource(hs, HookSpec{Script: "gpu", Class: ClassService})
	if err != nil {
		t.Fatal(err)
	}
	idle := NewInverse(Inverse{RuleName: "idle", Class: ClassService})
	change, err := NewChange("value", []string{"utilization"}, Criteria{})
	if err != nil {
		t.Fatal(err)
	}
	sink := &collectSink{}
	now := at0
	e := New(Config{
		Sources: []Source{src}, Rules: []Rule{idle, change}, Sinks: []Sink{sink},
		Now: func() time.Time { return now },
	})
	// Cycle 1 arms the locator with a sample.
	for _, ev := range mustPoll(t, src, now) {
		e.Observe(ev)
	}
	now = at0.Add(15 * time.Minute)
	mustPoll(t, src, now)
	e.Tick(now)
	now = at0.Add(30 * time.Minute)
	mustPoll(t, src, now)
	e.Tick(now)

	levels := []string{}
	for _, a := range sink.got {
		levels = append(levels, a.State+"/"+a.Level)
	}
	if len(sink.got) != 2 || sink.got[0].Level != LevelWarning || sink.got[1].Level != LevelCritical {
		t.Fatalf("alerts = %q, want warning then critical", levels)
	}

	// The value comes back: the change rule sees it and the ladder clears.
	now = at0.Add(31 * time.Minute)
	for _, ev := range mustPoll(t, src, now) {
		e.Observe(ev)
	}
	var cleared, changed bool
	for _, a := range sink.got[2:] {
		if a.State == StateClear {
			cleared = true
		}
		if a.Reason == ReasonChange && scalar(a.Fields["after"]) == "90" {
			changed = true
		}
	}
	if !cleared {
		t.Fatalf("alerts = %+v, want the ladder cleared on resume", sink.got)
	}
	if !changed {
		t.Fatalf("alerts = %+v, want the probe's value change alarmed", sink.got)
	}
}

func mustPoll(t *testing.T, src Source, now time.Time) []Event {
	t.Helper()
	evs, err := src.Poll(context.Background(), now)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	return evs
}

func TestHookProbeFailureIsAnAlarmAndKeepsWatching(t *testing.T) {
	fe := &fakeExec{fn: func(int) ([]byte, int, error) { return []byte("nvidia-smi: not found\n"), 127, nil }}
	hs := testHooks(fe)
	dir := t.TempDir()
	writeHook(t, filepath.Join(dir, "hooks"), "gpu", "echo\n")
	t.Setenv("BASHY_WATCH_DIR", dir)

	var raised []Alert
	src, err := NewHookSource(hs, HookSpec{Script: "gpu"})
	if err != nil {
		t.Fatal(err)
	}
	src.Alarm = func(a Alert) { raised = append(raised, a) }
	if _, err := src.Poll(context.Background(), at0); err == nil {
		t.Fatal("Poll returned nil for a probe that exited 127")
	}
	if len(raised) != 1 || raised[0].Reason != ReasonError || raised[0].Fields["exit"] != 127 {
		t.Fatalf("alarms = %+v", raised)
	}
}

func TestHookProbeMalformedLineIsAnAlarmNotASilentDrop(t *testing.T) {
	fe := &fakeExec{fn: func(int) ([]byte, int, error) {
		return []byte(`{"resource":"gpu:0","field":"utilization","value":91}` + "\nnot json\n"), 0, nil
	}}
	hs := testHooks(fe)
	dir := t.TempDir()
	writeHook(t, filepath.Join(dir, "hooks"), "gpu", "echo\n")
	t.Setenv("BASHY_WATCH_DIR", dir)

	var raised []Alert
	src, err := NewHookSource(hs, HookSpec{Script: "gpu"})
	if err != nil {
		t.Fatal(err)
	}
	src.Alarm = func(a Alert) { raised = append(raised, a) }
	evs, err := src.Poll(context.Background(), at0)
	if err != nil {
		t.Fatalf("one bad line must not discard the good ones: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want the readable sample kept", evs)
	}
	if len(raised) != 1 || raised[0].Reason != ReasonError {
		t.Fatalf("alarms = %+v, want the malformed line reported", raised)
	}
}

func TestHookProbeDeclaresItsTask(t *testing.T) {
	fe := &fakeExec{}
	hs := testHooks(fe)
	dir := t.TempDir()
	writeHook(t, filepath.Join(dir, "hooks"), "gpu", "echo\n")
	t.Setenv("BASHY_WATCH_DIR", dir)
	src, err := NewHookSource(hs, HookSpec{Script: "gpu", Locator: "gpu:0",
		Class: ClassService, Quiet: Duration(5 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	tasks := src.Tasks()
	if len(tasks) != 1 || tasks[0].Locator != "gpu:0" || tasks[0].Class != ClassService ||
		tasks[0].Quiet != 5*time.Minute {
		t.Fatalf("tasks = %+v", tasks)
	}
}

func TestHookRuleFileDeclaresProbesAndActions(t *testing.T) {
	dir := t.TempDir()
	writeHook(t, filepath.Join(dir, "hooks"), "gpu", "echo\n")
	writeHook(t, filepath.Join(dir, "hooks"), "reclaim", "echo\n")
	t.Setenv("BASHY_WATCH_DIR", dir)

	f, err := ParseRuleFile([]byte(`{
	  "schema_version": "bashy-watch-rules-v1",
	  "sources": [{"type": "hook", "script": "gpu", "locator": "gpu:0", "class": "service"}],
	  "rules": [{"type": "change", "fields": ["utilization"]}],
	  "sinks": [{"type": "hook", "script": "reclaim", "args": ["--apply"], "destructive": true, "confirm": true}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	fe := &fakeExec{}
	plan, err := f.Build(BuildOptions{Hooks: testHooks(fe)})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sources) != 1 || plan.Sources[0].Name() != "hook:gpu" {
		t.Fatalf("sources = %+v", plan.Sources)
	}
	if len(plan.Sinks) != 1 {
		t.Fatalf("sinks = %+v", plan.Sinks)
	}
	if err := plan.Sinks[0].Deliver(Alert{Rule: "change", State: StateFire}); err != nil {
		t.Fatal(err)
	}
	if fe.count() != 1 {
		t.Fatalf("exec calls = %d, want the rule file's action hook to have run", fe.count())
	}
}

func TestHookRuleFileWithoutAHookRunnerIsAClearError(t *testing.T) {
	f, err := ParseRuleFile([]byte(`{
	  "schema_version": "bashy-watch-rules-v1",
	  "sources": [{"type": "hook", "script": "gpu"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Build(BuildOptions{})
	if err == nil || !strings.Contains(err.Error(), "hook") {
		t.Fatalf("err = %v, want a clear refusal naming hooks", err)
	}
}

func TestHookShippedExamplesAreTheDocumentedTwo(t *testing.T) {
	// Examples, not verbs: a disk reclaim and a GPU probe, shipped so a user
	// has something to copy and override.
	names := HookNames()
	want := map[string]bool{"reclaim-go-build": false, "gpu": false}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for n, found := range want {
		if !found {
			t.Fatalf("hook %q is not shipped (have %q)", n, names)
		}
	}
}

func TestBashyExecNeverExecsTheTestBinary(t *testing.T) {
	// The one thing the injected exec cannot catch. os.Executable() here is the
	// test binary; the runner must look up the INSTALLED bashy on PATH.
	dir := t.TempDir()
	bashy := filepath.Join(dir, "bashy")
	if err := os.WriteFile(bashy, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got, err := LookBashy()
	if err != nil {
		t.Fatal(err)
	}
	if got != bashy {
		t.Fatalf("LookBashy = %q, want the bashy on PATH (%q)", got, bashy)
	}
	self, _ := os.Executable()
	if got == self {
		t.Fatal("the hook runner resolved to the test binary")
	}

	t.Setenv("PATH", t.TempDir())
	if _, err := LookBashy(); err == nil {
		t.Fatal("LookBashy succeeded with no bashy on PATH")
	}
}

func TestBashyExecRunsABashSharpScript(t *testing.T) {
	// The real runner, end to end, when a bashy is installed. Skipped rather
	// than faked: a green unit suite on a host without bashy must not claim
	// this works.
	if _, err := LookBashy(); err != nil {
		t.Skip("no installed bashy on PATH")
	}
	dir := t.TempDir()
	writeHook(t, filepath.Join(dir, "hooks"), "echoback", "cat\necho \"argv=$1\"\n")
	t.Setenv("BASHY_WATCH_DIR", dir)

	hs := NewHooks()
	h, err := hs.Resolve("echoback", HookAction)
	if err != nil {
		t.Fatal(err)
	}
	h.Args = []string{"--apply"}
	res := hs.Run(context.Background(), h, []byte("{\"rule\":\"idle\"}\n"))
	if res.Failed() {
		t.Fatalf("result = %+v (%s)", res, res.Stdout)
	}
	if !strings.Contains(string(res.Stdout), `"rule":"idle"`) {
		t.Fatalf("stdout = %q, want the stdin echoed back", res.Stdout)
	}
	if !strings.Contains(string(res.Stdout), "argv=--apply") {
		t.Fatalf("stdout = %q, want the fixed argv passed", res.Stdout)
	}
}

func TestEmbeddedExampleHooksRunUnderBashy(t *testing.T) {
	// The shipped examples are real Bash#, and a dry run of the reclaim must
	// not delete anything.
	if _, err := LookBashy(); err != nil {
		t.Skip("no installed bashy on PATH")
	}
	t.Setenv("BASHY_WATCH_DIR", t.TempDir())
	hs := NewHooks()
	for _, name := range []string{"gpu", "reclaim-go-build"} {
		kind := HookAction
		if name == "gpu" {
			kind = HookProbe
		}
		h, err := hs.Resolve(name, kind)
		if err != nil {
			t.Fatal(err)
		}
		res := hs.Run(context.Background(), h, []byte("{}\n"))
		if res.Err != nil || res.TimedOut {
			t.Fatalf("%s: %+v", name, res)
		}
		if res.Exit != 0 {
			t.Fatalf("%s exited %d: %s", name, res.Exit, res.Stdout)
		}
	}
}
