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
	Headroom(model string) (remaining float64, known bool)
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

// Candidate is one eligible agent and its auditable routing score.
type Candidate struct {
	Agent    string         `json:"agent"`
	Model    string         `json:"model"`
	Tool     string         `json:"tool"`
	Provider string         `json:"provider"`
	Band     int            `json:"band"`
	Score    CandidateScore `json:"score"`
}

// Decision is the selected route and the eligible candidates in rank order.
type Decision struct {
	Agent  string      `json:"agent"`
	Band   string      `json:"band"`
	Reason string      `json:"reason"`
	Ranked []Candidate `json:"ranked"`
}

// Header formats the gateway's routing audit response header.
func (d Decision) Header() string {
	return fmt.Sprintf("X-Bashy-Routed: agent=%s, band=%s, reason=%s", d.Agent, d.Band, d.Reason)
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(r.Path) == "" {
		return errors.New("cligw: empty usage recorder path")
	}
	b, err := json.Marshal(decision)
	if err != nil {
		return fmt.Errorf("cligw: encode usage decision: %w", err)
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
	agents := r.catalog.Candidates(ctx, sel, filter)
	if len(agents) == 0 {
		return Decision{}, &RouteError{Status: 503, Reason: "no fleet candidate matches the selector and filter"}
	}
	r.mu.Lock()
	affinity := r.affinity[sessionID]
	marks := make(map[string]ineligibleMark, len(r.ineligible))
	for name, mark := range r.ineligible {
		marks[name] = mark
	}
	r.mu.Unlock()

	byBand := make(map[int][]Candidate)
	nonQuotaRejected, quotaRejected := 0, 0
	for _, agent := range agents {
		if err := ctx.Err(); err != nil {
			return Decision{}, err
		}
		if _, marked := marks[agent.Name]; marked {
			nonQuotaRejected++
			continue
		}
		if r.breaker.InCooldown(agent.Name) || !r.toolInstalled(agent) {
			nonQuotaRejected++
			continue
		}
		allowed, _ := r.quota.Preview(ctx, agent)
		if !allowed {
			quotaRejected++
			continue
		}
		headroom, known := r.quota.Headroom(agent.Model)
		if known {
			if headroom < 0 {
				headroom = 0
			}
			if headroom > 1 {
				headroom = 1
			}
		}
		idle, queued := r.pool.Idle(agent.Name), r.pool.Queued(agent.Name)
		preferred := false
		if value, ok := strings.CutPrefix(policyName, "prefer:"); ok {
			preferred = agent.Provider == value || agent.Tool == value
		}
		candidate := Candidate{
			Agent: agent.Name, Model: agent.Model, Tool: agent.Tool, Provider: agent.Provider, Band: agent.Band,
			Score: CandidateScore{
				Headroom: headroom, HeadroomKnown: known,
				BelowReserve: known && headroom < r.policy.ReserveFloor,
				WarmIdle:     idle > 0, Idle: idle, Queued: queued,
				Weight: r.policy.weight(agent.Provider), Affinity: affinity == agent.Name,
				Preferred: preferred,
			},
		}
		byBand[agent.Band] = append(byBand[agent.Band], candidate)
	}

	bands := make([]int, 0, len(byBand))
	for band := range byBand {
		bands = append(bands, band)
	}
	sort.Ints(bands)
	for _, band := range bands {
		candidates := byBand[band]
		aboveFloor := false
		for _, candidate := range candidates {
			if !candidate.Score.BelowReserve {
				aboveFloor = true
				break
			}
		}
		if !aboveFloor {
			quotaRejected += len(candidates)
			continue
		}
		r.rank(candidates, policyName, sel, band)
		winner := candidates[0]
		return Decision{
			Agent: winner.Agent, Band: fleet.BandLabel(winner.Band),
			Reason: rankingReason(policyName, winner), Ranked: candidates,
		}, nil
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
		return fmt.Sprintf("latency-first idle=%d queue=%d headroom=%.2f", winner.Score.Idle, winner.Score.Queued, winner.Score.Headroom)
	}
	return fmt.Sprintf("quota-first headroom=%.2f idle=%d queue=%d weight=%.2f", winner.Score.Headroom, winner.Score.Idle, winner.Score.Queued, winner.Score.Weight)
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

func (llmBudgetQuota) Headroom(model string) (float64, bool) {
	status := llmbudget.Status(model)
	if !status.LimitKnown || status.Limit == nil || status.Remaining == nil || *status.Limit <= 0 {
		return 0, false
	}
	return float64(*status.Remaining) / float64(*status.Limit), true
}

func (llmBudgetQuota) Preview(ctx context.Context, agent Agent) (bool, string) {
	admission, err := llmbudget.Preview(ctx, llmbudget.Request{
		Model: agent.Model, Agent: agent.Name, Provider: agent.Provider, Concurrency: 1,
	})
	if err != nil {
		return false, err.Error()
	}
	return admission.Decision.Allowed(), admission.Decision.Reason
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
