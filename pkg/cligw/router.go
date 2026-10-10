package cligw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmbudget"
	"github.com/qiangli/yoke/pkg/llmgw/sched"
)

// PoolState is the read-only scheduling seam shared with the worker pool.
type PoolState interface {
	Idle(agent string) int
	Queued(agent string) int
}

// QuotaSource is the read-only subset of llmbudget used during routing.
// Headroom is normalized to [0,1]. An unknown headroom is not exhaustion.
type QuotaSource interface {
	Headroom(ctx context.Context, agent Agent) (remaining float64, known bool)
	Preview(ctx context.Context, agent Agent) (allowed bool, reason string)
}

// Recorder persists an auditable routing decision.
type Recorder interface {
	Record(context.Context, Decision) error
}

// CandidateScore exposes every value used by the ranking comparators.
type CandidateScore struct {
	Headroom      float64 `json:"headroom"`
	HeadroomKnown bool    `json:"headroom_known"`
	BelowReserve  bool    `json:"below_reserve"`
	WarmIdle      bool    `json:"warm_idle"`
	Idle          int     `json:"idle"`
	Queued        int     `json:"queued"`
	Weight        float64 `json:"weight"`
	Affinity      bool    `json:"affinity"`
	Preferred     bool    `json:"preferred"`
}

// Candidate is one cheaply eligible agent and its auditable routing score and
// preview outcome. Candidates after the winner remain marked "not previewed".
type Candidate struct {
	Agent    string         `json:"agent"`
	Model    string         `json:"model"`
	Tool     string         `json:"tool"`
	Provider string         `json:"provider"`
	Band     int            `json:"band"`
	Score    CandidateScore `json:"score"`
	Preview  string         `json:"preview"`
	Reason   string         `json:"preview_reason,omitempty"`
}

const (
	previewAllowed      = "allowed"
	previewRefused      = "refused"
	previewNotRun       = "not previewed"
	previewBelowReserve = "below reserve"
	headroomTTL         = 10 * time.Second
)

// Decision is the selected route and its cheaply eligible candidates in rank
// order, including refused and not-previewed seats for auditability.
type Decision struct {
	Agent  string      `json:"agent"`
	Band   string      `json:"band"`
	Reason string      `json:"reason"`
	Ranked []Candidate `json:"ranked"`
}

// Header formats the gateway's routing audit response header.
func (d Decision) Header() string { return RoutedHeader + ": " + d.HeaderValue() }

// HeaderValue is the RoutedHeader value alone — what an HTTP adapter sets on
// the response.
func (d Decision) HeaderValue() string {
	return fmt.Sprintf("agent=%s, band=%s, reason=%s", d.Agent, d.Band, d.Reason)
}

// RouteError is safe for an HTTP adapter to expose directly.
type RouteError struct {
	Status int    `json:"status"`
	Reason string `json:"reason"`
}

func (e *RouteError) Error() string {
	return fmt.Sprintf("cligw: route failed (%d): %s", e.Status, e.Reason)
}

// JSONLRecorder appends one JSON object per line. A recorder serializes its
// own writes so several concurrent requests cannot interleave records.
type JSONLRecorder struct {
	Path string
	mu   sync.Mutex
}

func (r *JSONLRecorder) Record(ctx context.Context, decision Decision) error {
	return r.Append(ctx, decision)
}

// Append writes one JSON value as a line. Routing decisions and served-token
// usage records share the recorder — and therefore the mutex — so the two
// streams interleave by line and never by byte.
func (r *JSONLRecorder) Append(ctx context.Context, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(r.Path) == "" {
		return errors.New("cligw: empty usage recorder path")
	}
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("cligw: encode usage record: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o700); err != nil {
		return fmt.Errorf("cligw: create usage directory: %w", err)
	}
	f, err := os.OpenFile(r.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("cligw: open usage log: %w", err)
	}
	_, writeErr := f.Write(append(b, '\n'))
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("cligw: append usage log: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("cligw: close usage log: %w", closeErr)
	}
	return nil
}

type routerOptions struct {
	pool         PoolState
	quota        QuotaSource
	breaker      *sched.Breaker
	recorder     Recorder
	historyLimit int
	lookPath     func(string) (string, error)
}

// RouterOption injects a routing dependency.
type RouterOption func(*routerOptions)

func WithPoolState(pool PoolState) RouterOption { return func(o *routerOptions) { o.pool = pool } }
func WithQuotaSource(quota QuotaSource) RouterOption {
	return func(o *routerOptions) { o.quota = quota }
}
func WithBreaker(b *sched.Breaker) RouterOption { return func(o *routerOptions) { o.breaker = b } }
func WithRecorder(r Recorder) RouterOption      { return func(o *routerOptions) { o.recorder = r } }
func WithHistoryLimit(n int) RouterOption       { return func(o *routerOptions) { o.historyLimit = n } }
func withLookPath(f func(string) (string, error)) RouterOption {
	return func(o *routerOptions) { o.lookPath = f }
}

// Router ranks fleet candidates and retains bounded recent decision history.
type Router struct {
	catalog  *FleetCatalog
	policy   Policy
	pool     PoolState
	quota    QuotaSource
	breaker  *sched.Breaker
	recorder Recorder
	lookPath func(string) (string, error)

	mu           sync.Mutex
	history      []Decision
	historyLimit int
	affinity     map[string]string
	roundRobin   map[string]uint64
	ineligible   map[string]ineligibleMark
}

type ineligibleMark struct {
	Account string
	Reason  string
}

// NewRouter constructs an isolated router. Nil dependencies receive their
// production defaults.
func NewRouter(catalog *FleetCatalog, policy Policy, options ...RouterOption) *Router {
	if catalog == nil {
		catalog = LoadFleetCatalog()
	}
	opt := routerOptions{historyLimit: 256, lookPath: exec.LookPath}
	for _, option := range options {
		option(&opt)
	}
	if opt.pool == nil {
		opt.pool = emptyPool{}
	}
	if opt.quota == nil {
		opt.quota = llmBudgetQuota{}
	}
	opt.quota = cacheHeadroom(opt.quota)
	if opt.breaker == nil {
		opt.breaker = sched.NewBreaker()
	}
	if opt.recorder == nil {
		opt.recorder = &JSONLRecorder{Path: usagePath()}
	}
	if opt.historyLimit < 1 {
		opt.historyLimit = 1
	}
	return &Router{
		catalog: catalog, policy: policy, pool: opt.pool, quota: opt.quota,
		breaker: opt.breaker, recorder: opt.recorder, lookPath: opt.lookPath,
		historyLimit: opt.historyLimit, affinity: map[string]string{},
		roundRobin: map[string]uint64{}, ineligible: map[string]ineligibleMark{},
	}
}

// NewDefaultRouter loads policy.yaml and constructs a production router.
func NewDefaultRouter(options ...RouterOption) (*Router, error) {
	policy, err := LoadPolicy()
	if err != nil {
		return nil, err
	}
	return NewRouter(nil, policy, options...), nil
}

// Route selects one eligible agent and records the decision.
func (r *Router) Route(ctx context.Context, sel Selector, filter Filter, policyName, sessionID string) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	policy := r.policy
	if err := policy.Validate(); err != nil {
		return Decision{}, err
	}
	policyName = strings.TrimSpace(policyName)
	if policyName == "" {
		policyName = policy.Default
	}
	if !validPolicyName(policyName) {
		return Decision{}, fmt.Errorf("cligw: unknown routing policy %q", policyName)
	}
	filter = policy.Filter.Merge(filter)

	firstBand := sel.Band
	decision, err := r.routeOnce(ctx, sel, filter, policyName, sessionID)
	if err != nil && policy.Escalate == EscalateUp && sel.Kind == SelectorBand && !sel.MinBand {
		for band := sel.Band + 1; band <= fleet.MaxBand; band++ {
			next := sel
			next.Raw, next.Band = fleet.BandLabel(band), band
			decision, err = r.routeOnce(ctx, next, filter, policyName, sessionID)
			if err == nil {
				decision.Reason = fmt.Sprintf("escalated L%d->L%d; %s", firstBand, band, decision.Reason)
				break
			}
		}
	}
	if err != nil {
		return Decision{}, err
	}
	if err := r.recorder.Record(ctx, decision); err != nil {
		return Decision{}, err
	}
	r.mu.Lock()
	if sessionID != "" {
		r.affinity[sessionID] = decision.Agent
	}
	r.history = append(r.history, decision)
	if extra := len(r.history) - r.historyLimit; extra > 0 {
		copy(r.history, r.history[extra:])
		r.history = r.history[:r.historyLimit]
	}
	r.mu.Unlock()
	return decision, nil
}

func (r *Router) routeOnce(ctx context.Context, sel Selector, filter Filter, policyName, sessionID string) (Decision, error) {
	return r.decide(ctx, r.catalog.Candidates(ctx, sel, filter), sel, policyName, sessionID)
}

// decide cheaply ranks an already-selected candidate set, then previews quota
// eligibility in rank order and stops at the first allowed seat.
func (r *Router) decide(ctx context.Context, agents []Agent, sel Selector, policyName, sessionID string) (Decision, error) {
	if len(agents) == 0 {
		return Decision{}, &RouteError{Status: 503, Reason: "no fleet candidate matches the selector and filter"}
	}
	scored := r.score(ctx, agents, policyName, sessionID)
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	byBand := make(map[int][]Candidate)
	byName := make(map[string]Agent, len(agents))
	for _, agent := range agents {
		byName[agent.Name] = agent
	}
	nonQuotaRejected, quotaRejected := len(agents)-len(scored), 0
	for _, candidate := range scored {
		byBand[candidate.Band] = append(byBand[candidate.Band], candidate)
	}

	bands := make([]int, 0, len(byBand))
	for band := range byBand {
		bands = append(bands, band)
	}
	sort.Ints(bands)
	for _, band := range bands {
		candidates := byBand[band]
		r.rank(candidates, policyName, sel, band)
		for i := range candidates {
			if candidates[i].Score.BelowReserve {
				candidates[i].Preview = previewBelowReserve
				quotaRejected++
				continue
			}
			agent, ok := byName[candidates[i].Agent]
			if !ok {
				continue
			}
			allowed, reason := r.quota.Preview(ctx, agent)
			if err := ctx.Err(); err != nil {
				return Decision{}, err
			}
			candidates[i].Reason = reason
			if !allowed {
				candidates[i].Preview = previewRefused
				quotaRejected++
				continue
			}
			candidates[i].Preview = previewAllowed
			winner := candidates[i]
			return Decision{
				Agent: winner.Agent, Band: fleet.BandLabel(winner.Band),
				Reason: rankingReason(policyName, winner), Ranked: candidates,
			}, nil
		}
	}
	if quotaRejected > 0 && nonQuotaRejected == 0 {
		return Decision{}, &RouteError{Status: 429, Reason: "all matching candidates were refused by quota or below the reserve floor"}
	}
	if quotaRejected > 0 && len(byBand) > 0 {
		return Decision{}, &RouteError{Status: 429, Reason: "remaining candidates were refused by quota or below the reserve floor"}
	}
	return Decision{}, &RouteError{Status: 503, Reason: "no matching candidate is installed and outside breaker cooldown"}
}

func (r *Router) rank(candidates []Candidate, policyName string, sel Selector, band int) {
	if policyName == PolicyRoundRobin {
		sort.SliceStable(candidates, func(i, j int) bool {
			if candidates[i].Score.BelowReserve != candidates[j].Score.BelowReserve {
				return !candidates[i].Score.BelowReserve
			}
			return candidates[i].Agent < candidates[j].Agent
		})
		available := len(candidates)
		for i, candidate := range candidates {
			if candidate.Score.BelowReserve {
				available = i
				break
			}
		}
		if available == 0 {
			return
		}
		key := sel.Raw + ":" + fmt.Sprint(band)
		r.mu.Lock()
		start := int(r.roundRobin[key] % uint64(available))
		r.roundRobin[key]++
		r.mu.Unlock()
		rotated := append(append([]Candidate(nil), candidates[start:available]...), candidates[:start]...)
		rotated = append(rotated, candidates[available:]...)
		copy(candidates, rotated)
		return
	}
	latencyFirst := policyName == PolicyLatencyFirst
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i].Score, candidates[j].Score
		if a.Preferred != b.Preferred {
			return a.Preferred
		}
		if latencyFirst {
			if a.WarmIdle != b.WarmIdle {
				return a.WarmIdle
			}
			if a.Queued != b.Queued {
				return a.Queued < b.Queued
			}
			if cmp := compareHeadroom(a, b); cmp != 0 {
				return cmp > 0
			}
		} else {
			if cmp := compareHeadroom(a, b); cmp != 0 {
				return cmp > 0
			}
			if a.WarmIdle != b.WarmIdle {
				return a.WarmIdle
			}
			if a.Queued != b.Queued {
				return a.Queued < b.Queued
			}
		}
		if a.Weight != b.Weight {
			return a.Weight > b.Weight
		}
		if a.Affinity != b.Affinity {
			return a.Affinity
		}
		return candidates[i].Agent < candidates[j].Agent
	})
}

func compareHeadroom(a, b CandidateScore) int {
	if a.BelowReserve != b.BelowReserve {
		if a.BelowReserve {
			return -1
		}
		return 1
	}
	if a.HeadroomKnown != b.HeadroomKnown {
		if a.HeadroomKnown {
			return 1
		}
		return -1
	}
	if a.Headroom > b.Headroom {
		return 1
	}
	if a.Headroom < b.Headroom {
		return -1
	}
	return 0
}

func rankingReason(policyName string, winner Candidate) string {
	if policyName == PolicyRoundRobin {
		return PolicyRoundRobin
	}
	if strings.HasPrefix(policyName, "prefer:") && winner.Score.Preferred {
		return policyName
	}
	if policyName == PolicyLatencyFirst {
		return fmt.Sprintf("latency-first idle=%d queue=%d headroom=%s", winner.Score.Idle, winner.Score.Queued, headroomReason(winner.Score))
	}
	return fmt.Sprintf("quota-first headroom=%s idle=%d queue=%d weight=%.2f", headroomReason(winner.Score), winner.Score.Idle, winner.Score.Queued, winner.Score.Weight)
}

func headroomReason(score CandidateScore) string {
	if !score.HeadroomKnown {
		return "unknown"
	}
	return fmt.Sprintf("%.2f", score.Headroom)
}

func (r *Router) toolInstalled(agent Agent) bool {
	_, tool, _, err := r.catalog.Registry().Binding(agent.Name)
	if err != nil {
		return false
	}
	_, err = r.lookPath(tool.Binary())
	return err == nil
}

// MarkIneligible permanently excludes an agent for this router lifetime and
// also trips the shared breaker for its maximum cooldown. This is used for
// account/model incompatibilities, which retries cannot repair.
func (r *Router) MarkIneligible(agent, account, reason string) {
	if strings.TrimSpace(agent) == "" {
		return
	}
	r.breaker.Trip(agent, 24*time.Hour)
	r.mu.Lock()
	r.ineligible[agent] = ineligibleMark{Account: account, Reason: reason}
	r.mu.Unlock()
}

// Rank returns the cheaply eligible agents of one band, best first, WITHOUT
// previewing quota, recording a decision, or touching affinity. It is the
// autoscaler's Ranker: prewarmed spares sit on the agent the router would pick
// next according to cached headroom and pool state.
//
// It deliberately does not escalate. A band with no eligible agent gets no
// spares; prewarming the band above it would spend a better seat on demand
// that has not arrived.
func (r *Router) Rank(ctx context.Context, band int, filter Filter, policyName string, only ...string) []string {
	if band < 1 || band > fleet.MaxBand {
		return nil
	}
	policy := r.policy
	if err := policy.Validate(); err != nil {
		return nil
	}
	if strings.TrimSpace(policyName) == "" {
		policyName = policy.Default
	}
	if !validPolicyName(policyName) {
		return nil
	}
	sel := Selector{Raw: fleet.BandLabel(band), Kind: SelectorBand, Band: band, catalog: r.catalog}
	agents := r.catalog.Candidates(ctx, sel, policy.Filter.Merge(filter))
	if len(only) > 0 {
		wanted := make(map[string]struct{}, len(only))
		for _, name := range only {
			wanted[name] = struct{}{}
		}
		kept := make([]Agent, 0, len(only))
		for _, agent := range agents {
			if _, ok := wanted[agent.Name]; ok {
				kept = append(kept, agent)
			}
		}
		agents = kept
	}
	candidates := r.score(ctx, agents, policyName, "")
	if len(candidates) == 0 {
		return nil
	}
	aboveFloor := false
	for _, candidate := range candidates {
		if !candidate.Score.BelowReserve {
			aboveFloor = true
			break
		}
	}
	if !aboveFloor {
		return nil
	}
	r.rank(candidates, policyName, sel, band)
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.Agent)
	}
	return out
}

// eligibilityReason reports why agent cannot be routed right now, given
// marks — a snapshot of permanently ineligible agents taken under r.mu. It
// returns "" when the agent is eligible. This is the one rule behind both
// score's candidate filter and the public Servable check: a listed model
// must never be a request guaranteed to hit the router's 503.
func (r *Router) eligibilityReason(agent Agent, marks map[string]ineligibleMark) string {
	if mark, marked := marks[agent.Name]; marked {
		return "ineligible: " + mark.Reason
	}
	if r.breaker.InCooldown(agent.Name) {
		return "breaker cooldown"
	}
	if !r.toolInstalled(agent) {
		return "tool not installed"
	}
	return ""
}

// Servable reports whether agent is a reachable routing candidate right
// now — not permanently marked ineligible, not inside a breaker cooldown,
// and its tool binary resolves on PATH. Callers that list models reuse this
// instead of duplicating the rule behind the router's 503 "no matching
// candidate is installed and outside breaker cooldown".
func (r *Router) Servable(agent Agent) (ok bool, reason string) {
	r.mu.Lock()
	mark, marked := r.ineligible[agent.Name]
	r.mu.Unlock()
	marks := map[string]ineligibleMark{}
	if marked {
		marks[agent.Name] = mark
	}
	reason = r.eligibilityReason(agent, marks)
	return reason == "", reason
}

func (r *Router) score(ctx context.Context, agents []Agent, policyName, sessionID string) []Candidate {
	r.mu.Lock()
	affinity := r.affinity[sessionID]
	marks := make(map[string]ineligibleMark, len(r.ineligible))
	for name, mark := range r.ineligible {
		marks[name] = mark
	}
	r.mu.Unlock()

	candidates := make([]Candidate, 0, len(agents))
	for _, agent := range agents {
		if ctx.Err() != nil {
			return nil
		}
		if r.eligibilityReason(agent, marks) != "" {
			continue
		}
		headroom, known := r.quota.Headroom(ctx, agent)
		if known {
			headroom = max(0, min(1, headroom))
		}
		idle, queued := r.pool.Idle(agent.Name), r.pool.Queued(agent.Name)
		preferred := false
		if value, ok := strings.CutPrefix(policyName, "prefer:"); ok {
			preferred = agent.Provider == value || agent.Tool == value
		}
		candidates = append(candidates, Candidate{
			Agent: agent.Name, Model: agent.Model, Tool: agent.Tool, Provider: agent.Provider, Band: agent.Band,
			Preview: previewNotRun,
			Score: CandidateScore{
				Headroom: headroom, HeadroomKnown: known,
				BelowReserve: known && headroom < r.policy.ReserveFloor,
				WarmIdle:     idle > 0, Idle: idle, Queued: queued,
				Weight: r.policy.weight(agent.Provider), Affinity: affinity == agent.Name,
				Preferred: preferred,
			},
		})
	}
	return candidates
}

// History returns a copy of the bounded in-memory decision ring, oldest first.
func (r *Router) History() []Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Decision(nil), r.history...)
}

type emptyPool struct{}

func (emptyPool) Idle(string) int   { return 0 }
func (emptyPool) Queued(string) int { return 0 }

type llmBudgetQuota struct{}

func (llmBudgetQuota) Headroom(ctx context.Context, agent Agent) (float64, bool) {
	remaining, known, err := llmbudget.SubscriptionHeadroom(ctx, llmbudget.Binding{
		Model: agent.Model, Agent: agent.Name, Provider: agent.Provider,
	})
	if err != nil {
		return 0, false
	}
	return remaining, known
}

func (llmBudgetQuota) Preview(ctx context.Context, agent Agent) (bool, string) {
	admission, err := llmbudget.Preview(ctx, llmbudget.Request{
		Model: agent.Model, ModelID: agent.ModelID, Agent: agent.Name, Provider: agent.Provider, Concurrency: 1,
	})
	if err != nil {
		return false, err.Error()
	}
	return admission.Decision.Allowed(), admission.Decision.Reason
}

type headroomEntry struct {
	remaining float64
	known     bool
	expires   time.Time
}

// headroomCache shares short-lived model headroom between routing, autoscaling,
// and /v1/models. Preview is deliberately never cached: it evaluates live
// concurrency and reservation state for the one candidate about to run.
type headroomCache struct {
	source QuotaSource
	ttl    time.Duration

	mu      sync.Mutex
	entries map[string]headroomEntry
}

func cacheHeadroom(source QuotaSource) QuotaSource {
	if _, ok := source.(*headroomCache); ok {
		return source
	}
	return &headroomCache{source: source, ttl: headroomTTL, entries: map[string]headroomEntry{}}
}

func (c *headroomCache) Headroom(ctx context.Context, agent Agent) (float64, bool) {
	now := time.Now()
	key := agent.Provider + "\x00" + agent.Name + "\x00" + agent.Model
	c.mu.Lock()
	if entry, ok := c.entries[key]; ok && now.Before(entry.expires) {
		c.mu.Unlock()
		return entry.remaining, entry.known
	}
	c.mu.Unlock()

	remaining, known := c.source.Headroom(ctx, agent)
	c.mu.Lock()
	c.entries[key] = headroomEntry{remaining: remaining, known: known, expires: now.Add(c.ttl)}
	c.mu.Unlock()
	return remaining, known
}

func (c *headroomCache) Preview(ctx context.Context, agent Agent) (bool, string) {
	return c.source.Preview(ctx, agent)
}

func usagePath() string {
	if home := strings.TrimSpace(os.Getenv("BASHY_HOME")); home != "" {
		return filepath.Join(home, "cligw", "usage.jsonl")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "bashy-cligw", "usage.jsonl")
	}
	return filepath.Join(home, ".bashy", "cligw", "usage.jsonl")
}
