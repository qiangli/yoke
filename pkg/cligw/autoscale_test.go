package cligw

import (
	"context"
	"testing"
	"time"
)

// fakeClock is the simulated tick source: the autoscaler never sleeps, so a
// test advances time itself and every rate is exact.
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// scalePool stands in for *Pool: it applies the two spare hooks the way a real
// pool eventually does (grow towards MinSpare, trim down to MaxSpare) without
// launching a CLI, and records the targets it was given.
type scalePool struct {
	cfg     PoolConfig
	idle    int
	busy    int
	queued  int
	spawned int64
	failed  int64

	stuck     bool // accepts targets but never produces a worker (a burst)
	failSpawn bool // every spawn fails (a broken tool)

	minCalls []int
	maxCalls []int
}

func newScalePool(cfg PoolConfig) *scalePool { return &scalePool{cfg: cfg} }

func (f *scalePool) Stats() PoolStats {
	return PoolStats{Idle: f.idle, Busy: f.busy, Queued: f.queued, Spawned: f.spawned, Failed: f.failed}
}

func (f *scalePool) Config() PoolConfig { return f.cfg }

func (f *scalePool) SetMinSpare(n int) {
	if n > f.cfg.MaxWorkers {
		n = f.cfg.MaxWorkers
	}
	f.minCalls = append(f.minCalls, n)
	f.cfg.MinSpare = n
	if f.cfg.MaxSpare < n {
		f.cfg.MaxSpare = n
	}
	need := n - f.idle
	if need <= 0 {
		return
	}
	switch {
	case f.failSpawn:
		f.failed += int64(need)
	case f.stuck:
	default:
		f.idle += need
		f.spawned += int64(need)
	}
}

func (f *scalePool) SetMaxSpare(n int) {
	f.maxCalls = append(f.maxCalls, n)
	f.cfg.MaxSpare = n
	if f.cfg.MinSpare > n {
		f.cfg.MinSpare = n
	}
	if f.idle > n {
		f.idle = n
	}
}

func (f *scalePool) lastMin() int {
	if len(f.minCalls) == 0 {
		return -1
	}
	return f.minCalls[len(f.minCalls)-1]
}

// ticks advances the simulated clock by one tick and runs one control step, n
// times, which is the only way these tests move time forward.
func ticks(a *Autoscaler, clk *fakeClock, n int) {
	for i := 0; i < n; i++ {
		clk.advance(a.Config().Tick)
		a.Tick()
	}
}

func newTestScaler(t *testing.T, clk *fakeClock, cfg AutoscaleConfig) *Autoscaler {
	t.Helper()
	cfg.Clock = clk.now
	if cfg.HostMaxWorkers == 0 {
		cfg.HostMaxWorkers = 64
	}
	return NewAutoscaler(cfg)
}

func register(t *testing.T, a *Autoscaler, reg PoolRegistration) {
	t.Helper()
	if err := a.Register(reg); err != nil {
		t.Fatalf("Register(%q): %v", reg.Agent, err)
	}
}

func snapshotFor(t *testing.T, a *Autoscaler, agent string) PoolSnapshot {
	t.Helper()
	for _, ps := range a.Snapshot().Pools {
		if ps.Agent == agent {
			return ps
		}
	}
	t.Fatalf("no snapshot for agent %q", agent)
	return PoolSnapshot{}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAutoscalerBurstDoublesSpawnRateUpToCap(t *testing.T) {
	clk := newFakeClock()
	a := newTestScaler(t, clk, AutoscaleConfig{})
	pool := newScalePool(PoolConfig{MinSpare: 8, MaxSpare: 8, MaxWorkers: 8})
	pool.stuck = true
	register(t, a, PoolRegistration{Agent: "claude", Pool: pool})

	ticks(a, clk, 5)

	// 1, 2, 4 then clamped by MaxWorkers/min_spare, never a step of 16.
	want := []int{1, 2, 4, 8, 8}
	if !equalInts(pool.minCalls, want) {
		t.Fatalf("min spare targets = %v, want %v", pool.minCalls, want)
	}
	if got := snapshotFor(t, a, "claude").SpawnRate; got != 8 {
		t.Fatalf("spawn rate = %d, want 8 (capped by MaxWorkers)", got)
	}
}

func TestAutoscalerResetsSpawnRateWhenSatisfied(t *testing.T) {
	clk := newFakeClock()
	a := newTestScaler(t, clk, AutoscaleConfig{})
	pool := newScalePool(PoolConfig{MinSpare: 4, MaxSpare: 4, MaxWorkers: 8})
	register(t, a, PoolRegistration{Agent: "claude", Pool: pool})

	ticks(a, clk, 4)
	if want := []int{1, 3, 4, 4}; !equalInts(pool.minCalls, want) {
		t.Fatalf("min spare targets = %v, want %v", pool.minCalls, want)
	}
	if got := snapshotFor(t, a, "claude").SpawnRate; got != 0 {
		t.Fatalf("spawn rate after satisfaction = %d, want 0", got)
	}

	// A request takes one warm worker: the ramp restarts at 1, not at 4.
	pool.idle--
	pool.busy++
	ticks(a, clk, 1)
	if got := snapshotFor(t, a, "claude").SpawnRate; got != 1 {
		t.Fatalf("spawn rate after a fresh deficit = %d, want 1", got)
	}
}

func TestAutoscalerShrinksToMaxSpareThenGoesColdAfterIdleTTL(t *testing.T) {
	clk := newFakeClock()
	a := newTestScaler(t, clk, AutoscaleConfig{})
	pool := newScalePool(PoolConfig{MinSpare: 1, MaxSpare: 3, MaxWorkers: 8, IdleTTL: 10 * time.Second})
	pool.idle = 6
	register(t, a, PoolRegistration{Agent: "claude", Pool: pool})
	a.Observe("claude", clk.now(), time.Second)

	// One retirement per tick: 6 -> 5 -> 4 -> 3, then it holds at max_spare.
	for _, want := range []int{5, 4, 3, 3, 3} {
		ticks(a, clk, 1)
		if pool.idle != want {
			t.Fatalf("idle = %d, want %d (calls %v)", pool.idle, want, pool.maxCalls)
		}
	}

	// No request for idle_ttl: effective min_spare 0, drain to fully cold.
	clk.advance(11 * time.Second)
	for _, want := range []int{2, 1, 0} {
		ticks(a, clk, 1)
		if pool.idle != want {
			t.Fatalf("cold drain idle = %d, want %d", pool.idle, want)
		}
	}
	snap := snapshotFor(t, a, "claude")
	if !snap.Cold || snap.MinSpare != 0 {
		t.Fatalf("snapshot = %+v, want cold with min_spare 0", snap)
	}
}

func TestAutoscalerPredictiveTargetFollowsLittlesLaw(t *testing.T) {
	clk := newFakeClock()
	a := newTestScaler(t, clk, AutoscaleConfig{})
	pool := newScalePool(PoolConfig{MinSpare: 0, MaxSpare: 8, MaxWorkers: 8})
	register(t, a, PoolRegistration{Agent: "agy", Pool: pool})

	// Four arrivals per second, half a second of service each: lambda*W = 2.
	for i := 0; i < 12; i++ {
		for j := 0; j < 4; j++ {
			a.Observe("agy", clk.now(), 500*time.Millisecond)
		}
		ticks(a, clk, 1)
	}

	snap := snapshotFor(t, a, "agy")
	if snap.Lambda < 3.9 || snap.Lambda > 4.1 {
		t.Fatalf("lambda = %v, want about 4/s", snap.Lambda)
	}
	if snap.ServiceSeconds != 0.5 {
		t.Fatalf("W = %v, want 0.5s", snap.ServiceSeconds)
	}
	if pool.lastMin() != 2 {
		t.Fatalf("predictive min spare = %d, want ceil(lambda*W) = 2", pool.lastMin())
	}
	if pool.idle != 2 {
		t.Fatalf("idle = %d, want 2", pool.idle)
	}
}

func TestAutoscalerBandSparesGoToTheRankLeader(t *testing.T) {
	clk := newFakeClock()
	a := newTestScaler(t, clk, AutoscaleConfig{
		FallbackServiceTime: time.Second,
		// The router prefers the quota leader, which is NOT registration order.
		Ranker: RankerFunc(func(band int) []string {
			if band == 4 {
				return []string{"codex", "claude"}
			}
			return nil
		}),
	})
	claude := newScalePool(PoolConfig{MaxSpare: 4, MaxWorkers: 4})
	codex := newScalePool(PoolConfig{MaxSpare: 4, MaxWorkers: 4})
	register(t, a, PoolRegistration{Agent: "claude", Band: 4, Pool: claude})
	register(t, a, PoolRegistration{Agent: "codex", Band: 4, Pool: codex})

	for i := 0; i < 12; i++ {
		for j := 0; j < 3; j++ {
			a.ObserveBand(4)
		}
		ticks(a, clk, 1)
	}

	leader, follower := snapshotFor(t, a, "codex"), snapshotFor(t, a, "claude")
	if leader.BandShare != 2 || follower.BandShare != 1 {
		t.Fatalf("band shares: codex=%d claude=%d, want 2 and 1", leader.BandShare, follower.BandShare)
	}
	if codex.idle != 2 || claude.idle != 1 {
		t.Fatalf("warm workers: codex=%d claude=%d, want 2 and 1", codex.idle, claude.idle)
	}
	bands := a.Snapshot().Bands
	if len(bands) != 1 || bands[0].Band != 4 || bands[0].Prewarm != 3 || bands[0].Idle != 3 || bands[0].Agents != 2 {
		t.Fatalf("band totals = %+v", bands)
	}
}

func TestAutoscalerDoesNotPrewarmBelowTheReserveFloor(t *testing.T) {
	clk := newFakeClock()
	a := newTestScaler(t, clk, AutoscaleConfig{
		FallbackServiceTime: time.Second,
		Headroom: func(agent string) (float64, bool) {
			if agent == "claude" {
				return 0.05, true // below the 0.15 reserve floor
			}
			return 0, false // untracked quota is not a reason to hold back
		},
	})
	claude := newScalePool(PoolConfig{MinSpare: 1, MaxSpare: 4, MaxWorkers: 4})
	codex := newScalePool(PoolConfig{MaxSpare: 4, MaxWorkers: 4})
	register(t, a, PoolRegistration{Agent: "claude", Band: 4, Pool: claude})
	register(t, a, PoolRegistration{Agent: "codex", Band: 4, Pool: codex})

	for i := 0; i < 12; i++ {
		for j := 0; j < 3; j++ {
			a.ObserveBand(4)
		}
		ticks(a, clk, 1)
	}

	if claude.idle != 0 || claude.lastMin() != 0 {
		t.Fatalf("below-floor agent warmed: idle=%d last min spare=%d", claude.idle, claude.lastMin())
	}
	// The whole band demand lands on the agent that still has quota.
	if codex.idle != 3 {
		t.Fatalf("codex idle = %d, want the full band demand 3", codex.idle)
	}
	snap := snapshotFor(t, a, "claude")
	if !snap.HeadroomKnown || snap.Headroom != 0.05 {
		t.Fatalf("headroom snapshot = %+v", snap)
	}
}

func TestAutoscalerRespectsVendorConcurrencyCap(t *testing.T) {
	clk := newFakeClock()
	a := newTestScaler(t, clk, AutoscaleConfig{VendorMaxWorkers: map[string]int{"anthropic": 3}})
	first := newScalePool(PoolConfig{MinSpare: 2, MaxSpare: 2, MaxWorkers: 4})
	second := newScalePool(PoolConfig{MinSpare: 2, MaxSpare: 2, MaxWorkers: 4})
	register(t, a, PoolRegistration{Agent: "claude-sonnet", Provider: "anthropic", Pool: first})
	register(t, a, PoolRegistration{Agent: "claude-haiku", Provider: "anthropic", Pool: second})

	ticks(a, clk, 6)

	if total := first.idle + second.idle; total != 3 {
		t.Fatalf("vendor workers = %d (first=%d second=%d), want the cap 3", total, first.idle, second.idle)
	}
	if first.idle != 2 || second.idle != 1 {
		t.Fatalf("cap shared as first=%d second=%d, want 2 and 1", first.idle, second.idle)
	}
}

func TestAutoscalerRespectsHostWideCap(t *testing.T) {
	clk := newFakeClock()
	a := newTestScaler(t, clk, AutoscaleConfig{HostMaxWorkers: 2})
	first := newScalePool(PoolConfig{MinSpare: 4, MaxSpare: 4, MaxWorkers: 4})
	second := newScalePool(PoolConfig{MinSpare: 4, MaxSpare: 4, MaxWorkers: 4})
	register(t, a, PoolRegistration{Agent: "claude", Pool: first})
	register(t, a, PoolRegistration{Agent: "codex", Pool: second})

	ticks(a, clk, 6)

	snap := a.Snapshot()
	if snap.Workers != 2 || snap.HostMaxWorkers != 2 {
		t.Fatalf("snapshot workers = %d host cap = %d, want 2 and 2", snap.Workers, snap.HostMaxWorkers)
	}
	if snap.SchemaVersion != AutoscaleSchemaVersion {
		t.Fatalf("schema version = %q", snap.SchemaVersion)
	}
}

func TestAutoscalerDoesNotPrewarmAFailingTool(t *testing.T) {
	clk := newFakeClock()
	a := newTestScaler(t, clk, AutoscaleConfig{})
	pool := newScalePool(PoolConfig{MinSpare: 2, MaxSpare: 2, MaxWorkers: 4})
	pool.failSpawn = true
	register(t, a, PoolRegistration{Agent: "opencode", Pool: pool})

	ticks(a, clk, 3)
	if !snapshotFor(t, a, "opencode").BreakerOpen {
		t.Fatalf("breaker still closed after %d failed spawns", pool.failed)
	}
	if pool.lastMin() != 0 {
		t.Fatalf("min spare = %d after the breaker opened, want 0", pool.lastMin())
	}

	failed := pool.failed
	ticks(a, clk, 5)
	if pool.failed != failed {
		t.Fatalf("kept spawning a broken tool: failed %d -> %d", failed, pool.failed)
	}

	// One successful spawn closes the breaker and prewarming resumes.
	pool.failSpawn = false
	pool.spawned++
	ticks(a, clk, 2)
	if snapshotFor(t, a, "opencode").BreakerOpen {
		t.Fatalf("breaker did not close after a successful spawn")
	}
	if pool.lastMin() == 0 {
		t.Fatalf("prewarming did not resume: min spare targets %v", pool.minCalls)
	}
}

func TestSplitDemandFavorsTheLeaderAndSumsToTotal(t *testing.T) {
	for _, tc := range []struct {
		total, seats int
		want         []int
	}{
		{total: 3, seats: 2, want: []int{2, 1}},
		{total: 1, seats: 3, want: []int{1, 0, 0}},
		{total: 6, seats: 3, want: []int{3, 2, 1}},
		{total: 0, seats: 2, want: []int{0, 0}},
	} {
		got := splitDemand(tc.total, tc.seats)
		if !equalInts(got, tc.want) {
			t.Fatalf("splitDemand(%d, %d) = %v, want %v", tc.total, tc.seats, got, tc.want)
		}
	}
}

func TestHostWorkerCapUsesFreeMemoryWhenAvailable(t *testing.T) {
	gib := uint64(1) << 30
	if got := hostWorkerCap(8, func() (uint64, bool) { return gib, true }, 512<<20); got != 2 {
		t.Fatalf("cap with 1GiB free = %d, want 2", got)
	}
	if got := hostWorkerCap(8, func() (uint64, bool) { return 0, false }, 512<<20); got != 8 {
		t.Fatalf("cap with unknown memory = %d, want NumCPU 8", got)
	}
	if got := hostWorkerCap(0, nil, 0); got != 1 {
		t.Fatalf("cap floor = %d, want 1", got)
	}
	if DefaultHostMaxWorkers() < 1 {
		t.Fatalf("DefaultHostMaxWorkers() = %d", DefaultHostMaxWorkers())
	}
}

func TestAutoscalerRegisterValidatesAndUnregisterStops(t *testing.T) {
	clk := newFakeClock()
	a := newTestScaler(t, clk, AutoscaleConfig{})
	if err := a.Register(PoolRegistration{Pool: newScalePool(DefaultPoolConfig())}); err == nil {
		t.Fatal("Register without an agent name: want error")
	}
	if err := a.Register(PoolRegistration{Agent: "claude"}); err == nil {
		t.Fatal("Register without a pool: want error")
	}
	pool := newScalePool(PoolConfig{MinSpare: 1, MaxSpare: 1, MaxWorkers: 2})
	register(t, a, PoolRegistration{Agent: "claude", Pool: pool})
	ticks(a, clk, 2)
	a.Unregister("claude")
	calls := len(pool.minCalls)
	ticks(a, clk, 2)
	if len(pool.minCalls) != calls {
		t.Fatalf("unregistered pool still driven: %v", pool.minCalls)
	}
	if len(a.Snapshot().Pools) != 0 {
		t.Fatalf("snapshot still lists the unregistered pool")
	}
}

func TestAutoscalerDrivesARealPool(t *testing.T) {
	installFakeCatalog(t, "claude", WarmStdinStreamJSON, "warm")
	clk := newFakeClock()
	a := newTestScaler(t, clk, AutoscaleConfig{})
	pool := NewPool(context.Background(), "test-agent", PoolConfig{
		StartServers: 0, MinSpare: 1, MaxSpare: 2, MaxWorkers: 2,
	})
	defer pool.Close()
	register(t, a, PoolRegistration{Agent: "test-agent", Pool: pool})

	ticks(a, clk, 1)
	waitFor(t, 3*time.Second, func() bool { return pool.Stats().Idle == 1 })
	if snap := snapshotFor(t, a, "test-agent"); snap.MinSpare != 1 {
		t.Fatalf("real pool min spare = %d, want 1", snap.MinSpare)
	}
}
