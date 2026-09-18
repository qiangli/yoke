// Package llmbudget provides the local-first meter and gate for LLM calls.
//
// It is intentionally brand-neutral. The fleet catalog supplies model kind,
// billing mode, provider and cost; this package turns those local facts into a
// decision before the call and usage counters after the response.
package llmbudget

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/telemetry"
)

type Action string

const (
	Allow     Action = "allow"
	Downgrade Action = "downgrade"
	Queue     Action = "queue"
	Block     Action = "block"
	RouteAlt  Action = "route_alt"
)

type Lane string

const (
	LaneAPIKey       Lane = "api-key"
	LaneSubscription Lane = "subscription"
	LaneLocal        Lane = "local"
)

type Decision struct {
	Action Action
	Model  string
	Delay  time.Duration
	Reason string
	Lane   Lane
}

func (d Decision) Allowed() bool {
	return d.Action == "" || d.Action == Allow || d.Action == Downgrade || d.Action == RouteAlt
}

type Model struct {
	Name        string
	Kind        string
	Billing     string
	Provider    string
	CostMicro   int64
	Plan        string
	DowngradeTo string
	RouteAltTo  string
	Limits      Limits
}

type Limits struct {
	BudgetUSD        float64
	ProviderUSD      float64
	DailyTokens      int64
	WeeklyTokens     int64
	DailyRequests    int64
	WeeklyRequests   int64
	NearLimitRatio   float64
	RateTokens       int64
	RatePer          time.Duration
	ProviderQuotaUSD float64
}

type Config struct {
	PolicyPath        string
	Policy            *Policy
	Adapters          []Adapter
	ResolveCredential CredentialResolver

	Models       map[string]Model
	StatePath    string
	AllowPremium bool
	Now          func() time.Time
	Logger       *slog.Logger
	BoundHit     func(context.Context, string, int64, int64, string)
}

type Gate struct {
	publishFault  func(string) error // package tests inject process/publish failures
	mu            sync.Mutex
	cfg           Config
	state         State
	loaded        bool
	seenDisk      bool
	inTransaction bool
	stateErr      error
}

type State struct {
	HardPolicy   bool                    `json:"hard_policy,omitempty"`
	Integrity    string                  `json:"integrity,omitempty"`
	Unattributed map[string]Counters     `json:"unattributed,omitempty"`
	Version      int                     `json:"version,omitempty"`
	Pools        map[string]PoolCounters `json:"pools,omitempty"`
	Reservations map[string]Reservation  `json:"reservations,omitempty"`
	Completed    map[string]Completion   `json:"completed,omitempty"`

	Models    map[string]Counters `json:"models,omitempty"`
	Plans     map[string]Counters `json:"plans,omitempty"`
	Providers map[string]Counters `json:"providers,omitempty"`
	Buckets   map[string]Bucket   `json:"buckets,omitempty"`
}

type Counters struct {
	DayStart     time.Time `json:"day_start,omitempty"`
	WeekStart    time.Time `json:"week_start,omitempty"`
	DayTokens    int64     `json:"day_tokens,omitempty"`
	WeekTokens   int64     `json:"week_tokens,omitempty"`
	DayRequests  int64     `json:"day_requests,omitempty"`
	WeekRequests int64     `json:"week_requests,omitempty"`
	DayCostUSD   float64   `json:"day_cost_usd,omitempty"`
	WeekCostUSD  float64   `json:"week_cost_usd,omitempty"`
	CostUSD      float64   `json:"cost_usd,omitempty"`
}

type Bucket struct {
	WindowStart time.Time `json:"window_start,omitempty"`
	Used        int64     `json:"used,omitempty"`
}

var defaultGate = DefaultFromEnv()

// SetDefault swaps the process-wide gate that the package-level Check/Record
// helpers consult, and returns a function that puts the previous one back.
//
// It exists so a call site can be PROVEN to refuse when the budget is gone. The
// default gate is built from the environment once, at init, so a test cannot
// reach it by setting variables — and a budget gate whose refusal path is never
// exercised is exactly the kind of capability that quietly does not work.
// Inject a Gate with a fixed clock and pre-loaded counters instead of sleeping
// or spending real money to reach a limit.
// DefaultGate returns the current authority for a caller to retain throughout a work lifetime.
func DefaultGate() *Gate { return defaultGate }

func SetDefault(g *Gate) (restore func()) {
	prev := defaultGate
	if g != nil {
		defaultGate = g
	}
	return func() { defaultGate = prev }
}

// Load seeds the gate's counters directly, so a test can start from an
// already-exhausted budget without issuing the calls that would exhaust it.
func (g *Gate) Load(s State) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureLoaded()
	if s.Models != nil {
		g.state.Models = s.Models
	}
	if s.Plans != nil {
		g.state.Plans = s.Plans
	}
	if s.Providers != nil {
		g.state.Providers = s.Providers
	}
	if s.Buckets != nil {
		g.state.Buckets = s.Buckets
	}
}

func New(cfg Config) *Gate {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.BoundHit == nil {
		cfg.BoundHit = telemetry.BoundHit
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Gate{cfg: cfg}
}

// Check applies the default local gate before an LLM call. It is deliberately
// context-free so every client can use it at its request interceptor; clients
// that have an OTel request context should use CheckContext.
func Check(model string, estTokens int64) Decision {
	return defaultGate.Check(model, estTokens)
}

// CheckContext is Check with the calling span attached to any BoundHit event.
func CheckContext(ctx context.Context, model string, estTokens int64) Decision {
	return defaultGate.CheckContext(ctx, model, estTokens)
}

func CheckWithOverride(ctx context.Context, model string, estTokens int64, allowPremium bool) Decision {
	return defaultGate.CheckWithOverride(ctx, model, estTokens, allowPremium)
}

// Record stores actual usage after an LLM response. It is safe to call even
// when pricing is unknown: subscription plan counters remain the evidence for
// a later exhaustion route.
func Record(model string, promptTokens, completionTokens int64, costUSD float64) {
	defaultGate.Record(model, promptTokens, completionTokens, costUSD)
}

// RecordContext is Record's span-aware counterpart, retained for call-site
// symmetry with CheckContext. Recording currently has no bound event itself.
func RecordContext(_ context.Context, model string, promptTokens, completionTokens int64, costUSD float64) {
	defaultGate.Record(model, promptTokens, completionTokens, costUSD)
}

func EstimatedCostUSD(model string, tokens int64) (float64, bool) {
	defaultGate.mu.Lock()
	defer defaultGate.mu.Unlock()
	defaultGate.ensureLoaded()
	m, ok := defaultGate.model(model)
	if !ok {
		return 0, false
	}
	return estimateCost(m, tokens)
}

// LaneForModel reports the catalog-derived billing lane for a model. Callers
// use it when following a RouteAlt decision: a subscription may only fall back
// to API-key billing under an explicit premium override.
func LaneForModel(model string) (Lane, bool) {
	defaultGate.mu.Lock()
	defer defaultGate.mu.Unlock()
	defaultGate.ensureLoaded()
	m, ok := defaultGate.model(model)
	if !ok {
		return "", false
	}
	return laneFor(m), true
}

func (g *Gate) Check(model string, estTokens int64) Decision {
	return g.CheckContext(context.Background(), model, estTokens)
}

func (g *Gate) CheckContext(ctx context.Context, model string, estTokens int64) Decision {
	return g.CheckWithOverride(ctx, model, estTokens, false)
}

func (g *Gate) CheckWithOverride(ctx context.Context, model string, estTokens int64, allowPremium bool) Decision {
	if estTokens < 0 {
		estTokens = 0
	}
	var d Decision
	err := g.transaction(ctx, func() error { d = g.checkLegacy(ctx, model, estTokens, allowPremium); return nil })
	if err != nil {
		return Decision{Action: Block, Model: model, Reason: "budget state unavailable"}
	}
	return d
}
func (g *Gate) checkLegacy(ctx context.Context, model string, estTokens int64, allowPremium bool) Decision {
	m, ok := g.model(model)
	if !ok {
		g.warn("llmbudget: missing model metadata; allowing", "model", model)
		return Decision{Action: Allow, Model: model, Reason: "missing model metadata; fail-open"}
	}
	lane := laneFor(m)
	if allowPremium || g.cfg.AllowPremium {
		return Decision{Action: Allow, Model: model, Lane: lane, Reason: "premium override"}
	}
	if m.Limits.RateTokens > 0 && m.Limits.RatePer > 0 {
		if delay := g.reserveRate(m.Provider, m.Limits, estTokens, g.now()); delay > 0 {
			g.bound(ctx, "rate_limit", m.Limits.RateTokens, estTokens, fmt.Sprintf("provider=%s model=%s", m.Provider, model))
			return Decision{Action: Queue, Model: model, Delay: delay, Lane: lane, Reason: "provider rate limit"}
		}
	}
	switch lane {
	case LaneAPIKey:
		return g.checkAPIKey(ctx, m, estTokens)
	case LaneSubscription:
		return g.checkSubscription(ctx, m, estTokens)
	default:
		return Decision{Action: Allow, Model: model, Lane: lane}
	}
}

func (g *Gate) Record(model string, promptTokens, completionTokens int64, costUSD float64) {
	if promptTokens < 0 {
		promptTokens = 0
	}
	if completionTokens < 0 {
		completionTokens = 0
	}
	if math.IsNaN(costUSD) || math.IsInf(costUSD, 0) || costUSD < 0 {
		return
	}
	if err := g.transaction(context.Background(), func() error {
		g.recordLocal(model, promptTokens, completionTokens, costUSD)
		g.state.Unattributed[model] = addCounters(g.state.Unattributed[model], g.now(), promptTokens+completionTokens, 1, costUSD)
		return nil
	}); err != nil {
		g.warn("llmbudget: usage was not recorded", "error", err)
	}
}
func (g *Gate) recordLocal(model string, promptTokens, completionTokens int64, costUSD float64) {
	m, _ := g.model(model)
	now := g.now()
	totalTokens := safeAdd(promptTokens, completionTokens)
	g.state.Models[model] = addCounters(g.state.Models[model], now, totalTokens, 1, costUSD)
	if m.Provider != "" {
		g.state.Providers[m.Provider] = addCounters(g.state.Providers[m.Provider], now, totalTokens, 1, costUSD)
	}
	if p := planName(m); p != "" {
		g.state.Plans[p] = addCounters(g.state.Plans[p], now, totalTokens, 1, 0)
	}
}

func (g *Gate) checkAPIKey(ctx context.Context, m Model, estTokens int64) Decision {
	cost, known := estimateCost(m, estTokens)
	if !known {
		g.warn("llmbudget: missing price metadata; allowing", "model", m.Name)
		return Decision{Action: Allow, Model: m.Name, Lane: LaneAPIKey, Reason: "missing price; fail-open"}
	}
	total := g.totalDayCostUSD() + cost
	if m.Limits.BudgetUSD > 0 && total > m.Limits.BudgetUSD {
		g.bound(ctx, "api_key_budget", microUSD(m.Limits.BudgetUSD), microUSD(total), "model="+m.Name)
		return overCostDecision(m, "budget cap")
	}
	provCounters := currentCounters(g.state.Providers[m.Provider], g.now())
	prov := provCounters.DayCostUSD + cost
	quota := firstPositive(m.Limits.ProviderQuotaUSD, m.Limits.ProviderUSD)
	if quota > 0 && prov > quota {
		g.bound(ctx, "api_key_provider_quota", microUSD(quota), microUSD(prov), "provider="+m.Provider)
		return Decision{Action: Block, Model: m.Name, Lane: LaneAPIKey, Reason: "provider quota"}
	}
	return Decision{Action: Allow, Model: m.Name, Lane: LaneAPIKey}
}

func (g *Gate) checkSubscription(ctx context.Context, m Model, estTokens int64) Decision {
	c := currentCounters(g.state.Plans[planName(m)], g.now())
	lim := m.Limits
	if lim.DailyTokens <= 0 && lim.WeeklyTokens <= 0 && lim.DailyRequests <= 0 && lim.WeeklyRequests <= 0 {
		g.warn("llmbudget: missing subscription plan limit metadata; allowing", "model", m.Name, "plan", planName(m))
		return Decision{Action: Allow, Model: m.Name, Lane: LaneSubscription, Reason: "missing plan limit; fail-open"}
	}
	ratio := lim.NearLimitRatio
	if ratio <= 0 || ratio >= 1 {
		ratio = 0.9
	}
	check := []struct {
		kind   string
		limit  int64
		actual int64
	}{
		{"subscription_daily_tokens", lim.DailyTokens, safeAdd(c.DayTokens, estTokens)},
		{"subscription_weekly_tokens", lim.WeeklyTokens, safeAdd(c.WeekTokens, estTokens)},
		{"subscription_daily_requests", lim.DailyRequests, safeAdd(c.DayRequests, 1)},
		{"subscription_weekly_requests", lim.WeeklyRequests, safeAdd(c.WeekRequests, 1)},
	}
	for _, b := range check {
		if b.limit <= 0 {
			continue
		}
		if b.actual >= int64(math.Ceil(float64(b.limit)*ratio)) {
			g.bound(ctx, b.kind, b.limit, b.actual, "plan="+planName(m)+" model="+m.Name)
			if m.RouteAltTo != "" {
				return Decision{Action: RouteAlt, Model: m.RouteAltTo, Lane: LaneSubscription, Reason: "subscription exhaustion near/at ceiling"}
			}
			return Decision{Action: Block, Model: m.Name, Lane: LaneSubscription, Reason: "subscription exhaustion near/at ceiling"}
		}
	}
	return Decision{Action: Allow, Model: m.Name, Lane: LaneSubscription}
}

func overCostDecision(m Model, reason string) Decision {
	if m.DowngradeTo != "" {
		return Decision{Action: Downgrade, Model: m.DowngradeTo, Lane: LaneAPIKey, Reason: reason}
	}
	return Decision{Action: Block, Model: m.Name, Lane: LaneAPIKey, Reason: reason}
}

func (g *Gate) reserveRate(provider string, lim Limits, tokens int64, now time.Time) time.Duration {
	if provider == "" {
		provider = "unknown"
	}
	b := g.state.Buckets[provider]
	if b.WindowStart.IsZero() || !now.Before(b.WindowStart.Add(lim.RatePer)) {
		b = Bucket{WindowStart: now}
	}
	if safeAdd(b.Used, tokens) > lim.RateTokens {
		g.state.Buckets[provider] = b
		g.save()
		return b.WindowStart.Add(lim.RatePer).Sub(now)
	}
	b.Used = safeAdd(b.Used, tokens)
	g.state.Buckets[provider] = b
	g.save()
	return 0
}

func (g *Gate) ensureLoaded() {
	if !g.loaded {
		g.stateErr = g.reload()
	}
}
func (g *Gate) save() {
	if !g.inTransaction {
		g.stateErr = g.writeState()
	}
}

func (g *Gate) model(name string) (Model, bool) {
	// Account-only source rows have no model. A missing model is not a
	// request to parse every embedded/local fleet model.
	if name == "" {
		return Model{}, false
	}
	if m, ok := g.cfg.Models[name]; ok {
		m.Name = nonEmpty(m.Name, name)
		return m, true
	}
	if cat := fleet.New(); cat != nil {
		if fm, ok := cat.Model(name); ok {
			return FromFleetModel(fm), true
		}
	}
	return Model{}, false
}

func FromFleetModel(m fleet.Model) Model {
	return Model{
		Name:      m.Name,
		Kind:      m.Kind,
		Billing:   m.BillingMode(),
		Provider:  m.Provider,
		CostMicro: m.CostMicro,
		Plan:      env("BASHY_LLM_PLAN_"+envKey(m.Name), ""),
		Limits: Limits{
			BudgetUSD:      envFloat("BASHY_LLM_BUDGET_DAILY_USD"),
			ProviderUSD:    envFloat("BASHY_LLM_PROVIDER_" + envKey(m.Provider) + "_DAILY_USD"),
			DailyTokens:    envInt("BASHY_LLM_MODEL_" + envKey(m.Name) + "_DAILY_TOKENS"),
			WeeklyTokens:   envInt("BASHY_LLM_MODEL_" + envKey(m.Name) + "_WEEKLY_TOKENS"),
			DailyRequests:  envInt("BASHY_LLM_MODEL_" + envKey(m.Name) + "_DAILY_REQUESTS"),
			WeeklyRequests: envInt("BASHY_LLM_MODEL_" + envKey(m.Name) + "_WEEKLY_REQUESTS"),
			NearLimitRatio: envFloat("BASHY_LLM_NEAR_LIMIT_RATIO"),
			RateTokens:     envInt("BASHY_LLM_PROVIDER_" + envKey(m.Provider) + "_RATE_TOKENS"),
			RatePer:        envDuration("BASHY_LLM_PROVIDER_" + envKey(m.Provider) + "_RATE_PER"),
		},
		DowngradeTo: env("BASHY_LLM_DOWNGRADE_"+envKey(m.Name), ""),
		RouteAltTo:  env("BASHY_LLM_ROUTE_ALT_"+envKey(m.Name), ""),
	}
}

func DefaultFromEnv() *Gate {
	return New(Config{
		StatePath:    env("BASHY_LLM_BUDGET_STATE", defaultStatePath()),
		AllowPremium: premiumAllowed(),
	})
}

func defaultStatePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".bashy", "llm-budget.json")
}

func premiumAllowed() bool {
	for _, k := range []string{"BASHY_ALLOW_PREMIUM", "LLM_ALLOW_PREMIUM"} {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}

func laneFor(m Model) Lane {
	switch m.Kind {
	case fleet.ModelKindSubscription:
		return LaneSubscription
	case fleet.ModelKindAPI:
		if m.Billing == fleet.BillingFlat || m.Billing == fleet.BillingFlatThenMetered {
			return LaneSubscription
		}
		return LaneAPIKey
	case fleet.ModelKindLocal:
		return LaneLocal
	default:
		if m.Billing == fleet.BillingMetered {
			return LaneAPIKey
		}
		if m.Billing == fleet.BillingFlat || m.Billing == fleet.BillingFlatThenMetered {
			return LaneSubscription
		}
		return ""
	}
}

func estimateCost(m Model, tokens int64) (float64, bool) {
	if m.CostMicro <= 0 {
		return 0, false
	}
	return float64(tokens) * float64(m.CostMicro) / 1_000_000, true
}

func addCounters(c Counters, now time.Time, tokens, requests int64, cost float64) Counters {
	c = currentCounters(c, now)
	c.DayTokens = safeAdd(c.DayTokens, tokens)
	c.WeekTokens = safeAdd(c.WeekTokens, tokens)
	c.DayRequests = safeAdd(c.DayRequests, requests)
	c.WeekRequests = safeAdd(c.WeekRequests, requests)
	c.DayCostUSD += cost
	c.WeekCostUSD += cost
	c.CostUSD += cost
	return c
}

func currentCounters(c Counters, now time.Time) Counters {
	day := dayStart(now)
	if !c.DayStart.Equal(day) {
		c.DayStart, c.DayTokens, c.DayRequests, c.DayCostUSD = day, 0, 0, 0
	}
	week := weekStart(now)
	if !c.WeekStart.Equal(week) {
		c.WeekStart, c.WeekTokens, c.WeekRequests, c.WeekCostUSD = week, 0, 0, 0
	}
	return c
}

func dayStart(t time.Time) time.Time {
	y, m, d := t.Local().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Local().Location())
}

func weekStart(t time.Time) time.Time {
	d := dayStart(t)
	wd := int(d.Weekday())
	if wd == 0 {
		wd = 7
	}
	return d.AddDate(0, 0, -(wd - 1))
}

func (g *Gate) totalDayCostUSD() float64 {
	var total float64
	for _, c := range g.state.Models {
		total += currentCounters(c, g.now()).DayCostUSD
	}
	return total
}

func (g *Gate) bound(ctx context.Context, kind string, limit, actual int64, detail string) {
	if g.cfg.BoundHit != nil {
		g.cfg.BoundHit(ctx, kind, limit, actual, detail)
	}
}

func (g *Gate) now() time.Time { return g.cfg.Now() }

func (g *Gate) warn(msg string, attrs ...any) {
	if g.cfg.Logger != nil {
		g.cfg.Logger.Warn(msg, attrs...)
	}
}

func planName(m Model) string {
	if m.Plan != "" {
		return m.Plan
	}
	if m.Provider != "" {
		return m.Provider + ":default"
	}
	return m.Name
}

func firstPositive(v ...float64) float64 {
	for _, x := range v {
		if x > 0 {
			return x
		}
	}
	return 0
}

func microUSD(v float64) int64 {
	if v >= float64(math.MaxInt64)/1e6 {
		return math.MaxInt64
	}
	return int64(math.Ceil(v * 1_000_000))
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envFloat(k string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(os.Getenv(k)), 64)
	return v
}

func envInt(k string) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(os.Getenv(k)), 10, 64)
	return v
}

func envDuration(k string) time.Duration {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err == nil {
		return d
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	if n > 0 {
		return time.Duration(n) * time.Second
	}
	return 0
}

func envKey(s string) string {
	s = strings.ToUpper(s)
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func nonEmpty(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
