package weave

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/llmbudget"
	"github.com/qiangli/yoke/pkg/quotaobs"
	"github.com/spf13/cobra"
)

func withQuotaImmuneFakes(t *testing.T) {
	t.Helper()
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	now, policy := quotaImmuneNow, quotaImmunePolicy
	headroom, observe := quotaImmuneHeadroom, quotaImmuneObserve
	forecast, resolve := quotaImmuneForecast, quotaImmuneResolveSeat
	read, observed := quotaImmuneRead, quotaImmuneObserved
	delta, classify := quotaImmuneDelta, quotaImmuneClassify
	quotaImmuneDelta = nil // Legacy fake observations in existing decision tests.
	say, kill, send := quotaImmuneSay, quotaImmuneKill, weaveSupervisionSend
	t.Cleanup(func() {
		quotaImmuneNow, quotaImmunePolicy = now, policy
		quotaImmuneHeadroom, quotaImmuneObserve = headroom, observe
		quotaImmuneForecast, quotaImmuneResolveSeat = forecast, resolve
		quotaImmuneRead, quotaImmuneObserved = read, observed
		quotaImmuneDelta, quotaImmuneClassify = delta, classify
		quotaImmuneSay, quotaImmuneKill, weaveSupervisionSend = say, kill, send
	})
}

func TestQuotaImmuneSharesInputsWithinTick(t *testing.T) {
	withQuotaImmuneFakes(t)
	root := t.TempDir()
	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		q.Items = []*weaveItem{
			{ID: 1, State: "working", LaunchSpec: &weaveLaunchSpec{Agent: "one"}},
			{ID: 2, State: "working", LaunchSpec: &weaveLaunchSpec{Agent: "two"}},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	quotaImmuneNow = func() time.Time { return now }
	quotaImmunePolicy = func() (llmbudget.ImmuneSettings, error) { return llmbudget.ImmuneSettings{}, nil }
	quotaImmuneResolveSeat = func(agent, _, _ string) (quotaImmuneSeat, bool) {
		return quotaImmuneSeat{Agent: agent, Account: "plan"}, true
	}
	quotaImmuneHeadroom = func(context.Context, string, string) (float64, llmbudget.Binding, bool, error) {
		return 0, llmbudget.Binding{}, false, nil
	}
	quotaImmuneObserve = readQuotaImmuneObservation
	var reads, classifications, forecasts int
	quotaImmuneRead = func(time.Time, quotaobs.Filter) ([]quotaobs.Observation, error) {
		reads++
		return nil, nil
	}
	quotaImmuneObserved = func(context.Context, time.Time) []llmbudget.ObservedState {
		classifications++
		return nil
	}
	quotaImmuneForecast = func(context.Context, time.Time) []llmbudget.AccountForecast {
		forecasts++
		return nil
	}
	cmd, _ := immuneTestCommand()
	for tick := 1; tick <= 2; tick++ {
		if actions, err := runQuotaImmuneTick(cmd, root, dir, true); err != nil || len(actions) != 0 {
			t.Fatalf("tick %d: actions=%+v err=%v", tick, actions, err)
		}
		if reads != tick || classifications != tick || forecasts != tick {
			t.Fatalf("tick %d scans: observations=%d classifications=%d forecasts=%d", tick, reads, classifications, forecasts)
		}
	}
}

func immuneTestCommand() (*cobra.Command, *bytes.Buffer) {
	buf := new(bytes.Buffer)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	return cmd, buf
}

func TestQuotaImmuneDeltaRestartAndRotation(t *testing.T) {
	withQuotaImmuneFakes(t)
	quotaImmuneDelta = quotaobs.ReadDelta
	root := t.TempDir()
	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		q.Items = []*weaveItem{{ID: 1, State: "working", LaunchSpec: &weaveLaunchSpec{Agent: "agent"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	quotaImmuneNow = func() time.Time { return now }
	quotaImmunePolicy = func() (llmbudget.ImmuneSettings, error) { return llmbudget.ImmuneSettings{GracePeriod: time.Hour}, nil }
	quotaImmuneResolveSeat = func(string, string, string) (quotaImmuneSeat, bool) { return quotaImmuneSeat{Account: "plan"}, true }
	quotaImmuneHeadroom = func(context.Context, string, string) (float64, llmbudget.Binding, bool, error) {
		return 0, llmbudget.Binding{}, false, nil
	}
	quotaImmuneForecast = func(context.Context, time.Time) []llmbudget.AccountForecast { return nil }
	quotaImmuneObserve = readQuotaImmuneObservation
	quotaImmuneClassify = func(rows []quotaobs.Observation, _ time.Time) []llmbudget.ObservedState {
		out := make([]llmbudget.ObservedState, 0, len(rows))
		for _, o := range rows {
			out = append(out, llmbudget.ObservedState{Account: o.Account, Plan: o.Plan, Tool: o.Tool, At: o.At, ExcerptSHA: o.ExcerptSHA, Kind: o.Parsed.Kind, ResetAt: o.Parsed.ResetAt, Matcher: o.Parsed, Classification: "observed"})
		}
		return out
	}
	quotaImmuneSay = func(*cobra.Command, int64, string) error { return nil }
	weaveSupervisionSend = func(string, string) error { return nil }
	read := quotaImmuneDelta
	var counts []int
	quotaImmuneDelta = func(c quotaobs.Cursor) ([]quotaobs.Observation, quotaobs.Cursor, error) {
		rows, next, err := read(c)
		counts = append(counts, len(rows))
		return rows, next, err
	}
	cmd, _ := immuneTestCommand()
	reset := now.Add(time.Hour)
	add := func(kind string) {
		t.Helper()
		if err := quotaobs.Record(context.Background(), quotaobs.Observation{At: now, Account: "plan", Excerpt: kind, Parsed: &quotaobs.ParsedSignal{Kind: kind, ResetAt: &reset}}); err != nil {
			t.Fatal(err)
		}
	}
	add("warning")
	if a, err := runQuotaImmuneTick(cmd, root, dir, true); err != nil || len(a) != 1 || !a[0].DryRun {
		t.Fatalf("dry run: %+v %v", a, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "quota-immune-cursor.json")); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote cursor: %v", err)
	}
	if a, err := runQuotaImmuneTick(cmd, root, dir, false); err != nil || len(a) != 1 || a[0].Level != "warn" {
		t.Fatalf("first: %+v %v", a, err)
	}
	if a, err := runQuotaImmuneTick(cmd, root, dir, false); err != nil || len(a) != 0 {
		t.Fatalf("restart: %+v %v", a, err)
	}
	if len(counts) != 3 || counts[0] != 1 || counts[1] != 1 || counts[2] != 0 {
		t.Fatalf("delta counts: %v", counts)
	}
	add("exhausted")
	if a, err := runQuotaImmuneTick(cmd, root, dir, false); err != nil || len(a) != 1 || a[0].Level != "critical" {
		t.Fatalf("critical: %+v %v", a, err)
	}
	if counts[3] != 1 {
		t.Fatalf("third delta: %v", counts)
	}
	// Simulate the journal's rename rotation while a cursor is persisted.
	p := filepath.Join(os.Getenv("BASHY_HOME"), "quota", "observations.jsonl")
	if err := os.Rename(p, p+".1"); err != nil {
		t.Fatal(err)
	}
	add("warning again")
	if a, err := runQuotaImmuneTick(cmd, root, dir, false); err != nil || len(a) != 0 {
		t.Fatalf("rotation replay: %+v %v", a, err)
	}
	if counts[4] != 1 {
		t.Fatalf("rotation delta: %v", counts)
	}
}

func TestQuotaImmuneWarnCriticalGraceAndDedupe(t *testing.T) {
	withQuotaImmuneFakes(t)
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(workspace, "partial.txt")
	if err := os.WriteFile(work, []byte("preserve me"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	item := &weaveItem{ID: 7, State: "working", Tool: "tool", Workspace: workspace, Created: now.Add(-time.Hour), StartedAt: now.Add(-10 * time.Minute), LaunchSpec: &weaveLaunchSpec{Agent: "agent", Model: "model", MaxRuntime: time.Hour}}
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error { q.Items = []*weaveItem{item}; return nil }); err != nil {
		t.Fatal(err)
	}
	store, err := sprintStoreDir()
	if err != nil {
		t.Fatal(err)
	}
	story := &weaveStory{ID: 44, Lease: &weaveStoryLease{Holder: "manager", At: now}, Runs: []sprintRun{{ID: item.ID, Queue: filepath.Base(dir), Born: item.Created}}}
	if err := withWeaveQueueLock(store, func(q *weaveQueue) error { q.Stories = []*weaveStory{story}; return nil }); err != nil {
		t.Fatal(err)
	}
	remaining := 0.14
	quotaImmuneNow = func() time.Time { return now }
	quotaImmunePolicy = func() (llmbudget.ImmuneSettings, error) {
		return llmbudget.ImmuneSettings{WarnHeadroom: .15, CriticalHeadroom: .05, ForecastMargin: 15 * time.Minute, GracePeriod: 3 * time.Minute}, nil
	}
	quotaImmuneResolveSeat = func(string, string, string) (quotaImmuneSeat, bool) {
		return quotaImmuneSeat{Agent: "agent", Tool: "tool", Model: "model", Account: "plan-a"}, true
	}
	quotaImmuneHeadroom = func(context.Context, string, string) (float64, llmbudget.Binding, bool, error) {
		return remaining, llmbudget.Binding{}, true, nil
	}
	quotaImmuneObserve = func(context.Context, quotaImmuneSeat, time.Time) (quotaImmuneObservation, bool, error) {
		return quotaImmuneObservation{}, false, nil
	}
	quotaImmuneForecast = func(context.Context, time.Time) []llmbudget.AccountForecast {
		return nil
	}
	var says, alarms, kills int
	quotaImmuneSay = func(*cobra.Command, int64, string) error { says++; return nil }
	weaveSupervisionSend = func(string, string) error { alarms++; return nil }
	quotaImmuneKill = func(_ *cobra.Command, id int64) error {
		kills++
		if _, err := os.Stat(work); err != nil {
			t.Fatalf("kill hook did not receive preserved workspace: %v", err)
		}
		return withWeaveQueueLock(dir, func(q *weaveQueue) error {
			findWeaveItem(q, id).State = "killed"
			return nil
		})
	}
	cmd, _ := immuneTestCommand()
	actions, err := runQuotaImmuneTick(cmd, root, dir, false)
	if err != nil || len(actions) != 1 || actions[0].Level != "warn" {
		t.Fatalf("warn actions=%+v err=%v", actions, err)
	}
	if actions, err = runQuotaImmuneTick(cmd, root, dir, false); err != nil || len(actions) != 0 {
		t.Fatalf("warn dedupe actions=%+v err=%v", actions, err)
	}
	remaining = .04
	if actions, err = runQuotaImmuneTick(cmd, root, dir, false); err != nil || len(actions) != 1 || actions[0].Level != "critical" || actions[0].Kind != "steer" {
		t.Fatalf("critical actions=%+v err=%v", actions, err)
	}
	if kills != 0 {
		t.Fatal("critical killed before grace")
	}
	now = now.Add(3 * time.Minute)
	if actions, err = runQuotaImmuneTick(cmd, root, dir, false); err != nil || len(actions) != 1 || actions[0].Kind != "kill-preserve" {
		t.Fatalf("post-grace actions=%+v err=%v", actions, err)
	}
	if says != 2 || alarms != 2 || kills != 1 {
		t.Fatalf("calls say=%d alarm=%d kill=%d", says, alarms, kills)
	}
	q, _ := loadWeaveQueue(dir)
	record := q.Items[0].QuotaImmune
	if !record.NeedsSalvage || !record.NeedsReassignment || record.Reason != "quota-critical" || q.Items[0].StewardReason == "" {
		t.Fatalf("kill handoff record=%+v steward=%q", record, q.Items[0].StewardReason)
	}
	if got, err := os.ReadFile(work); err != nil || string(got) != "preserve me" {
		t.Fatalf("workspace was not preserved: %q %v", got, err)
	}
}

func TestQuotaImmuneForecastThresholdsAndDryRun(t *testing.T) {
	withQuotaImmuneFakes(t)
	t.Setenv("BASHY_HOME", t.TempDir())
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	quotaImmuneHeadroom = func(context.Context, string, string) (float64, llmbudget.Binding, bool, error) {
		return 0, llmbudget.Binding{}, false, nil
	}
	quotaImmuneObserve = func(context.Context, quotaImmuneSeat, time.Time) (quotaImmuneObservation, bool, error) {
		return quotaImmuneObservation{}, false, nil
	}
	quotaImmuneForecast = func(context.Context, time.Time) []llmbudget.AccountForecast {
		p50, p90 := now.Add(50*time.Minute), now.Add(20*time.Minute)
		return []llmbudget.AccountForecast{{Account: "a", P50: &p50, P90: &p90}}
	}
	policy := llmbudget.ImmuneSettings{WarnHeadroom: .2, CriticalHeadroom: .1, ForecastMargin: 15 * time.Minute, GracePeriod: 3 * time.Minute}
	a, err := assessQuotaImmune(context.Background(), quotaImmuneSeat{Account: "a"}, now.Add(40*time.Minute), now, policy)
	if err != nil || a.Level != "warn" || !strings.Contains(a.Detail, "P50") {
		t.Fatalf("forecast warn=%+v err=%v", a, err)
	}
	quotaImmuneForecast = func(context.Context, time.Time) []llmbudget.AccountForecast {
		p90 := now.Add(10 * time.Minute)
		return []llmbudget.AccountForecast{{Account: "a", P90: &p90}}
	}
	a, err = assessQuotaImmune(context.Background(), quotaImmuneSeat{Account: "a"}, time.Time{}, now, policy)
	if err != nil || a.Level != "critical" || !strings.Contains(a.Detail, "P90") {
		t.Fatalf("forecast critical=%+v err=%v", a, err)
	}

	root := filepath.Join(t.TempDir(), "repo")
	_ = os.MkdirAll(root, 0o755)
	dir, _ := weaveQueueDir(root)
	item := &weaveItem{ID: 1, State: "working", StartedAt: now, LaunchSpec: &weaveLaunchSpec{Agent: "a", MaxRuntime: time.Hour}}
	_ = withWeaveQueueLock(dir, func(q *weaveQueue) error { q.Items = []*weaveItem{item}; return nil })
	quotaImmuneNow = func() time.Time { return now }
	quotaImmunePolicy = func() (llmbudget.ImmuneSettings, error) { return policy, nil }
	quotaImmuneResolveSeat = func(string, string, string) (quotaImmuneSeat, bool) { return quotaImmuneSeat{Account: "a"}, true }
	var sideEffects int
	quotaImmuneSay = func(*cobra.Command, int64, string) error { sideEffects++; return nil }
	weaveSupervisionSend = func(string, string) error { sideEffects++; return nil }
	cmd, _ := immuneTestCommand()
	actions, err := runQuotaImmuneTick(cmd, root, dir, true)
	if err != nil || len(actions) != 1 || actions[0].Level != "critical" || !actions[0].DryRun || sideEffects != 0 {
		t.Fatalf("dry run actions=%+v effects=%d err=%v", actions, sideEffects, err)
	}
	q, _ := loadWeaveQueue(dir)
	if q.Items[0].QuotaImmune != nil || len(q.Items[0].Comments) != 0 {
		t.Fatalf("dry run mutated item: %+v", q.Items[0])
	}
}

func TestQuotaImmuneObservedWarningAndExhaustion(t *testing.T) {
	withQuotaImmuneFakes(t)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	quotaImmuneHeadroom = func(context.Context, string, string) (float64, llmbudget.Binding, bool, error) {
		return 0, llmbudget.Binding{}, false, nil
	}
	quotaImmuneForecast = func(context.Context, time.Time) []llmbudget.AccountForecast {
		return nil
	}
	level := "warn"
	quotaImmuneObserve = func(context.Context, quotaImmuneSeat, time.Time) (quotaImmuneObservation, bool, error) {
		return quotaImmuneObservation{Level: level, Detail: "used 90% of weekly quota", ResetAt: now.Add(time.Hour)}, true, nil
	}
	policy := llmbudget.ImmuneSettings{WarnHeadroom: .15, CriticalHeadroom: .05, ForecastMargin: 15 * time.Minute, GracePeriod: 3 * time.Minute}
	a, err := assessQuotaImmune(context.Background(), quotaImmuneSeat{Account: "vendor-plan"}, time.Time{}, now, policy)
	if err != nil || a.Level != "warn" || a.ResetAt != now.Add(time.Hour) {
		t.Fatalf("observed warning=%+v err=%v", a, err)
	}
	level = "critical"
	a, err = assessQuotaImmune(context.Background(), quotaImmuneSeat{Account: "vendor-plan"}, time.Time{}, now, policy)
	if err != nil || a.Level != "critical" {
		t.Fatalf("observed exhausted=%+v err=%v", a, err)
	}
}

func TestQuotaImmuneClassifiedObservations(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	used := 91.0
	policy := llmbudget.ImmuneSettings{WarnHeadroom: .15, CriticalHeadroom: .05}
	tests := []struct {
		name string
		obs  quotaobs.Observation
		want string
	}{
		{"empty kind narration", quotaobs.Observation{Excerpt: "quota exceeded, reset in one hour"}, ""},
		{"observed exhausted", quotaobs.Observation{Excerpt: "quota exhausted", Parsed: &quotaobs.ParsedSignal{Kind: "exhausted", ResetAt: &reset}, Analysis: &quotaobs.Analysis{Kind: "exhausted", ResetAt: &reset}}, "critical"},
		{"inferred exhausted", quotaobs.Observation{Excerpt: "quota exhausted", Analysis: &quotaobs.Analysis{Kind: "exhausted", ResetAt: &reset}}, ""},
		{"unconfirmed prose", quotaobs.Observation{Excerpt: "quota exhausted", Parsed: &quotaobs.ParsedSignal{Kind: "exhausted", ResetAt: &reset}}, ""},
		{"conflict", quotaobs.Observation{Excerpt: "quota exhausted", Parsed: &quotaobs.ParsedSignal{Kind: "exhausted", ResetAt: &reset}, Analysis: &quotaobs.Analysis{Kind: "warning", ResetAt: &reset}}, ""},
		{"observed percent", quotaobs.Observation{Excerpt: "weekly utilization 91%", Parsed: &quotaobs.ParsedSignal{Kind: "rate-limited", UsedPercent: &used}, Analysis: &quotaobs.Analysis{Kind: "rate-limited", UsedPercent: &used}}, "warn"},
		{"claude weekly utilization", quotaobs.Observation{Tool: "claude", Excerpt: `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","rateLimitType":"seven_day","resetsAt":1792069200,"unifiedWindows":{"seven_day":{"utilization":0.91,"resetsAt":1792069200}}}}`, Parsed: &quotaobs.ParsedSignal{Kind: "warning", UsedPercent: &used, ResetAt: &reset}}, "warn"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withQuotaImmuneFakes(t)
			t.Setenv("BASHY_HOME", t.TempDir())
			quotaImmunePolicy = func() (llmbudget.ImmuneSettings, error) { return policy, nil }
			quotaImmuneHeadroom = func(context.Context, string, string) (float64, llmbudget.Binding, bool, error) {
				return 0, llmbudget.Binding{}, false, nil
			}
			quotaImmuneForecast = func(context.Context, time.Time) []llmbudget.AccountForecast { return nil }
			obs := tc.obs
			obs.At, obs.Source, obs.Account = now, "weave", "plan-a"
			if err := quotaobs.Record(context.Background(), obs); err != nil {
				t.Fatal(err)
			}
			quotaImmuneObserve = readQuotaImmuneObservation
			a, err := assessQuotaImmune(context.Background(), quotaImmuneSeat{Account: "plan-a", Tool: "claude"}, time.Time{}, now, policy)
			if err != nil || a.Level != tc.want {
				t.Fatalf("assessment=%+v, want level %q, err=%v", a, tc.want, err)
			}
			if tc.name == "empty kind narration" {
				root := filepath.Join(t.TempDir(), "repo")
				if err := os.MkdirAll(root, 0o755); err != nil {
					t.Fatal(err)
				}
				dir, err := weaveQueueDir(root)
				if err != nil {
					t.Fatal(err)
				}
				if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
					q.Items = []*weaveItem{{ID: 1, State: "working", LaunchSpec: &weaveLaunchSpec{Agent: "agent"}}}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				quotaImmuneNow = func() time.Time { return now }
				quotaImmuneResolveSeat = func(string, string, string) (quotaImmuneSeat, bool) {
					return quotaImmuneSeat{Account: "plan-a"}, true
				}
				cmd, _ := immuneTestCommand()
				actions, err := runQuotaImmuneTick(cmd, root, dir, true)
				if err != nil || len(actions) != 0 {
					t.Fatalf("empty-kind observation caused actions=%+v err=%v", actions, err)
				}
			}
		})
	}
}

func TestWeaveImmuneDryRunCLIPrintsWithoutMutation(t *testing.T) {
	withQuotaImmuneFakes(t)
	t.Setenv("BASHY_HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, root, "init", "-q")
	t.Chdir(root)
	dir, _ := weaveQueueDir(root)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	item := &weaveItem{ID: 2, State: "working", StartedAt: now, LaunchSpec: &weaveLaunchSpec{Agent: "a", MaxRuntime: time.Hour}}
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error { q.Items = []*weaveItem{item}; return nil }); err != nil {
		t.Fatal(err)
	}
	quotaImmuneNow = func() time.Time { return now }
	quotaImmunePolicy = func() (llmbudget.ImmuneSettings, error) {
		return llmbudget.ImmuneSettings{WarnHeadroom: .15, CriticalHeadroom: .05, ForecastMargin: 15 * time.Minute, GracePeriod: 3 * time.Minute}, nil
	}
	quotaImmuneResolveSeat = func(string, string, string) (quotaImmuneSeat, bool) { return quotaImmuneSeat{Account: "plan"}, true }
	quotaImmuneHeadroom = func(context.Context, string, string) (float64, llmbudget.Binding, bool, error) {
		return .01, llmbudget.Binding{}, true, nil
	}
	quotaImmuneObserve = func(context.Context, quotaImmuneSeat, time.Time) (quotaImmuneObservation, bool, error) {
		return quotaImmuneObservation{}, false, nil
	}
	quotaImmuneForecast = func(context.Context, time.Time) []llmbudget.AccountForecast {
		return nil
	}
	cmd := NewWeaveCmd()
	out := new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"immune", "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err, out.String())
	}
	if !strings.Contains(out.String(), "would fire: critical steer for run #2") {
		t.Fatalf("dry-run output: %s", out.String())
	}
	q, _ := loadWeaveQueue(dir)
	if q.Items[0].QuotaImmune != nil {
		t.Fatal("dry-run CLI mutated queue")
	}
}

func TestQuotaImmuneConductorAlarmRecordedOnce(t *testing.T) {
	withQuotaImmuneFakes(t)
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	store, _ := sprintStoreDir()
	s := &weaveStory{ID: 44, Column: "doing", Lease: &weaveStoryLease{Holder: "manager", At: now}, Boxes: []weaveStoryBox{{StartedAt: now.Add(-time.Hour), Cutoff: now.Add(time.Hour), Planned: 2 * time.Hour}}}
	if err := withWeaveQueueLock(store, func(q *weaveQueue) error { q.Stories = []*weaveStory{s}; return nil }); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "repo")
	_ = os.MkdirAll(root, 0o755)
	dir, _ := weaveQueueDir(root)
	quotaImmuneNow = func() time.Time { return now }
	quotaImmunePolicy = func() (llmbudget.ImmuneSettings, error) {
		return llmbudget.ImmuneSettings{WarnHeadroom: .15, CriticalHeadroom: .05, ForecastMargin: 15 * time.Minute, GracePeriod: 3 * time.Minute}, nil
	}
	quotaImmuneResolveSeat = func(agent, _, _ string) (quotaImmuneSeat, bool) {
		return quotaImmuneSeat{Agent: agent, Model: "m", Account: "manager-plan"}, true
	}
	quotaImmuneHeadroom = func(context.Context, string, string) (float64, llmbudget.Binding, bool, error) {
		return .1, llmbudget.Binding{}, true, nil
	}
	quotaImmuneObserve = func(context.Context, quotaImmuneSeat, time.Time) (quotaImmuneObservation, bool, error) {
		return quotaImmuneObservation{}, false, nil
	}
	quotaImmuneForecast = func(context.Context, time.Time) []llmbudget.AccountForecast {
		return nil
	}
	var targets []string
	weaveSupervisionSend = func(target, _ string) error { targets = append(targets, target); return nil }
	cmd, _ := immuneTestCommand()
	actions, err := runQuotaImmuneTick(cmd, root, dir, false)
	if err != nil || len(actions) != 1 || actions[0].Kind != "conductor-alarm" || len(targets) != 1 || targets[0] != "manager" {
		t.Fatalf("actions=%+v targets=%v err=%v", actions, targets, err)
	}
	actions, err = runQuotaImmuneTick(cmd, root, dir, false)
	if err != nil || len(actions) != 0 || len(targets) != 1 {
		t.Fatalf("dedupe actions=%+v targets=%v err=%v", actions, targets, err)
	}
	board, _ := loadWeaveQueue(store)
	if len(board.Stories[0].Thread) != 1 || board.Stories[0].Thread[0].Kind != "quota-warn" {
		t.Fatalf("thread=%+v", board.Stories[0].Thread)
	}
}
