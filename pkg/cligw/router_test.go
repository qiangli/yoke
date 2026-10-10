package cligw

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmgw/sched"
)

type fakeQuota struct {
	mu            sync.Mutex
	headroom      map[string]float64
	headroomCalls map[string]int
	refused       map[string]string
	refuseFirst   int
	previewDelay  time.Duration
	previewCalls  []string
}

func (q *fakeQuota) Headroom(_ context.Context, agent Agent) (float64, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.headroomCalls == nil {
		q.headroomCalls = map[string]int{}
	}
	q.headroomCalls[agent.Model]++
	v, ok := q.headroom[agent.Name]
	if !ok {
		v, ok = q.headroom[agent.Model]
	}
	return v, ok
}
func (q *fakeQuota) Preview(_ context.Context, agent Agent) (bool, string) {
	q.mu.Lock()
	q.previewCalls = append(q.previewCalls, agent.Name)
	call := len(q.previewCalls)
	delay := q.previewDelay
	reason, refused := q.refused[agent.Name]
	refused = refused || call <= q.refuseFirst
	q.mu.Unlock()
	time.Sleep(delay)
	return !refused, reason
}

func (q *fakeQuota) previews() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.previewCalls...)
}

func (q *fakeQuota) headrooms(model string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.headroomCalls[model]
}

func expireHeadroom(q QuotaSource) {
	cache := q.(*headroomCache)
	cache.mu.Lock()
	cache.entries = map[string]headroomEntry{}
	cache.mu.Unlock()
}

type fakePool map[string][2]int

func (p fakePool) Idle(agent string) int   { return p[agent][0] }
func (p fakePool) Queued(agent string) int { return p[agent][1] }

type memoryRecorder struct{ decisions []Decision }

func (r *memoryRecorder) Record(_ context.Context, d Decision) error {
	r.decisions = append(r.decisions, d)
	return nil
}

func newTestRouter(t *testing.T, policy Policy, quota *fakeQuota, options ...RouterOption) (*Router, *FleetCatalog, *memoryRecorder) {
	t.Helper()
	cat := testFleet(t)
	recorder := &memoryRecorder{}
	base := []RouterOption{WithQuotaSource(quota), WithRecorder(recorder)}
	base = append(base, options...)
	return NewRouter(cat, policy, base...), cat, recorder
}

func selector(t *testing.T, cat *FleetCatalog, value string) Selector {
	t.Helper()
	sel, err := cat.ParseModelSelector(value)
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

func TestRouterQuotaFirstAndBreakerFallback(t *testing.T) {
	quota := &fakeQuota{headroom: map[string]float64{"strong": .8, "small": .2}}
	breaker := sched.NewBreaker()
	router, cat, recorder := newTestRouter(t, DefaultPolicy(), quota, WithBreaker(breaker))

	got, err := router.Route(context.Background(), selector(t, cat, "L4"), Filter{}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != "gamma" || len(got.Ranked) != 2 || got.Ranked[0].Score.Headroom != .8 {
		t.Fatalf("quota-first decision = %+v", got)
	}
	if !strings.HasPrefix(got.Header(), "X-Bashy-Routed: agent=gamma, band=L4, reason=") {
		t.Fatalf("Header() = %q", got.Header())
	}
	breaker.Trip("gamma", 1)
	got, err = router.Route(context.Background(), selector(t, cat, "L4"), Filter{}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != "x-cascade" {
		t.Fatalf("breaker fallback = %q, want x-cascade", got.Agent)
	}
	if len(recorder.decisions) != 2 || len(router.History()) != 2 {
		t.Fatalf("recorded=%d history=%d", len(recorder.decisions), len(router.History()))
	}
}

func TestHeadroomRanksKnownAboveFloorThenUnknownThenBelowFloor(t *testing.T) {
	router := &Router{policy: DefaultPolicy()}
	candidates := []Candidate{
		{Agent: "below", Score: CandidateScore{Headroom: .10, HeadroomKnown: true, BelowReserve: true}},
		{Agent: "unknown", Score: CandidateScore{}},
		{Agent: "above", Score: CandidateScore{Headroom: .40, HeadroomKnown: true}},
	}
	router.rank(candidates, PolicyQuotaFirst, Selector{Raw: "L4"}, 4)
	if got := []string{candidates[0].Agent, candidates[1].Agent, candidates[2].Agent}; got[0] != "above" || got[1] != "unknown" || got[2] != "below" {
		t.Fatalf("headroom order = %v, want [above unknown below]", got)
	}
}

func TestRankingReasonPrintsUnknownHeadroomHonestly(t *testing.T) {
	router, cat, _ := newTestRouter(t, DefaultPolicy(), &fakeQuota{headroom: map[string]float64{}})
	decision, err := router.Route(context.Background(), selector(t, cat, "L4"), Filter{}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	got := decision.Header()
	if !strings.Contains(got, "headroom=unknown") || strings.Contains(got, "headroom=0.00") {
		t.Fatalf("Header() = %q, want literal unknown headroom", got)
	}
}

func TestRouterReserveFloorReturns429(t *testing.T) {
	quota := &fakeQuota{headroom: map[string]float64{"strong": .10, "small": .14}}
	router, cat, _ := newTestRouter(t, DefaultPolicy(), quota)
	_, err := router.Route(context.Background(), selector(t, cat, "L4"), Filter{}, "", "")
	var routeErr *RouteError
	if !errors.As(err, &routeErr) || routeErr.Status != 429 {
		t.Fatalf("error = %T %v, want RouteError 429", err, err)
	}
}

func TestRouterL4PlusUsesNextBandWhenL4Unavailable(t *testing.T) {
	quota := &fakeQuota{headroom: map[string]float64{"strong": .8, "small": .7, "frontier": .6}}
	breaker := sched.NewBreaker()
	breaker.Trip("gamma", 1)
	breaker.Trip("x-cascade", 1)
	router, cat, _ := newTestRouter(t, DefaultPolicy(), quota, WithBreaker(breaker))
	got, err := router.Route(context.Background(), selector(t, cat, "L4+"), Filter{}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != "omega" || got.Band != "L5" {
		t.Fatalf("L4+ fallback = %+v, want omega/L5", got)
	}
}

func TestRouterRoundRobinPreferAndAffinity(t *testing.T) {
	quota := &fakeQuota{headroom: map[string]float64{"strong": .8, "small": .8}}
	pool := fakePool{"gamma": {1, 0}, "x-cascade": {1, 0}}
	router, cat, _ := newTestRouter(t, DefaultPolicy(), quota, WithPoolState(pool))
	sel := selector(t, cat, "L4")

	first, err := router.Route(context.Background(), sel, Filter{}, PolicyRoundRobin, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := router.Route(context.Background(), sel, Filter{}, PolicyRoundRobin, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Agent != "gamma" || second.Agent != "x-cascade" {
		t.Fatalf("round robin = %q then %q", first.Agent, second.Agent)
	}

	preferred, err := router.Route(context.Background(), sel, Filter{}, "prefer:alpha-tool", "")
	if err != nil {
		t.Fatal(err)
	}
	if preferred.Agent != "x-cascade" || !preferred.Ranked[0].Score.Preferred {
		t.Fatalf("prefer decision = %+v", preferred)
	}

	sticky1, err := router.Route(context.Background(), sel, Filter{}, PolicyQuotaFirst, "conversation")
	if err != nil {
		t.Fatal(err)
	}
	sticky2, err := router.Route(context.Background(), sel, Filter{}, PolicyQuotaFirst, "conversation")
	if err != nil {
		t.Fatal(err)
	}
	if sticky1.Agent != "gamma" || sticky2.Agent != "gamma" || !sticky2.Ranked[0].Score.Affinity {
		t.Fatalf("affinity decisions = %+v then %+v", sticky1, sticky2)
	}
}

func TestRouterLatencyFirstAndProviderWeight(t *testing.T) {
	quota := &fakeQuota{headroom: map[string]float64{"strong": .9, "small": .4}}
	pool := fakePool{"gamma": {0, 0}, "x-cascade": {1, 2}}
	policy := DefaultPolicy()
	policy.ProviderWeights["anthropic"] = 5
	router, cat, _ := newTestRouter(t, policy, quota, WithPoolState(pool))
	sel := selector(t, cat, "L4")

	got, err := router.Route(context.Background(), sel, Filter{}, PolicyLatencyFirst, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != "x-cascade" {
		t.Fatalf("latency-first = %+v, want warm x-cascade", got)
	}

	quota.headroom["strong"], quota.headroom["small"] = .5, .5
	expireHeadroom(router.quota)
	pool["gamma"], pool["x-cascade"] = [2]int{}, [2]int{}
	got, err = router.Route(context.Background(), sel, Filter{}, PolicyQuotaFirst, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != "x-cascade" || got.Ranked[0].Score.Weight != 5 {
		t.Fatalf("provider weight = %+v", got)
	}
}

func TestRouterPreviewsCandidatesLazily(t *testing.T) {
	quota := &fakeQuota{
		headroom:    map[string]float64{"strong": .8, "small": .7},
		refuseFirst: 1,
	}
	router, cat, _ := newTestRouter(t, DefaultPolicy(), quota)
	got, err := router.Route(context.Background(), selector(t, cat, "L4"), Filter{}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if calls := quota.previews(); len(calls) != 2 {
		t.Fatalf("Preview calls = %v, want two after the leader refused", calls)
	}
	if got.Ranked[0].Preview != previewRefused || got.Ranked[1].Preview != previewAllowed {
		t.Fatalf("preview audit = %+v", got.Ranked)
	}

	quota = &fakeQuota{headroom: map[string]float64{"strong": .8, "small": .7}}
	router, cat, _ = newTestRouter(t, DefaultPolicy(), quota)
	got, err = router.Route(context.Background(), selector(t, cat, "L4"), Filter{}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if calls := quota.previews(); len(calls) != 1 {
		t.Fatalf("Preview calls = %v, want only the allowed leader", calls)
	}
	if got.Ranked[0].Preview != previewAllowed || got.Ranked[1].Preview != previewNotRun {
		t.Fatalf("preview audit = %+v", got.Ranked)
	}
}

func TestRouterRankNeverPreviews(t *testing.T) {
	quota := &fakeQuota{headroom: map[string]float64{"strong": .8, "small": .7}}
	router, _, _ := newTestRouter(t, DefaultPolicy(), quota)
	if got := router.Rank(context.Background(), 4, Filter{}, ""); len(got) != 2 {
		t.Fatalf("Rank = %v, want two L4 candidates", got)
	}
	if calls := quota.previews(); len(calls) != 0 {
		t.Fatalf("Rank called Preview for %v", calls)
	}
}

func TestRouterRouteTenCandidatesPaysForOnePreview(t *testing.T) {
	cat := testFleet(t)
	for i := range 8 {
		if err := cat.Registry().SaveAgent(fleet.Agent{
			Name: fmt.Sprintf("candidate-%02d", i), Tool: "beta-tool", Model: "strong",
		}); err != nil {
			t.Fatal(err)
		}
	}
	cat = NewFleetCatalog(cat.Registry())
	quota := &fakeQuota{
		headroom:     map[string]float64{"strong": .8, "small": .7},
		previewDelay: 50 * time.Millisecond,
	}
	router := NewRouter(cat, DefaultPolicy(), WithQuotaSource(quota), WithRecorder(&memoryRecorder{}))
	start := time.Now()
	got, err := router.Route(context.Background(), selector(t, cat, "L4"), Filter{}, "", "")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Ranked) != 10 {
		t.Fatalf("ranked candidates = %d, want 10", len(got.Ranked))
	}
	if calls := quota.previews(); len(calls) != 1 {
		t.Fatalf("Preview calls = %v, want one", calls)
	}
	if elapsed >= 150*time.Millisecond {
		t.Fatalf("Route took %v, want less than 150ms", elapsed)
	}
}

func TestRouterEscalatesUpAndNeverDown(t *testing.T) {
	quota := &fakeQuota{headroom: map[string]float64{"small": .1, "strong": .1, "frontier": .8}}
	policy := DefaultPolicy()
	policy.Escalate = EscalateUp
	router, cat, _ := newTestRouter(t, policy, quota)
	got, err := router.Route(context.Background(), selector(t, cat, "L4"), Filter{}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != "omega" || !strings.Contains(got.Reason, "L4->L5") {
		t.Fatalf("up escalation = %+v", got)
	}

	_, err = router.Route(context.Background(), selector(t, cat, "L5"), Filter{Provider: "anthropic"}, "", "")
	var routeErr *RouteError
	if !errors.As(err, &routeErr) || routeErr.Status != 503 {
		t.Fatalf("L5 must not route down to L2: %T %v", err, err)
	}
}

func TestRouterMarkIneligibleIsPermanentForRouter(t *testing.T) {
	quota := &fakeQuota{headroom: map[string]float64{"strong": .8, "small": .7}}
	router, cat, _ := newTestRouter(t, DefaultPolicy(), quota)
	router.MarkIneligible("gamma", "chatgpt", "model is not supported when using Codex with a ChatGPT account")
	got, err := router.Route(context.Background(), selector(t, cat, "L4"), Filter{}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != "x-cascade" {
		t.Fatalf("marked agent retried: %+v", got)
	}
}

// servableFixture builds a tiny fleet with one agent in each of the states
// Servable distinguishes: installed and reachable, bound to a tool whose
// binary cannot be found, and marked ineligible. A fourth agent is put into
// breaker cooldown by the caller, since that state is per-router, not
// per-fleet.
func servableFixture(t *testing.T) (*FleetCatalog, map[string]Agent) {
	t.Helper()
	root := t.TempDir()
	fake := filepath.Join(root, "fake-cli")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cat := fleet.New(fleet.WithRoot(filepath.Join(root, "fleet")), fleet.WithBaselineFS(fstest.MapFS{}), fleet.WithoutCloudOverlay())
	tools := []fleet.Tool{
		{Name: "good-tool", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: fake, Launch: fleet.ToolLaunch{Exec: fake + " --model {model} {prompt}"}}},
		{Name: "missing-tool", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{
			Binary: filepath.Join(root, "does-not-exist"),
			Launch: fleet.ToolLaunch{Exec: "does-not-exist {model} {prompt}"},
		}},
	}
	for _, tool := range tools {
		if err := cat.SaveTool(tool); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.SaveModel(fleet.Model{Name: "mid", Band: 3, Kind: fleet.ModelKindAPI, Provider: "test"}); err != nil {
		t.Fatal(err)
	}
	agents := []fleet.Agent{
		{Name: "ok-agent", Tool: "good-tool", Model: "mid"},
		{Name: "missing-agent", Tool: "missing-tool", Model: "mid"},
		{Name: "cooldown-agent", Tool: "good-tool", Model: "mid"},
	}
	for _, agent := range agents {
		if err := cat.SaveAgent(agent); err != nil {
			t.Fatal(err)
		}
	}
	catalog := NewFleetCatalog(cat)
	byName := map[string]Agent{}
	for _, a := range catalog.inventory() {
		byName[a.Name] = a
	}
	for _, name := range []string{"ok-agent", "missing-agent", "cooldown-agent"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("fixture inventory is missing %q", name)
		}
	}
	return catalog, byName
}

// TestRouterServableMatchesThe503Rule pins S406/#1772: Servable must agree
// with the router's own candidate filter in score, since listModels reuses
// it to decide whether a model is worth advertising.
func TestRouterServableMatchesThe503Rule(t *testing.T) {
	catalog, agents := servableFixture(t)
	breaker := sched.NewBreaker()
	router := NewRouter(catalog, DefaultPolicy(), WithBreaker(breaker))
	breaker.Trip("cooldown-agent", time.Minute)

	if ok, reason := router.Servable(agents["ok-agent"]); !ok {
		t.Fatalf("ok-agent Servable = (%v, %q), want servable", ok, reason)
	}
	if ok, reason := router.Servable(agents["missing-agent"]); ok || reason != "tool not installed" {
		t.Fatalf("missing-agent Servable = (%v, %q), want (false, \"tool not installed\")", ok, reason)
	}
	if ok, reason := router.Servable(agents["cooldown-agent"]); ok || reason != "breaker cooldown" {
		t.Fatalf("cooldown-agent Servable = (%v, %q), want (false, \"breaker cooldown\")", ok, reason)
	}

	router.MarkIneligible("ok-agent", "test-account", "incompatible binding")
	if ok, reason := router.Servable(agents["ok-agent"]); ok || !strings.HasPrefix(reason, "ineligible:") {
		t.Fatalf("marked ok-agent Servable = (%v, %q), want an ineligible reason", ok, reason)
	}
}

// TestRouter429NamesEachRefusedCandidate pins #1807: the 429 reason must name
// every refused candidate and its exact cause — the Preview reason string, or
// headroom against the reserve floor — so the door is diagnosable from the
// body alone.
func TestRouter429NamesEachRefusedCandidate(t *testing.T) {
	quota := &fakeQuota{
		headroom: map[string]float64{"strong": .8, "small": .05},
		refused:  map[string]string{"gamma": "daily token budget exhausted"},
	}
	router, cat, _ := newTestRouter(t, DefaultPolicy(), quota)
	_, err := router.Route(context.Background(), selector(t, cat, "L4"), Filter{}, "", "")
	var routeErr *RouteError
	if !errors.As(err, &routeErr) || routeErr.Status != 429 {
		t.Fatalf("error = %T %v, want RouteError 429", err, err)
	}
	for _, want := range []string{"gamma", "daily token budget exhausted", "x-cascade", "below reserve floor", "0.05", "0.15"} {
		if !strings.Contains(routeErr.Reason, want) {
			t.Fatalf("429 reason = %q, want it to contain %q", routeErr.Reason, want)
		}
	}
}

// TestRouterRefusalEvictsStaleHeadroom pins the self-healing half of #1807: a
// cached low headroom must not stick past the refusal it caused. After a 429
// the refused entries are dropped, so the next request recomputes headroom
// instead of reusing the stale value for the rest of the TTL.
func TestRouterRefusalEvictsStaleHeadroom(t *testing.T) {
	quota := &fakeQuota{headroom: map[string]float64{"strong": .05, "small": .05}}
	router, cat, _ := newTestRouter(t, DefaultPolicy(), quota)
	if _, err := router.Route(context.Background(), selector(t, cat, "L4"), Filter{}, "", ""); err == nil {
		t.Fatal("stale low headroom routed, want 429")
	}
	firstCalls := quota.headrooms("strong") + quota.headrooms("small")

	quota.mu.Lock()
	quota.headroom["strong"], quota.headroom["small"] = .8, .8
	quota.mu.Unlock()
	got, err := router.Route(context.Background(), selector(t, cat, "L4"), Filter{}, "", "")
	if err != nil {
		t.Fatalf("second route after headroom recovery = %v, want success without waiting out the TTL", err)
	}
	if got.Agent == "" {
		t.Fatal("second route picked no agent")
	}
	if calls := quota.headrooms("strong") + quota.headrooms("small"); calls <= firstCalls {
		t.Fatalf("headroom calls = %d after %d, want the refusal to have evicted the stale entries", calls, firstCalls)
	}
}
