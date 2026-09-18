package llmbudget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}
func TestOpenAIOrganizationDocumentedFixtures(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	var calls int
	a := OpenAIOrganizationAdapter{Resolve: func(context.Context, string) (string, error) { return "fixture-secret", nil }, Client: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" || r.URL.Host != "api.openai.com" || r.Header.Get("Authorization") != "Bearer fixture-secret" {
			t.Fatal("unexpected request")
		}
		result := `{"object":"organization.usage.completions.result","input_tokens":120,"input_cached_tokens":20,"output_tokens":30,"num_model_requests":2}`
		if strings.HasSuffix(r.URL.Path, "costs") {
			result = `{"object":"organization.costs.result","amount":{"value":0.25,"currency":"usd"}}`
		}
		return response(200, fmt.Sprintf(`{"object":"page","data":[{"object":"bucket","start_time":%d,"end_time":%d,"results":[%s]}],"has_more":false,"next_page":null}`, now.Add(-time.Hour).Unix(), now.Unix(), result)), nil
	})}}
	c := SourceConfig{Provider: "openai", Account: "org", Pool: "org", Lane: LaneAPIKey, CredentialRef: "secret:admin"}
	s, e := a.Collect(context.Background(), c, now)
	if e != nil || s.Status != "ok" || calls != 2 {
		t.Fatal(s, e, calls)
	}
	values := map[string]float64{}
	for _, m := range s.Metrics {
		values[m.Name] = *m.Value
		if m.WindowStart == nil || m.WindowEnd == nil {
			t.Fatal("missing window")
		}
		if strings.HasPrefix(m.Name, "quota.") {
			t.Fatal("billing mislabeled quota")
		}
	}
	if values["usage.input_tokens"] != 120 || values["usage.cached_input_tokens"] != 20 || values["billing.spend"] != .25 {
		t.Fatal(values)
	}
	c.CredentialRef = ""
	calls = 0
	s, e = a.Collect(context.Background(), c, now)
	if e == nil || calls != 0 || s.Status != "auth_required" {
		t.Fatal("implicit auth", s, e, calls)
	}
}
func TestOpenAISourceFailureAndBoundedPagination(t *testing.T) {
	now := time.Now().UTC()
	c := SourceConfig{Provider: "openai", Account: "org", Pool: "org", Lane: LaneAPIKey, CredentialRef: "ref"}
	for _, code := range []int{401, 429, 302} {
		var calls int
		a := OpenAIOrganizationAdapter{Resolve: func(context.Context, string) (string, error) { return "not-in-output", nil }, Client: &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
			calls++
			r := response(code, `secret-response`)
			r.Header.Set("Retry-After", "120")
			r.Header.Set("Location", "https://foreign.invalid")
			return r, nil
		})}}
		s, e := a.Collect(context.Background(), c, now)
		if e == nil || len(s.Metrics) != 0 || calls != 2 || strings.Contains(e.Error(), "secret") {
			t.Fatal(code, s, e, calls)
		}
		if code == 429 && (s.RetryAt == nil || !s.RetryAt.Equal(now.Add(2*time.Minute))) {
			t.Fatal(s)
		}
	}
	calls := 0
	a := OpenAIOrganizationAdapter{Resolve: func(context.Context, string) (string, error) { return "key", nil }, Client: &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		return response(200, `{"object":"page","data":[],"has_more":true,"next_page":"same"}`), nil
	})}}
	s, e := a.Collect(context.Background(), c, now)
	if e == nil || calls > 8 || len(s.Metrics) != 0 {
		t.Fatal(s, e, calls)
	}
}
func TestClaudeBridgeScopeFreshnessAndContext(t *testing.T) {
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "bridge.json")
	snapshot := map[string]any{"schema_version": "bashy-claude-statusline-v1", "observed_at": now, "account": "claude-user", "payload": map[string]any{"cost": map[string]any{"total_cost_usd": 1.2}, "context_window": map[string]any{"remaining_percentage": 75, "current_usage": map[string]int{"input_tokens": 100, "output_tokens": 20}}, "rate_limits": map[string]any{"five_hour": map[string]any{"used_percentage": 40, "resets_at": now.Add(time.Hour).Unix()}, "seven_day": map[string]any{"used_percentage": 10}}}}
	write := func() {
		b, _ := json.Marshal(snapshot)
		if e := os.WriteFile(path, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	write()
	a := ClaudeStatuslineAdapter{}
	c := SourceConfig{Path: path, Account: "claude-user"}
	s, e := a.Collect(context.Background(), c, now)
	if e != nil || len(s.Metrics) != 6 {
		t.Fatal(s, e)
	}
	for _, m := range s.Metrics {
		if m.Name == "usage.input_tokens" {
			t.Fatal("context masquerades as cumulative usage")
		}
		if m.Name == "billing.spend" && m.Classification != "estimated" {
			t.Fatal(m)
		}
	}
	c.Account = "other"
	s, e = a.Collect(context.Background(), c, now)
	if e == nil {
		t.Fatal("account mismatch accepted")
	}
	c.Account = "claude-user"
	snapshot["observed_at"] = now.Add(-time.Hour)
	write()
	s, e = a.Collect(context.Background(), c, now)
	if e != nil || s.Status != "stale" {
		t.Fatal(s, e)
	}
}

type fixtureAdapter struct {
	count *atomic.Int32
	fail  *atomic.Bool
	hold  <-chan struct{}
}

func (f fixtureAdapter) Kind() string { return "fixture" }
func (f fixtureAdapter) Collect(ctx context.Context, c SourceConfig, at time.Time) (SourceResult, error) {
	f.count.Add(1)
	if f.hold != nil {
		select {
		case <-f.hold:
		case <-ctx.Done():
			return SourceResult{}, ctx.Err()
		}
	}
	if f.fail.Load() {
		return SourceResult{Status: "unavailable"}, errors.New("fixture failure")
	}
	return SourceResult{Status: "ok", Metrics: []Metric{measured("usage.requests", 7, "requests", "actual", "fixture", at)}}, nil
}
func TestSharedSourceCacheAndFailureBackoff(t *testing.T) {
	now := time.Now().UTC()
	var count atomic.Int32
	var fail atomic.Bool
	g := fixtureGate(t, &Policy{Version: 1})
	g.cfg.Adapters = []Adapter{fixtureAdapter{count: &count, fail: &fail}}
	g.cfg.Now = func() time.Time { return now }
	c := SourceConfig{ID: "fixture", Kind: "fixture", Enabled: true, RefreshSeconds: 60}
	for i := 0; i < 2; i++ {
		s, e := New(g.cfg).source(context.Background(), c, now, true)
		if e != nil || s.Status != "ok" {
			t.Fatal(s, e)
		}
	}
	if count.Load() != 1 {
		t.Fatal("cross-gate throttle bypass", count.Load())
	}
	fail.Store(true)
	now = now.Add(61 * time.Second)
	s, e := g.source(context.Background(), c, now, true)
	if e == nil || s.Status != "stale" || s.Metrics[0].Classification != "stale" || !s.Metrics[0].ObservedAt.Equal(now.Add(-61*time.Second)) {
		t.Fatal(s, e)
	}
	now = now.Add(time.Second)
	_, _ = g.source(context.Background(), c, now, true)
	if count.Load() != 2 {
		t.Fatal("failure backoff bypass")
	}
}
func TestSharedSourceRefreshDoesNotStampede(t *testing.T) {
	now := time.Now().UTC()
	var count atomic.Int32
	var fail atomic.Bool
	hold := make(chan struct{})
	g := fixtureGate(t, &Policy{Version: 1})
	g.cfg.Adapters = []Adapter{fixtureAdapter{count: &count, fail: &fail, hold: hold}}
	c := SourceConfig{ID: "fixture", Kind: "fixture", Enabled: true}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = g.source(context.Background(), c, now, false) }()
	deadline := time.Now().Add(time.Second)
	for count.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	s, e := New(g.cfg).source(context.Background(), c, now, false)
	close(hold)
	wg.Wait()
	if e != nil || s.Status != "unavailable" || count.Load() != 1 {
		t.Fatal(s, e, count.Load())
	}
}
func TestReportRosterFilteringPreservesPoolContext(t *testing.T) {
	p := fixturePolicy()
	g := fixtureGate(t, p)
	now := time.Now().UTC()
	r, e := g.CollectReport(context.Background(), ReportOptions{Model: "one", Roster: []Binding{{Model: "one", Agent: "alpha"}, {Model: "two", Agent: "beta"}, {Model: "unconfigured", Provider: "other"}}, Active: []Attribution{{Model: "two", Agent: "beta", Host: "remote", Run: "external", Active: true}}, Now: now})
	if e != nil || len(r.Accounts) != 1 {
		t.Fatal(r, e)
	}
	a := r.Accounts[0]
	if len(a.Models) != 2 || len(a.Agents) != 2 || len(a.Attribution) != 1 || !a.AccountKnown {
		t.Fatal(a)
	}
	for _, name := range []string{"quota.remaining", "limits.requests_per_minute", "usage.input_tokens"} {
		found := false
		for _, m := range a.Metrics {
			if m.Name == name {
				found = true
				if m.Value != nil || m.Classification != "unknown" {
					t.Fatal(m)
				}
			}
		}
		if !found {
			t.Fatal("unknown omitted", name)
		}
	}
	if _, e := os.Stat(g.cfg.StatePath); !os.IsNotExist(e) {
		t.Fatal("report mutated admission state")
	}
	r, e = g.CollectReport(context.Background(), ReportOptions{Roster: []Binding{{Model: "unconfigured", Provider: "other"}}, Now: now})
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, a := range r.Accounts {
		if a.Provider == "other" {
			found = true
			if a.AccountKnown || a.Status != "unsupported" {
				t.Fatal(a)
			}
		}
	}
	if !found {
		t.Fatal("unused roster member omitted")
	}
}

// Each source has its own shared file and lock. A report may reach its second
// source after a newer report has already refreshed it: never overwrite that
// newer observation using the earlier report timestamp.
type processSourceAdapter struct{ directory string }

func (processSourceAdapter) Kind() string { return "process-fixture" }
func (a processSourceAdapter) Collect(_ context.Context, c SourceConfig, at time.Time) (SourceResult, error) {
	f, err := os.OpenFile(filepath.Join(a.directory, c.ID+".calls"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return SourceResult{}, err
	}
	_, err = fmt.Fprintln(f, at.UnixNano())
	closeErr := f.Close()
	if err != nil {
		return SourceResult{}, err
	}
	if closeErr != nil {
		return SourceResult{}, closeErr
	}
	return SourceResult{Status: "ok", Metrics: []Metric{measured("usage.requests", 1, "requests", "actual", c.ID, at)}}, nil
}
func TestSharedSourceProcessHelper(t *testing.T) {
	directory := os.Getenv("LLMBUDGET_SOURCE_PROCESS_DIR")
	if directory == "" {
		return
	}
	at, err := time.Parse(time.RFC3339Nano, os.Getenv("LLMBUDGET_SOURCE_PROCESS_AT"))
	if err != nil {
		t.Fatal(err)
	}
	g := New(Config{StatePath: filepath.Join(directory, "meter.json"), Adapters: []Adapter{processSourceAdapter{directory}}})
	for _, id := range []string{"alpha", "beta"} {
		result, err := g.source(context.Background(), SourceConfig{ID: id, Kind: "process-fixture", Enabled: true, RefreshSeconds: 60}, at, true)
		if err != nil || result.Status != "ok" {
			t.Fatal(result, err)
		}
	}
}
func TestSharedSourceTwoSourcesEightProcessesEarlierReport(t *testing.T) {
	directory := t.TempDir()
	at := time.Now().UTC()
	run := func(when time.Time) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSharedSourceProcessHelper$")
		cmd.Env = append(os.Environ(), "LLMBUDGET_SOURCE_PROCESS_DIR="+directory, "LLMBUDGET_SOURCE_PROCESS_AT="+when.Format(time.RFC3339Nano))
		return cmd
	}
	if out, err := run(at.Add(-time.Minute - time.Second)).CombinedOutput(); err != nil {
		t.Fatalf("seed: %v %s", err, out)
	}
	// One of eight clients wins both refresh locks with a slightly newer report.
	if out, err := run(at.Add(5 * time.Millisecond)).CombinedOutput(); err != nil {
		t.Fatalf("winner: %v %s", err, out)
	}
	var wg sync.WaitGroup
	for i := 0; i < 7; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if out, err := run(at).CombinedOutput(); err != nil {
				t.Errorf("reader: %v %s", err, out)
			}
		}()
	}
	wg.Wait()
	for _, id := range []string{"alpha", "beta"} {
		b, err := os.ReadFile(filepath.Join(directory, id+".calls"))
		if err != nil {
			t.Fatal(err)
		}
		if got := len(strings.Fields(string(b))); got != 2 {
			t.Errorf("%s refreshed %d times; want initial + one cadence refresh", id, got)
		}
	}
}

func TestSharedSourceRejectsGenuineFutureObservation(t *testing.T) {
	now := time.Now().UTC()
	var count atomic.Int32
	var fail atomic.Bool
	g := fixtureGate(t, &Policy{Version: 1})
	g.cfg.Adapters = []Adapter{fixtureAdapter{count: &count, fail: &fail}}
	g.cfg.Now = func() time.Time { return now }
	c := SourceConfig{ID: "future", Kind: "fixture", Enabled: true, RefreshSeconds: 60}
	if _, err := g.source(context.Background(), c, now.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if _, err := g.source(context.Background(), c, now, false); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 2 {
		t.Fatal("future observation reused", count.Load())
	}
}
