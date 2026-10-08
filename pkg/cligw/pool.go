package cligw

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrPoolClosed is returned to queued acquisitions when a pool is closed.
var ErrPoolClosed = errors.New("cligw: pool is closed")

// PoolConfig controls one agent's one-shot worker pool.
type PoolConfig struct {
	StartServers int
	MinSpare     int
	MaxSpare     int
	MaxWorkers   int
	IdleTTL      time.Duration
}

// DefaultPoolConfig returns the prefork defaults from the cligw design.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		StartServers: 1,
		MinSpare:     1,
		MaxSpare:     3,
		MaxWorkers:   4,
		IdleTTL:      10 * time.Minute,
	}
}

// PoolStats is a point-in-time pool snapshot. Spawned and Failed are lifetime
// counters; Idle, Busy and Queued are current gauges.
type PoolStats struct {
	Idle    int   `json:"idle"`
	Busy    int   `json:"busy"`
	Queued  int   `json:"queued"`
	Spawned int64 `json:"spawned"`
	Failed  int64 `json:"failed"`
}

type idleWorker struct {
	worker *Worker
	since  time.Time
}

type acquireResult struct {
	worker *Worker
	err    error
}

type poolWaiter struct {
	ctx context.Context
	ch  chan acquireResult
}

// Pool is a FIFO, MaxWorkers-bounded one-shot worker pool for one fleet agent.
type Pool struct {
	mu sync.Mutex

	agent string
	cfg   PoolConfig
	ctx   context.Context
	stop  context.CancelFunc

	idle     []idleWorker
	busy     map[*Worker]struct{}
	total    int // idle + busy + spawning reservations
	spawning int
	waiters  []*poolWaiter

	spawned int64
	failed  int64

	failStreak uint
	nextSpawn  time.Time
	closed     bool
	closeDone  chan struct{} // closed once the first Close has retired everything
	wg         sync.WaitGroup
}

// NewPool creates an agent pool and starts StartServers asynchronously. A zero
// StartServers is valid; MaxWorkers defaults to four when non-positive.
func NewPool(ctx context.Context, agent string, cfg PoolConfig) *Pool {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.MaxWorkers <= 0 {
		cfg.MaxWorkers = 4
	}
	if cfg.StartServers < 0 {
		cfg.StartServers = 0
	}
	if cfg.MinSpare < 0 {
		cfg.MinSpare = 0
	}
	if cfg.MaxSpare < 0 {
		cfg.MaxSpare = 0
	}
	if cfg.MinSpare > cfg.MaxWorkers {
		cfg.MinSpare = cfg.MaxWorkers
	}
	if cfg.MaxSpare < cfg.MinSpare {
		cfg.MaxSpare = cfg.MinSpare
	}
	if cfg.MaxSpare > cfg.MaxWorkers {
		cfg.MaxSpare = cfg.MaxWorkers
	}
	if cfg.StartServers > cfg.MaxWorkers {
		cfg.StartServers = cfg.MaxWorkers
	}
	pctx, cancel := context.WithCancel(ctx)
	p := &Pool{agent: agent, cfg: cfg, ctx: pctx, stop: cancel, busy: make(map[*Worker]struct{}), closeDone: make(chan struct{})}

	p.mu.Lock()
	for i := 0; i < cfg.StartServers; i++ {
		p.reserveSpawnLocked()
	}
	p.ensureLocked()
	p.mu.Unlock()

	if cfg.IdleTTL > 0 {
		p.wg.Add(1)
		go p.idleLoop()
	}
	go func() {
		<-pctx.Done()
		p.Close()
	}()
	return p
}

// Acquire takes the oldest idle worker or joins the FIFO wait queue. Both
// queued waits and spawn backoff honor ctx.
func (p *Pool) Acquire(ctx context.Context) (*Worker, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.Lock()
	retired := p.retireLocked(time.Now())
	if p.closed {
		p.mu.Unlock()
		closeWorkers(retired)
		return nil, ErrPoolClosed
	}
	if len(p.idle) > 0 {
		entry := p.idle[0]
		p.idle = p.idle[1:]
		p.busy[entry.worker] = struct{}{}
		p.mu.Unlock()
		closeWorkers(retired)
		return entry.worker, nil
	}
	waiter := &poolWaiter{ctx: ctx, ch: make(chan acquireResult, 1)}
	p.waiters = append(p.waiters, waiter)
	p.ensureLocked()
	p.mu.Unlock()
	closeWorkers(retired)

	select {
	case got := <-waiter.ch:
		return got.worker, got.err
	case <-ctx.Done():
		p.mu.Lock()
		removed := p.removeWaiterLocked(waiter)
		p.ensureLocked()
		p.mu.Unlock()
		if !removed {
			// Delivery won the queue race. Retire the now-unwanted one-shot so
			// cancellation cannot leak a busy slot.
			select {
			case got := <-waiter.ch:
				if got.worker != nil {
					p.Release(got.worker)
				}
			default:
			}
		}
		return nil, ctx.Err()
	}
}

// Release retires an acquired one-shot worker and asynchronously restores the
// FIFO queue and MinSpare target.
func (p *Pool) Release(worker *Worker) {
	if worker == nil {
		return
	}
	_ = worker.Close()
	p.mu.Lock()
	if _, ok := p.busy[worker]; ok {
		delete(p.busy, worker)
		p.total--
	}
	p.ensureLocked()
	p.mu.Unlock()
}

// Stats returns current gauges and lifetime spawn counters.
func (p *Pool) Stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	queued := 0
	for _, waiter := range p.waiters {
		if waiter.ctx.Err() == nil {
			queued++
		}
	}
	return PoolStats{
		Idle: len(p.idle), Busy: len(p.busy), Queued: queued,
		Spawned: p.spawned, Failed: p.failed,
	}
}

// Total returns idle plus busy workers plus spawning reservations: every
// process this pool owns or is about to own. Sticky sessions are counted
// separately by the server against the same ceiling.
func (p *Pool) Total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.total
}

// Config returns the pool's configuration with the live MinSpare/MaxSpare
// targets, so the autoscaler can read the ceilings it must not exceed.
func (p *Pool) Config() PoolConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg
}

// SetMinSpare changes the target driven by the later autoscaler.
func (p *Pool) SetMinSpare(n int) {
	if n < 0 {
		n = 0
	}
	p.mu.Lock()
	if n > p.cfg.MaxWorkers {
		n = p.cfg.MaxWorkers
	}
	p.cfg.MinSpare = n
	if p.cfg.MaxSpare < n {
		p.cfg.MaxSpare = n
	}
	p.ensureLocked()
	p.mu.Unlock()
}

// SetMaxSpare changes the idle ceiling driven by the later autoscaler.
func (p *Pool) SetMaxSpare(n int) {
	if n < 0 {
		n = 0
	}
	p.mu.Lock()
	if n > p.cfg.MaxWorkers {
		n = p.cfg.MaxWorkers
	}
	p.cfg.MaxSpare = n
	if p.cfg.MinSpare > n {
		p.cfg.MinSpare = n
	}
	retired := p.retireLocked(time.Now())
	p.ensureLocked()
	p.mu.Unlock()
	closeWorkers(retired)
}

// Close kills idle workers, cancels spawn backoff, and wakes queued callers.
// Every caller returns only after the first Close has retired every worker
// and its directory: a cancelled parent context starts Close from the pool's
// watcher, and the door exits as soon as its own Close returns.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		<-p.closeDone
		return nil
	}
	defer close(p.closeDone)
	p.closed = true
	p.stop()
	idle := p.idle
	p.idle = nil
	p.total -= len(idle)
	busy := make([]*Worker, 0, len(p.busy))
	for worker := range p.busy {
		busy = append(busy, worker)
	}
	p.total -= len(busy)
	p.busy = make(map[*Worker]struct{})
	waiters := p.waiters
	p.waiters = nil
	p.mu.Unlock()

	for _, entry := range idle {
		_ = entry.worker.Close()
	}
	closeWorkers(busy)
	for _, waiter := range waiters {
		waiter.ch <- acquireResult{err: ErrPoolClosed}
	}
	p.wg.Wait()
	return nil
}

func (p *Pool) ensureLocked() {
	if p.closed {
		return
	}
	p.pruneWaitersLocked()
	queued := len(p.waiters)
	need := queued + p.cfg.MinSpare - len(p.idle) - p.spawning
	capacity := p.cfg.MaxWorkers - p.total
	if need > capacity {
		need = capacity
	}
	for i := 0; i < need; i++ {
		p.reserveSpawnLocked()
	}
}

func (p *Pool) reserveSpawnLocked() {
	if p.closed || p.total >= p.cfg.MaxWorkers {
		return
	}
	p.total++
	p.spawning++
	p.wg.Add(1)
	go p.spawnOne()
}

func (p *Pool) spawnOne() {
	defer p.wg.Done()
	if err := p.waitForSpawnWindow(); err != nil {
		p.spawnFinished(nil, err)
		return
	}
	worker, err := NewWorker(p.ctx, p.agent)
	p.spawnFinished(worker, err)
}

func (p *Pool) waitForSpawnWindow() error {
	p.mu.Lock()
	wait := time.Until(p.nextSpawn)
	p.mu.Unlock()
	if wait <= 0 {
		return p.ctx.Err()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return p.ctx.Err()
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
}

func (p *Pool) spawnFinished(worker *Worker, err error) {
	p.mu.Lock()
	p.spawning--
	if err != nil {
		p.total--
		if !errors.Is(err, context.Canceled) || !p.closed {
			p.failed++
			p.failStreak++
			delay := 100 * time.Millisecond
			for i := uint(1); i < p.failStreak && delay < 30*time.Second; i++ {
				delay *= 2
			}
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
			p.nextSpawn = time.Now().Add(delay)
		}
		p.ensureLocked()
		p.mu.Unlock()
		return
	}
	p.failStreak = 0
	p.nextSpawn = time.Time{}
	p.spawned++
	if p.closed {
		p.total--
		p.mu.Unlock()
		_ = worker.Close()
		return
	}
	if waiter := p.popWaiterLocked(); waiter != nil {
		p.busy[worker] = struct{}{}
		waiter.ch <- acquireResult{worker: worker}
		p.mu.Unlock()
		return
	}
	if len(p.idle) >= p.cfg.MaxSpare {
		p.total--
		p.mu.Unlock()
		_ = worker.Close()
		return
	}
	p.idle = append(p.idle, idleWorker{worker: worker, since: time.Now()})
	p.ensureLocked()
	p.mu.Unlock()
}

func (p *Pool) idleLoop() {
	defer p.wg.Done()
	interval := p.cfg.IdleTTL / 4
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	if interval > time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			p.mu.Lock()
			retired := p.retireLocked(now)
			p.ensureLocked()
			p.mu.Unlock()
			closeWorkers(retired)
		case <-p.ctx.Done():
			return
		}
	}
}

func (p *Pool) retireLocked(now time.Time) []*Worker {
	if len(p.idle) == 0 {
		return nil
	}
	keepFloor := p.cfg.MinSpare
	var retired []*Worker
	for len(p.idle) > p.cfg.MaxSpare {
		retired = append(retired, p.idle[0].worker)
		p.idle = p.idle[1:]
		p.total--
	}
	if p.cfg.IdleTTL <= 0 {
		return retired
	}
	for len(p.idle) > keepFloor && now.Sub(p.idle[0].since) >= p.cfg.IdleTTL {
		retired = append(retired, p.idle[0].worker)
		p.idle = p.idle[1:]
		p.total--
	}
	return retired
}

func (p *Pool) popWaiterLocked() *poolWaiter {
	for len(p.waiters) > 0 {
		waiter := p.waiters[0]
		p.waiters = p.waiters[1:]
		if waiter.ctx.Err() == nil {
			return waiter
		}
	}
	return nil
}

func (p *Pool) pruneWaitersLocked() {
	out := p.waiters[:0]
	for _, waiter := range p.waiters {
		if waiter.ctx.Err() == nil {
			out = append(out, waiter)
		}
	}
	p.waiters = out
}

func (p *Pool) removeWaiterLocked(target *poolWaiter) bool {
	for i, waiter := range p.waiters {
		if waiter == target {
			copy(p.waiters[i:], p.waiters[i+1:])
			p.waiters = p.waiters[:len(p.waiters)-1]
			return true
		}
	}
	return false
}

func closeWorkers(workers []*Worker) {
	for _, worker := range workers {
		_ = worker.Close()
	}
}
