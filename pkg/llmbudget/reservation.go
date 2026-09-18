package llmbudget

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

type OwnerLease struct {
	id   string
	lock *lockfile.Lock
}

func (o *OwnerLease) ID() string {
	if o == nil {
		return ""
	}
	return o.id
}
func (o *OwnerLease) Close() error {
	if o == nil || o.lock == nil {
		return nil
	}
	return o.lock.Release()
}
func (g *Gate) NewOwner(ctx context.Context, name string) (*OwnerLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(b[:])
	if g.cfg.StatePath == "" {
		return &OwnerLease{id: id}, nil
	}
	l, err := lockfile.TryAcquire(g.ownerPath(id), lockfile.Holder{Name: name, Intent: "budget work owner"})
	if err != nil {
		return nil, err
	}
	return &OwnerLease{id: id, lock: l}, nil
}
func NewOwner(ctx context.Context, name string) (*OwnerLease, error) {
	return defaultGate.NewOwner(ctx, name)
}
func (g *Gate) ownerPath(id string) string {
	return filepath.Join(filepath.Dir(g.cfg.StatePath), "llm-budget-owners", id+".lock")
}
func validOwner(id string) bool { b, e := hex.DecodeString(id); return e == nil && len(b) == 16 }
func (g *Gate) ownerLive(id string) bool {
	if !validOwner(id) {
		return false
	}
	if g.cfg.StatePath == "" {
		return true
	}
	// Do not create a never-existing owner merely to check it.
	if _, err := os.Stat(g.ownerPath(id)); err != nil {
		return false
	}
	l, err := lockfile.TryAcquire(g.ownerPath(id), lockfile.Holder{Name: "budget-owner-check"})
	if errors.Is(err, lockfile.ErrHeld) {
		return true
	}
	if err == nil {
		_ = l.Release()
	}
	return false
}
func Preview(ctx context.Context, r Request) (Admission, error) { return defaultGate.Preview(ctx, r) }
func Reserve(ctx context.Context, r Request) (Admission, error) { return defaultGate.Reserve(ctx, r) }
func Renew(ctx context.Context, id, owner string, ttl time.Duration) error {
	return defaultGate.Renew(ctx, id, owner, ttl)
}
func Settle(ctx context.Context, id, owner string, a Actual) error {
	return defaultGate.Settle(ctx, id, owner, a)
}
func Release(ctx context.Context, id, owner string) error { return defaultGate.Release(ctx, id, owner) }
func ReconcileTerminated(ctx context.Context, id string, p TerminationProof) error {
	return defaultGate.ReconcileTerminated(ctx, id, p)
}

func (g *Gate) prepare(p *Policy, r Request, mutation bool) (Request, error) {
	if r.Tokens < 0 || r.Concurrency < 0 || r.HostSlots < 0 || r.SpendMicroUSD != nil && *r.SpendMicroUSD < 0 {
		return r, errors.New("llmbudget: negative reservation demand")
	}
	if r.Tokens > math.MaxInt64/4 || r.SpendMicroUSD != nil && *r.SpendMicroUSD > math.MaxInt64/4 {
		return r, errors.New("llmbudget: excessive reservation demand")
	}
	if mutation && (r.ID == "" || len(r.ID) > 256 || !validOwner(r.Owner)) {
		return r, errors.New("llmbudget: reservation requires id and owner lease")
	}
	if r.TTL == 0 {
		r.TTL = 2 * time.Minute
	}
	if r.TTL < time.Second || r.TTL > 24*time.Hour {
		return r, errors.New("llmbudget: TTL must be between one second and 24 hours")
	}
	// Host-only work has no model/account demand and must not consume LLM units.
	if r.Model == "" && !r.UnknownTokens && r.Tokens == 0 && r.Concurrency == 0 && r.SpendMicroUSD == nil {
		return r, nil
	}
	model := r.Model
	if m, ok := g.model(model); ok {
		model = m.Name
		if r.Lane == "" {
			r.Lane = laneFor(m)
		}
		if r.Provider == "" {
			r.Provider = m.Provider
		}
	}
	r.Model = model
	if b, ok := bindingFor(p, model, r.Agent); ok {
		if r.Provider != "" && r.Provider != b.Provider && r.Provider != "openai-compat" || r.Account != "" && r.Account != b.Account || r.Pool != "" && r.Pool != b.Pool || r.Lane != "" && r.Lane != b.Lane {
			return r, errors.New("llmbudget: request disagrees with configured binding")
		}
		r.Provider, r.Account, r.Pool, r.Lane = b.Provider, b.Account, b.Pool, b.Lane
	} else if r.Account != "" || r.Pool != "" {
		return r, errors.New("llmbudget: account demand requires an explicit policy binding")
	}
	return r, nil
}
func (g *Gate) Preview(ctx context.Context, r Request) (Admission, error) {
	p, err := g.policy()
	if err != nil {
		return refused(r, err)
	}
	r, err = g.prepare(p, r, false)
	if err != nil {
		return refused(r, err)
	}
	if err = ctx.Err(); err != nil {
		return refused(r, err)
	}
	s, err := g.stateSnapshot()
	if err != nil {
		return refused(r, err)
	}
	if p.missing && s.HardPolicy {
		return refused(r, errors.New("llmbudget: prior hard policy missing; restore it before admission"))
	}
	return g.evaluate(p, s, r), nil
}
func refused(r Request, err error) (Admission, error) {
	return Admission{Decision: Decision{Action: Block, Model: r.Model, Lane: r.Lane, Reason: err.Error()}}, err
}
func (g *Gate) Reserve(ctx context.Context, r Request) (Admission, error) {
	p, err := g.policy()
	if err != nil {
		return refused(r, err)
	}
	r, err = g.prepare(p, r, true)
	if err != nil {
		return refused(r, err)
	}
	var out Admission
	err = g.transaction(ctx, func() error {
		current, e := g.policy()
		if e != nil {
			return e
		}
		p = current
		r, e = g.prepare(p, r, true)
		if e != nil {
			return e
		}
		if p.missing && g.state.HardPolicy {
			return errors.New("llmbudget: prior hard policy missing; restore it before admission")
		}
		if hasHardPolicy(p) {
			g.state.HardPolicy = true
		}
		if old, ok, e := g.completion(r.ID); e != nil {
			return e
		} else if ok {
			if !reflect.DeepEqual(old.Request, r) {
				return errors.New("llmbudget: reused completed reservation id")
			}
			out = Admission{Decision: Decision{Action: Block, Model: r.Model, Reason: "reservation already completed"}}
			return nil
		}
		if old, ok := g.state.Reservations[r.ID]; ok {
			if !g.ownerLive(r.Owner) {
				return errors.New("llmbudget: existing reservation owner lease is not live")
			}
			if !reflect.DeepEqual(old.Request, r) {
				return errors.New("llmbudget: reused reservation id with different demand")
			}
			out = Admission{Decision: Decision{Action: Allow, Model: r.Model, Lane: r.Lane}, Reservation: &old}
			return nil
		}
		if !g.ownerLive(r.Owner) {
			return errors.New("llmbudget: owner lease is not live")
		}
		if len(g.state.Reservations) >= maxCompletions {
			return errors.New("llmbudget: active reservation capacity reached; reconcile ended work before new admission")
		}
		out = g.evaluate(p, g.state, r)
		if out.Decision.Action != Allow {
			return nil
		}
		if m, ok := g.model(r.Model); ok && m.Limits.RateTokens > 0 && m.Limits.RatePer > 0 {
			if delay := g.reserveRate(m.Provider, m.Limits, r.Tokens, g.now()); delay > 0 {
				return errors.New("llmbudget: rate changed during admission")
			}
		}
		now := g.now()
		res := Reservation{ID: r.ID, Owner: r.Owner, Request: r, CreatedAt: now, RenewedAt: now, ExpiresAt: now.Add(r.TTL)}
		g.state.Reservations[r.ID] = res
		out.Reservation = &res
		return nil
	})
	if err != nil {
		return refused(r, err)
	}
	return out, nil
}
func (g *Gate) evaluate(p *Policy, s State, r Request) Admission {
	now := g.now()
	out := Admission{Decision: Decision{Action: Allow, Model: r.Model, Lane: r.Lane}}
	queue := func(reason string, reset time.Time) Admission {
		out.Decision.Action = Queue
		out.Decision.Reason = reason
		if !reset.IsZero() {
			out.RetryAt = &reset
			out.Decision.Delay = reset.Sub(now)
			if out.Decision.Delay < 0 {
				out.Decision.Delay = 0
			}
		}
		for _, route := range p.Routes {
			if route.From == r.Model && route.To != "" {
				b, ok := bindingFor(p, route.To, "")
				if !ok {
					continue
				}
				if r.Lane == LaneSubscription && b.Lane == LaneAPIKey && !(route.AllowPremium && r.AllowPremium) {
					continue
				}
				out.Decision = Decision{Action: RouteAlt, Model: route.To, Lane: b.Lane, Reason: reason + "; reserve explicitly permitted alternative before launch"}
				break
			}
		}
		return out
	}
	b := requestBinding(r)
	for _, c := range p.Constraints {
		if c.Soft {
			continue
		}
		// A request lacking account mapping cannot evade a matching provider cap.
		probe := b
		if probe.Provider == "" && (r.Model != "" || r.UnknownTokens) {
			probe.Provider = c.Provider
		}
		if probe.Lane == "" && (r.Model != "" || r.UnknownTokens) {
			probe.Lane = c.Lane
		}
		if probe.Account == "" {
			probe.Account = c.Account
		}
		if probe.Pool == "" {
			probe.Pool = c.Pool
		}
		if !matches(c, probe, r.Host) {
			continue
		}
		vendorCap := c.DailyTokens != nil || c.WeeklyTokens != nil || c.DailySpendMicroUSD != nil || c.WeeklySpendMicroUSD != nil || c.Concurrency != nil
		if vendorCap && (r.Model != "" || r.UnknownTokens) && b.Account == "" {
			return queue("account identity unknown under configured limit", time.Time{})
		}
		var dayTokens, weekTokens, daySpend, weekSpend int64
		var concurrency, slots int64
		var memory uint64
		unknownSpend := false
		unknownTokens, unknownMemory := r.UnknownTokens, r.UnknownMemory
		for _, v := range s.Pools {
			if matches(c, v.Binding, "") {
				cc := currentCounters(v.Counters, now)
				unknownTokens = unknownTokens || v.UnknownTokens && v.UnknownTokensAt != nil && ((c.DailyTokens != nil && dayStart(*v.UnknownTokensAt).Equal(dayStart(now))) || (c.WeeklyTokens != nil && weekStart(*v.UnknownTokensAt).Equal(weekStart(now))))
				dayTokens = safeAdd(dayTokens, cc.DayTokens)
				weekTokens = safeAdd(weekTokens, cc.WeekTokens)
				daySpend = safeAdd(daySpend, microUSD(cc.DayCostUSD))
				weekSpend = safeAdd(weekSpend, microUSD(cc.WeekCostUSD))
				unknownSpend = unknownSpend || v.UnknownSpend && ((c.DailySpendMicroUSD != nil && dayStart(v.UnknownSpendAt).Equal(dayStart(now))) || (c.WeeklySpendMicroUSD != nil && weekStart(v.UnknownSpendAt).Equal(weekStart(now))))
			}
		}
		// Historical / legacy callers have no account attribution. Charge relevant
		// model totals conservatively to each matching configured account, once.
		for model, v := range s.Unattributed {
			relevant := false
			for _, bound := range p.Bindings {
				if bound.Model == model && matches(c, bound, "") {
					relevant = true
					break
				}
			}
			if relevant {
				cc := currentCounters(v, now)
				dayTokens = safeAdd(dayTokens, cc.DayTokens)
				weekTokens = safeAdd(weekTokens, cc.WeekTokens)
				daySpend = safeAdd(daySpend, microUSD(cc.DayCostUSD))
				weekSpend = safeAdd(weekSpend, microUSD(cc.WeekCostUSD))
			}
		}
		for _, v := range s.Reservations {
			if matches(c, requestBinding(v.Request), v.Request.Host) {
				q := v.Request
				unknownTokens = unknownTokens || q.UnknownTokens
				unknownMemory = unknownMemory || q.UnknownMemory
				dayTokens = safeAdd(dayTokens, q.Tokens)
				weekTokens = safeAdd(weekTokens, q.Tokens)
				concurrency = safeAdd(concurrency, int64(q.Concurrency))
				slots = safeAdd(slots, int64(q.HostSlots))
				if math.MaxUint64-memory < q.MemoryBytes {
					memory = math.MaxUint64
				} else {
					memory += q.MemoryBytes
				}
				if q.SpendMicroUSD != nil {
					daySpend = safeAdd(daySpend, *q.SpendMicroUSD)
					weekSpend = safeAdd(weekSpend, *q.SpendMicroUSD)
				} else if q.Tokens > 0 || q.UnknownTokens {
					unknownSpend = true
				}
			}
		}
		if unknownTokens && (c.DailyTokens != nil || c.WeeklyTokens != nil) {
			return queue("token demand unknown under configured hard budget", time.Time{})
		}
		if unknownMemory && c.MemoryBytes != nil {
			return queue("memory demand unknown under configured hard budget", time.Time{})
		}
		if c.Concurrency != nil && safeAdd(concurrency, int64(r.Concurrency)) > int64(*c.Concurrency) {
			return queue("account concurrency budget reserved", time.Time{})
		}
		if c.HostSlots != nil && safeAdd(slots, int64(r.HostSlots)) > int64(*c.HostSlots) {
			return queue("host work slots reserved", time.Time{})
		}
		if c.MemoryBytes != nil && (memory > *c.MemoryBytes || r.MemoryBytes > *c.MemoryBytes-memory) {
			return queue("host memory allocation reserved", time.Time{})
		}
		if c.DailyTokens != nil && safeAdd(dayTokens, r.Tokens) > *c.DailyTokens {
			return queue("daily token budget exhausted", dayStart(now).AddDate(0, 0, 1))
		}
		if c.WeeklyTokens != nil && safeAdd(weekTokens, r.Tokens) > *c.WeeklyTokens {
			return queue("weekly token budget exhausted", weekStart(now).AddDate(0, 0, 7))
		}
		if c.DailySpendMicroUSD != nil || c.WeeklySpendMicroUSD != nil {
			if (r.Tokens > 0 || r.UnknownTokens) && r.SpendMicroUSD == nil || unknownSpend {
				return queue("spend unknown under configured hard budget", time.Time{})
			}
			demandSpend := int64(0)
			if r.SpendMicroUSD != nil {
				demandSpend = *r.SpendMicroUSD
			}
			if c.DailySpendMicroUSD != nil && safeAdd(daySpend, demandSpend) > *c.DailySpendMicroUSD {
				return queue("daily spend budget exhausted", dayStart(now).AddDate(0, 0, 1))
			}
			if c.WeeklySpendMicroUSD != nil && safeAdd(weekSpend, demandSpend) > *c.WeeklySpendMicroUSD {
				return queue("weekly spend budget exhausted", weekStart(now).AddDate(0, 0, 7))
			}
		}
	}
	// Preserve configured legacy limits without consuming Check's rate bucket or
	// firing telemetry. Explicit hard policy above can never be premium-bypassed.
	if r.Model != "" {
		clone := New(g.cfg)
		clone.cfg.BoundHit = func(context.Context, string, int64, int64, string) {}
		clone.cfg.StatePath = ""
		clone.state = State{}
		clone.loaded = true
		clone.inTransaction = true
		// Deep clone because legacy rate checking mutates maps.
		raw, _ := json.Marshal(s)
		_ = json.Unmarshal(raw, &clone.state)
		normalizeState(&clone.state)
		for _, res := range s.Reservations {
			q := res.Request
			if q.Model == "" {
				continue
			}
			cost := 0.0
			if q.SpendMicroUSD != nil {
				cost = float64(*q.SpendMicroUSD) / 1e6
			} else if model, ok := clone.model(q.Model); ok {
				cost, _ = estimateCost(model, q.Tokens)
			}
			clone.recordLocal(q.Model, q.Tokens, 0, cost)
		}
		if model, ok := clone.model(r.Model); ok && (model.Limits.BudgetUSD > 0 || model.Limits.ProviderUSD > 0 || model.Limits.ProviderQuotaUSD > 0) && model.CostMicro <= 0 && r.Tokens > 0 {
			return queue("price unknown under configured legacy spend budget", time.Time{})
		}
		d := clone.checkLegacy(context.Background(), r.Model, r.Tokens, r.AllowPremium)
		if d.Action != Allow {
			return Admission{Decision: d}
		}
	}
	return out
}
func safeAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
func (g *Gate) Renew(ctx context.Context, id, owner string, ttl time.Duration) error {
	if ttl < time.Second || ttl > 24*time.Hour {
		return errors.New("llmbudget: invalid renewal TTL")
	}
	return g.transaction(ctx, func() error {
		r, ok := g.state.Reservations[id]
		if !ok {
			return errors.New("llmbudget: reservation missing")
		}
		if r.Owner != owner || !g.ownerLive(owner) {
			return errors.New("llmbudget: reservation owner not live")
		}
		r.RenewedAt = g.now()
		r.ExpiresAt = r.RenewedAt.Add(ttl)
		g.state.Reservations[id] = r
		return nil
	})
}
func (g *Gate) Settle(ctx context.Context, id, owner string, a Actual) error {
	if a.InputTokens < 0 || a.OutputTokens < 0 || a.CachedInputTokens < 0 || a.CachedInputTokens > a.InputTokens || a.InputTokens > math.MaxInt64-a.OutputTokens || a.SpendMicroUSD != nil && *a.SpendMicroUSD < 0 {
		return errors.New("llmbudget: invalid actual usage")
	}
	return g.transaction(ctx, func() error {
		if old, ok, e := g.completion(id); e != nil {
			return e
		} else if ok {
			if old.Owner == owner && old.Kind == "settled" && reflect.DeepEqual(old.Actual, &a) {
				return nil
			}
			return errors.New("llmbudget: conflicting settlement")
		}
		r, ok := g.state.Reservations[id]
		if !ok || r.Owner != owner {
			return errors.New("llmbudget: reservation/owner mismatch")
		}
		cost := 0.0
		if a.SpendMicroUSD != nil {
			cost = float64(*a.SpendMicroUSD) / 1e6
		} else if r.Request.SpendMicroUSD != nil {
			cost = float64(*r.Request.SpendMicroUSD) / 1e6
		}
		b := requestBinding(r.Request)
		key := poolKey(b)
		pool := g.state.Pools[key]
		pool.Binding = b
		pool.Counters = addCounters(pool.Counters, g.now(), a.InputTokens+a.OutputTokens, 1, cost)
		pool.InputTokens = safeAdd(pool.InputTokens, a.InputTokens)
		pool.OutputTokens = safeAdd(pool.OutputTokens, a.OutputTokens)
		pool.CachedInputTokens = safeAdd(pool.CachedInputTokens, a.CachedInputTokens)
		pool.ObservedAt = g.now()
		if a.TokensEstimated {
			pool.EstimatedTokens = true
			pool.UnknownTokens = true
			at := g.now()
			pool.UnknownTokensAt = &at
		}
		if (a.SpendMicroUSD == nil || a.TokensEstimated) && (r.Request.Tokens > 0 || r.Request.UnknownTokens) {
			pool.UnknownSpend = true
			pool.UnknownSpendAt = g.now()
		}
		g.state.Pools[key] = pool
		if r.Request.Model != "" {
			g.recordLocal(r.Request.Model, a.InputTokens, a.OutputTokens, cost)
		}
		g.state.Completed[id] = Completion{Owner: owner, Request: r.Request, Actual: &a, At: g.now(), Kind: "settled"}
		delete(g.state.Reservations, id)
		return nil
	})
}
func (g *Gate) Release(ctx context.Context, id, owner string) error {
	return g.transaction(ctx, func() error {
		if old, ok, e := g.completion(id); e != nil {
			return e
		} else if ok {
			if old.Owner == owner {
				return nil
			}
			return errors.New("llmbudget: release owner mismatch")
		}
		r, ok := g.state.Reservations[id]
		if !ok {
			return errors.New("llmbudget: reservation missing")
		}
		if r.Owner != owner {
			return errors.New("llmbudget: release owner mismatch")
		}
		g.state.Completed[id] = Completion{Owner: owner, Request: r.Request, At: g.now(), Kind: "released"}
		delete(g.state.Reservations, id)
		return nil
	})
}
func (g *Gate) ReconcileTerminated(ctx context.Context, id string, p TerminationProof) error {
	if p.VerifiedAt.IsZero() || p.VerifiedAt.After(g.now().Add(time.Minute)) || strings.TrimSpace(p.Evidence) == "" {
		return errors.New("llmbudget: verified termination evidence required")
	}
	return g.transaction(ctx, func() error {
		if old, ok, e := g.completion(id); e != nil {
			return e
		} else if ok {
			if old.Kind == "terminated" && reflect.DeepEqual(old.Termination, &p) {
				return nil
			}
			return errors.New("llmbudget: conflicting termination")
		}
		r, ok := g.state.Reservations[id]
		if !ok || r.Owner != p.Owner || r.Request.Run != p.Run || r.Request.Host != p.Host || p.VerifiedAt.Before(r.CreatedAt) {
			return errors.New("llmbudget: termination proof identity mismatch")
		}
		if r.Request.UnknownTokens || r.Request.Tokens > 0 || r.Request.SpendMicroUSD != nil && *r.Request.SpendMicroUSD > 0 {
			b := requestBinding(r.Request)
			k := poolKey(b)
			pool := g.state.Pools[k]
			pool.Binding = b
			cost := 0.0
			if r.Request.SpendMicroUSD != nil {
				cost = float64(*r.Request.SpendMicroUSD) / 1e6
			}
			pool.Counters = addCounters(pool.Counters, g.now(), r.Request.Tokens, 1, cost)
			pool.UnknownSpend = true
			pool.UnknownSpendAt = g.now()
			if r.Request.UnknownTokens {
				pool.UnknownTokens = true
				at := g.now()
				pool.UnknownTokensAt = &at
			}
			pool.ObservedAt = g.now()
			g.state.Pools[k] = pool
			if r.Request.Model != "" {
				g.recordLocal(r.Request.Model, r.Request.Tokens, 0, cost)
			}
		}
		g.state.Completed[id] = Completion{Owner: p.Owner, Request: r.Request, At: g.now(), Kind: "terminated", Termination: &p}
		delete(g.state.Reservations, id)
		return nil
	})
}
