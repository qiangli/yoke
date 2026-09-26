package cligw

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Autoscaler is the Apache prefork-style prewarmer over a set of agent pools.
//
// It owns no workers. Every decision is expressed through the two pool hooks
// SetMinSpare (how many warm idle workers to hold) and SetMaxSpare (the idle
// ceiling, which is how a retirement is asked for), so there is exactly one
// pool implementation and the scaler stays a pure control loop:
//
//   - scale up: while idle warm < the effective min_spare, raise the target by
//     an exponential 1, 2, 4, … per CONSECUTIVE deficit tick (the ramp resets
//     as soon as the target is met), capped by the pool's MaxWorkers, a
//     host-wide worker cap and a per-vendor concurrency cap;
//   - scale down: while idle warm > the effective max_spare, drop the ceiling
//     by one per tick, which retires the oldest idle worker first;
//   - cold: a pool with no request for IdleTTL has an effective min_spare of
//     0 and an effective max_spare of 0, so it drains to fully cold;
//   - predictive: the effective min_spare also rises to ⌈λ·W⌉ (Little's law)
//     from the EWMA arrival rate λ and EWMA service time W of that pool;
//   - band spares: band demand is split across the band's agents in Ranker
//     order, weighted so the leader — the agent the policy would pick next —
//     gets the most, and an agent below the quota reserve floor or with an
//     open spawn breaker gets none.
//
// The clock and the tick are injectable, so the control loop is testable
// without sleeping: Tick() runs exactly one control step at Clock() time and
// Run() is only the ticker that calls it.
type Autoscaler struct {
	mu  sync.Mutex
	cfg AutoscaleConfig

	pools map[string]*poolState
	order []string
	bands map[int]*bandState

	hostCap int
}

// Ranker is the routing rank the policy layer (L8) supplies: the agents of a
// band, best first. Band spares follow this order, so a warm worker sits on
// the agent the router would pick next. A nil Ranker falls back to pool
// registration order; a non-nil Ranker is authoritative — an agent it omits is
// one the router would not pick, and it is not prewarmed for that band.
type Ranker interface {
	Rank(band int) []string
}

// Headroom reports an agent's remaining quota as a fraction in [0,1] — the
// policy layer's view of the seat. known is false when the agent's quota is
// not tracked, which is treated as "no reason to hold back", never as zero.
type Headroom func(agent string) (fraction float64, known bool)

// RankerFunc adapts a plain function to Ranker.
type RankerFunc func(band int) []string

// Rank implements Ranker.
func (f RankerFunc) Rank(band int) []string { return f(band) }

// AutoscaleConfig is the operator-visible knob set. The zero value is valid:
// every field falls back to the documented default.
type AutoscaleConfig struct {
	// Tick is the control-loop period (default 1s). Rates are computed
	// against it, so a simulated clock must advance by Tick per Tick call.
	Tick time.Duration
	// HostMaxWorkers caps warm+busy workers across ALL pools. Zero uses
	// DefaultHostMaxWorkers().
	HostMaxWorkers int
	// PerWorkerMemoryBytes is the memory a worker is assumed to need when
	// deriving the default host cap from free memory (default 512 MiB).
	PerWorkerMemoryBytes uint64
	// VendorMaxWorkers caps concurrent workers per provider (the vendor's
	// concurrent-session cap). An absent provider is uncapped.
	VendorMaxWorkers map[string]int
	// ReserveFloor is the quota fraction below which an agent is no longer
	// prewarmed (default 0.15). Requests still route to it; only speculative
	// spare capacity stops.
	ReserveFloor float64
	// MaxSpawnRate caps the 1, 2, 4, … ramp. Zero uses the pool's MaxWorkers.
	MaxSpawnRate int
	// Alpha is the EWMA smoothing factor for λ and W (default 0.5).
	Alpha float64
	// FallbackServiceTime is W for a band whose agents have not served a
	// request yet (default 2s). A per-pool λ·W target needs a measured W and
	// is skipped until one exists.
	FallbackServiceTime time.Duration
	// BreakerFailures is how many consecutive failed spawns stop prewarming
	// a pool (default 3). A successful spawn closes the breaker.
	BreakerFailures int

	// Clock is the time source (default time.Now).
	Clock func() time.Time
	// Ranker supplies the per-band routing rank; see Ranker.
	Ranker Ranker
	// Headroom reports per-agent quota headroom; see Headroom.
	Headroom Headroom
}

// Autoscale defaults, exported so an operator surface can show them.
const (
	DefaultAutoscaleTick        = time.Second
	DefaultReserveFloor         = 0.15
	DefaultAutoscaleAlpha       = 0.5
	DefaultFallbackServiceTime  = 2 * time.Second
	DefaultBreakerFailures      = 3
	DefaultPerWorkerMemoryBytes = uint64(512) << 20

	// AutoscaleSchemaVersion is the Snapshot envelope version.
	AutoscaleSchemaVersion = "bashy-cligw-autoscale-v1"
)

// ScalablePool is the autoscaler's view of a pool; *Pool implements it.
type ScalablePool interface {
	Stats() PoolStats
	Config() PoolConfig
	SetMinSpare(int)
	SetMaxSpare(int)
}

var _ ScalablePool = (*Pool)(nil)

// PoolRegistration binds one pool to the agent, provider and band the scaler
// accounts it under.
type PoolRegistration struct {
	Agent    string
	Provider string
	Band     int
	Pool     ScalablePool
}

type poolState struct {
	reg  PoolRegistration
	pool ScalablePool
	// base is the OPERATOR's spare configuration, read once at registration.
	// The live PoolConfig cannot be used for it: the scaler writes MinSpare
	// and MaxSpare itself, so reading them back would make each tick's
	// decision the previous tick's decision.
	base PoolConfig

	arrivals int
	lambda   float64
	service  float64
	measured bool

	lastActive time.Time

	spawnRate int
	bandShare int

	lastSpawned int64
	lastFailed  int64
	failStreak  int

	headroom      float64
	headroomKnown bool

	cold    bool
	breaker bool
	target  int
	ceiling int
}

type bandState struct {
	arrivals int
	lambda   float64
	prewarm  int
}

// NewAutoscaler returns a scaler with cfg's zero fields defaulted. Nothing
// runs until Tick or Run is called.
func NewAutoscaler(cfg AutoscaleConfig) *Autoscaler {
	if cfg.Tick <= 0 {
		cfg.Tick = DefaultAutoscaleTick
	}
	if cfg.PerWorkerMemoryBytes == 0 {
		cfg.PerWorkerMemoryBytes = DefaultPerWorkerMemoryBytes
	}
	if cfg.ReserveFloor <= 0 {
		cfg.ReserveFloor = DefaultReserveFloor
	}
	if cfg.Alpha <= 0 || cfg.Alpha > 1 {
		cfg.Alpha = DefaultAutoscaleAlpha
	}
	if cfg.FallbackServiceTime <= 0 {
		cfg.FallbackServiceTime = DefaultFallbackServiceTime
	}
	if cfg.BreakerFailures <= 0 {
		cfg.BreakerFailures = DefaultBreakerFailures
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	hostCap := cfg.HostMaxWorkers
	if hostCap <= 0 {
		hostCap = hostWorkerCap(runtime.NumCPU(), availableMemoryBytes, cfg.PerWorkerMemoryBytes)
	}
	return &Autoscaler{
		cfg:     cfg,
		pools:   make(map[string]*poolState),
		bands:   make(map[int]*bandState),
		hostCap: hostCap,
	}
}

// Config returns the effective configuration after defaulting.
func (a *Autoscaler) Config() AutoscaleConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// HostMaxWorkers returns the host-wide worker cap in force.
func (a *Autoscaler) HostMaxWorkers() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hostCap
}

// Register adds or replaces a pool. Replacing resets that agent's EWMA state,
// because λ and W describe a pool's workers, not its name.
func (a *Autoscaler) Register(reg PoolRegistration) error {
	if strings.TrimSpace(reg.Agent) == "" {
		return fmt.Errorf("cligw: autoscaler registration needs an agent name")
	}
	if reg.Pool == nil {
		return fmt.Errorf("cligw: autoscaler registration for %q needs a pool", reg.Agent)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.pools[reg.Agent]; !ok {
		a.order = append(a.order, reg.Agent)
	}
	a.pools[reg.Agent] = &poolState{reg: reg, pool: reg.Pool, base: reg.Pool.Config(), lastActive: a.cfg.Clock()}
	if reg.Band > 0 && a.bands[reg.Band] == nil {
		a.bands[reg.Band] = &bandState{}
	}
	return nil
}

// Unregister drops a pool from the control loop. It does not close the pool.
func (a *Autoscaler) Unregister(agent string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.pools[agent]; !ok {
		return
	}
	delete(a.pools, agent)
	for i, name := range a.order {
		if name == agent {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
}

// Observe records one served request: its arrival instant and how long the
// worker was busy. It feeds the pool's EWMA λ and W and marks the pool active,
// which is what keeps it out of the cold state.
func (a *Autoscaler) Observe(agent string, arrival time.Time, service time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ps := a.pools[agent]
	if ps == nil {
		return
	}
	ps.arrivals++
	if arrival.After(ps.lastActive) {
		ps.lastActive = arrival
	}
	if service > 0 {
		seconds := service.Seconds()
		if ps.measured {
			ps.service = a.cfg.Alpha*seconds + (1-a.cfg.Alpha)*ps.service
		} else {
			ps.service, ps.measured = seconds, true
		}
	}
}

// ObserveBand records one request that named a band rather than an agent. The
// resulting demand is prewarmed across the band's agents in Ranker order; the
// agent that actually serves it reports through Observe, so band demand and
// per-agent demand are combined with max, never added.
func (a *Autoscaler) ObserveBand(band int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if band <= 0 {
		return
	}
	bs := a.bands[band]
	if bs == nil {
		bs = &bandState{}
		a.bands[band] = bs
	}
	bs.arrivals++
}

// Run drives Tick every cfg.Tick until ctx is done. Tests call Tick directly.
func (a *Autoscaler) Run(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(a.cfg.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			a.Tick()
		case <-ctx.Done():
			return
		}
	}
}

// Tick runs exactly one control step: refresh the rate estimates, split band
// demand, then set each pool's spare targets.
func (a *Autoscaler) Tick() {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.cfg.Clock()
	seconds := a.cfg.Tick.Seconds()
	alpha := a.cfg.Alpha

	stats := make(map[string]PoolStats, len(a.pools))
	configs := make(map[string]PoolConfig, len(a.pools))
	for _, name := range a.order {
		ps := a.pools[name]
		st := ps.pool.Stats()
		cfg := ps.pool.Config()
		stats[name], configs[name] = st, cfg

		ps.lambda = alpha*(float64(ps.arrivals)/seconds) + (1-alpha)*ps.lambda
		ps.arrivals = 0

		spawned, failed := st.Spawned-ps.lastSpawned, st.Failed-ps.lastFailed
		switch {
		case spawned > 0:
			ps.failStreak = 0
		case failed > 0:
			ps.failStreak += int(failed)
		}
		ps.lastSpawned, ps.lastFailed = st.Spawned, st.Failed
		ps.breaker = ps.failStreak >= a.cfg.BreakerFailures

		ps.headroom, ps.headroomKnown = 1, false
		if a.cfg.Headroom != nil {
			ps.headroom, ps.headroomKnown = a.cfg.Headroom(name)
		}

		if st.Busy > 0 || st.Queued > 0 {
			ps.lastActive = now
		}
		ps.cold = cfg.IdleTTL > 0 && now.Sub(ps.lastActive) >= cfg.IdleTTL
	}

	for band, bs := range a.bands {
		bs.lambda = alpha*(float64(bs.arrivals)/seconds) + (1-alpha)*bs.lambda
		bs.arrivals = 0
		bs.prewarm = a.splitBandLocked(band, bs)
	}

	workers, vendor := 0, make(map[string]int)
	for _, name := range a.order {
		st := stats[name]
		workers += st.Idle + st.Busy
		if provider := a.pools[name].reg.Provider; provider != "" {
			vendor[provider] += st.Idle + st.Busy
		}
	}

	for _, name := range a.order {
		ps, st, cfg := a.pools[name], stats[name], configs[name]

		want := 0
		if !ps.cold && !ps.gated(a.cfg.ReserveFloor) {
			want = ps.base.MinSpare
			if ps.measured {
				if predicted := ceilInt(ps.lambda * ps.service); predicted > want {
					want = predicted
				}
			}
			if ps.bandShare > want {
				want = ps.bandShare
			}
		}
		if room := cfg.MaxWorkers - st.Busy; want > room {
			want = room
		}
		if want < 0 {
			want = 0
		}

		if want > st.Idle {
			rate := ps.spawnRate
			if rate <= 0 {
				rate = 1
			} else {
				rate *= 2
			}
			if limit := a.spawnRateCap(cfg); rate > limit {
				rate = limit
			}
			ps.spawnRate = rate
			if step := st.Idle + rate; want > step {
				want = step
			}
		} else {
			ps.spawnRate = 0
		}

		// Host-wide and per-vendor caps apply to the NEW workers this target
		// asks for, so a pool already over a cap is drained, never grown.
		if extra := want - st.Idle; extra > 0 {
			if room := a.hostCap - workers; extra > room {
				extra = room
			}
			if limit, ok := a.cfg.VendorMaxWorkers[ps.reg.Provider]; ok && ps.reg.Provider != "" {
				if room := limit - vendor[ps.reg.Provider]; extra > room {
					extra = room
				}
			}
			if extra < 0 {
				extra = 0
			}
			want = st.Idle + extra
		}

		ceiling := ps.base.MaxSpare
		if ps.cold || ps.gated(a.cfg.ReserveFloor) {
			ceiling = 0
		}
		if ceiling < want {
			ceiling = want
		}
		if st.Idle > ceiling {
			// Retire exactly one per tick, oldest first (the pool's own rule).
			ceiling = st.Idle - 1
			if want > ceiling {
				want = ceiling
			}
		}

		ps.pool.SetMaxSpare(ceiling)
		ps.pool.SetMinSpare(want)
		ps.target, ps.ceiling = want, ceiling

		if extra := want - st.Idle; extra > 0 {
			workers += extra
			if ps.reg.Provider != "" {
				vendor[ps.reg.Provider] += extra
			}
		}
	}
}

func (ps *poolState) gated(floor float64) bool {
	return ps.breaker || (ps.headroomKnown && ps.headroom < floor)
}

func (a *Autoscaler) spawnRateCap(cfg PoolConfig) int {
	if a.cfg.MaxSpawnRate > 0 {
		return a.cfg.MaxSpawnRate
	}
	if cfg.MaxWorkers > 0 {
		return cfg.MaxWorkers
	}
	return 1
}

// splitBandLocked assigns bandShare to the band's eligible agents in Ranker
// order, weighted 1, 1/2, 1/3, … so the leader gets the most, and returns the
// total assigned.
func (a *Autoscaler) splitBandLocked(band int, bs *bandState) int {
	members := a.bandMembersLocked(band)
	for _, ps := range members {
		ps.bandShare = 0
	}
	if len(members) == 0 {
		return 0
	}
	eligible := make([]*poolState, 0, len(members))
	for _, ps := range members {
		if !ps.gated(a.cfg.ReserveFloor) {
			eligible = append(eligible, ps)
		}
	}
	if len(eligible) == 0 || bs.lambda <= 0 {
		return 0
	}
	demand := ceilInt(bs.lambda * a.bandServiceLocked(members))
	if demand <= 0 {
		return 0
	}
	total := 0
	for i, share := range splitDemand(demand, len(eligible)) {
		eligible[i].bandShare = share
		total += share
	}
	return total
}

// bandMembersLocked returns the band's pools in Ranker order. Agents the
// Ranker omits are excluded; with no Ranker, registration order is used.
func (a *Autoscaler) bandMembersLocked(band int) []*poolState {
	if a.cfg.Ranker != nil {
		ranked := a.cfg.Ranker.Rank(band)
		out := make([]*poolState, 0, len(ranked))
		for _, name := range ranked {
			if ps := a.pools[name]; ps != nil && ps.reg.Band == band {
				out = append(out, ps)
			}
		}
		return out
	}
	out := make([]*poolState, 0, len(a.order))
	for _, name := range a.order {
		if ps := a.pools[name]; ps != nil && ps.reg.Band == band {
			out = append(out, ps)
		}
	}
	return out
}

// bandServiceLocked is W for the band: the mean measured service time of its
// agents, or FallbackServiceTime while none has served a request.
func (a *Autoscaler) bandServiceLocked(members []*poolState) float64 {
	sum, n := 0.0, 0
	for _, ps := range members {
		if ps.measured {
			sum += ps.service
			n++
		}
	}
	if n == 0 {
		return a.cfg.FallbackServiceTime.Seconds()
	}
	return sum / float64(n)
}

// splitDemand spreads total over n seats with harmonic weights and largest
// remainders, so seat 0 gets the most and the parts sum to total.
func splitDemand(total, n int) []int {
	out := make([]int, n)
	if n <= 0 || total <= 0 {
		return out
	}
	weights := make([]float64, n)
	sum := 0.0
	for i := range weights {
		weights[i] = 1 / float64(i+1)
		sum += weights[i]
	}
	type part struct {
		index int
		frac  float64
	}
	assigned := 0
	parts := make([]part, 0, n)
	for i := range weights {
		exact := float64(total) * weights[i] / sum
		whole := int(math.Floor(exact))
		out[i] = whole
		assigned += whole
		parts = append(parts, part{index: i, frac: exact - float64(whole)})
	}
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].frac > parts[j].frac })
	for i := 0; assigned < total && i < len(parts); i++ {
		out[parts[i].index]++
		assigned++
	}
	return out
}

// PoolSnapshot is one pool's line in the live view (bashy llm pools).
type PoolSnapshot struct {
	Agent          string  `json:"agent"`
	Provider       string  `json:"provider,omitempty"`
	Band           int     `json:"band,omitempty"`
	Idle           int     `json:"idle"`
	Busy           int     `json:"busy"`
	Queued         int     `json:"queued"`
	SpawnRate      int     `json:"spawn_rate"`
	MinSpare       int     `json:"min_spare"`
	MaxSpare       int     `json:"max_spare"`
	MaxWorkers     int     `json:"max_workers"`
	Lambda         float64 `json:"lambda"`
	ServiceSeconds float64 `json:"service_seconds"`
	ServiceKnown   bool    `json:"service_known"`
	Headroom       float64 `json:"headroom"`
	HeadroomKnown  bool    `json:"headroom_known"`
	BandShare      int     `json:"band_share"`
	Cold           bool    `json:"cold"`
	BreakerOpen    bool    `json:"breaker_open"`
	Spawned        int64   `json:"spawned"`
	Failed         int64   `json:"failed"`
}

// BandSnapshot is one band's totals in the live view.
type BandSnapshot struct {
	Band    int     `json:"band"`
	Agents  int     `json:"agents"`
	Idle    int     `json:"idle"`
	Busy    int     `json:"busy"`
	Queued  int     `json:"queued"`
	Lambda  float64 `json:"lambda"`
	Prewarm int     `json:"prewarm"`
}

// AutoscaleSnapshot is the bashy-cligw-autoscale-v1 envelope.
type AutoscaleSnapshot struct {
	SchemaVersion  string         `json:"schema_version"`
	At             time.Time      `json:"at"`
	HostMaxWorkers int            `json:"host_max_workers"`
	Workers        int            `json:"workers"`
	Pools          []PoolSnapshot `json:"pools"`
	Bands          []BandSnapshot `json:"bands"`
}

// Snapshot reads every pool's live gauges together with the scaler's own
// estimates. Pools keep registration order; bands are band-number ordered.
func (a *Autoscaler) Snapshot() AutoscaleSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := AutoscaleSnapshot{
		SchemaVersion:  AutoscaleSchemaVersion,
		At:             a.cfg.Clock(),
		HostMaxWorkers: a.hostCap,
		Pools:          make([]PoolSnapshot, 0, len(a.order)),
		Bands:          make([]BandSnapshot, 0, len(a.bands)),
	}
	bands := make(map[int]*BandSnapshot, len(a.bands))
	for band := range a.bands {
		bands[band] = &BandSnapshot{Band: band, Lambda: a.bands[band].lambda, Prewarm: a.bands[band].prewarm}
	}
	for _, name := range a.order {
		ps := a.pools[name]
		st, cfg := ps.pool.Stats(), ps.pool.Config()
		out.Workers += st.Idle + st.Busy
		out.Pools = append(out.Pools, PoolSnapshot{
			Agent:          name,
			Provider:       ps.reg.Provider,
			Band:           ps.reg.Band,
			Idle:           st.Idle,
			Busy:           st.Busy,
			Queued:         st.Queued,
			SpawnRate:      ps.spawnRate,
			MinSpare:       cfg.MinSpare,
			MaxSpare:       cfg.MaxSpare,
			MaxWorkers:     cfg.MaxWorkers,
			Lambda:         ps.lambda,
			ServiceSeconds: ps.service,
			ServiceKnown:   ps.measured,
			Headroom:       ps.headroom,
			HeadroomKnown:  ps.headroomKnown,
			BandShare:      ps.bandShare,
			Cold:           ps.cold,
			BreakerOpen:    ps.breaker,
			Spawned:        st.Spawned,
			Failed:         st.Failed,
		})
		if bs := bands[ps.reg.Band]; bs != nil {
			bs.Agents++
			bs.Idle += st.Idle
			bs.Busy += st.Busy
			bs.Queued += st.Queued
		}
	}
	numbers := make([]int, 0, len(bands))
	for band := range bands {
		numbers = append(numbers, band)
	}
	sort.Ints(numbers)
	for _, band := range numbers {
		out.Bands = append(out.Bands, *bands[band])
	}
	return out
}

// DefaultHostMaxWorkers is the host-wide worker cap used when the operator
// sets none: CPU count, lowered when free memory cannot back that many
// workers. Free memory is only consulted where it is a cheap read.
func DefaultHostMaxWorkers() int {
	return hostWorkerCap(runtime.NumCPU(), availableMemoryBytes, DefaultPerWorkerMemoryBytes)
}

func hostWorkerCap(cpus int, freeMemory func() (uint64, bool), perWorker uint64) int {
	limit := max(cpus, 1)
	if freeMemory == nil || perWorker == 0 {
		return limit
	}
	if free, ok := freeMemory(); ok {
		if byMemory := int(free / perWorker); byMemory < limit {
			limit = byMemory
		}
	}
	return max(limit, 1)
}

// availableMemoryBytes reports memory a new worker could have, but only from
// an interface that costs one file read (/proc/meminfo's MemAvailable). Where
// there is no such interface it reports false and the cap stays CPU-derived —
// a syscall-based probe is not worth a control loop's tick.
func availableMemoryBytes() (uint64, bool) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok || key != "MemAvailable" {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			return 0, false
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}

func ceilInt(v float64) int {
	if v <= 0 || math.IsNaN(v) {
		return 0
	}
	return int(math.Ceil(v))
}
