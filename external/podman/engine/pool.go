package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// Pool manages a set of pre-warmed containers for fast agent startup.
// Containers are created ahead of time and claimed by agents on demand,
// reducing cold-start latency.
type Pool struct {
	engine   *Engine
	template *ContainerConfig
	size     int

	mu        sync.Mutex
	available []*Container
	inUse     map[string]*Container // container ID → container
}

// NewPool creates a container pool with the given capacity.
func NewPool(engine *Engine, template *ContainerConfig, size int) *Pool {
	return &Pool{
		engine:   engine,
		template: template,
		size:     size,
		inUse:    make(map[string]*Container),
	}
}

// Warm pre-creates containers to fill the pool.
func (p *Pool) Warm(ctx context.Context) error {
	p.mu.Lock()
	needed := p.size - len(p.available)
	p.mu.Unlock()

	slog.Info("container: warming pool", "target", p.size, "needed", needed)

	for i := 0; i < needed; i++ {
		cfg := *p.template // copy
		cfg.Name = fmt.Sprintf("%s-pool-%d", cfg.Name, i)

		ctr, err := p.engine.CreateContainer(ctx, &cfg)
		if err != nil {
			return fmt.Errorf("warm pool container %d: %w", i, err)
		}

		if err := ctr.Start(ctx); err != nil {
			ctr.Remove(ctx, true)
			return fmt.Errorf("start pool container %d: %w", i, err)
		}

		p.mu.Lock()
		p.available = append(p.available, ctr)
		p.mu.Unlock()
	}

	slog.Info("container: pool warmed", "size", p.size)
	return nil
}

// Acquire claims a pre-warmed container from the pool.
// Returns an error if no containers are available.
func (p *Pool) Acquire(ctx context.Context) (*Container, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.available) == 0 {
		return nil, fmt.Errorf("no containers available in pool")
	}

	ctr := p.available[len(p.available)-1]
	p.available = p.available[:len(p.available)-1]
	p.inUse[ctr.ID] = ctr

	// Replenish in background.
	go p.replenishOne(ctx)

	return ctr, nil
}

// Release returns a container to the pool (or removes it if pool is full).
func (p *Pool) Release(ctr *Container) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.inUse, ctr.ID)

	if len(p.available) < p.size {
		p.available = append(p.available, ctr)
	} else {
		// Pool is full, remove the container.
		go ctr.Remove(context.Background(), true)
	}
}

// Available returns the number of containers available in the pool.
func (p *Pool) Available() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.available)
}

// Size returns the target pool capacity.
func (p *Pool) Size() int {
	return p.size
}

// Close drains all containers from the pool.
func (p *Pool) Close(ctx context.Context) {
	p.mu.Lock()
	available := p.available
	inUse := make([]*Container, 0, len(p.inUse))
	for _, c := range p.inUse {
		inUse = append(inUse, c)
	}
	p.available = nil
	p.inUse = make(map[string]*Container)
	p.mu.Unlock()

	for _, ctr := range available {
		ctr.Remove(ctx, true)
	}
	for _, ctr := range inUse {
		ctr.Remove(ctx, true)
	}
}

// replenishOne creates a single container to replace one taken from the pool.
func (p *Pool) replenishOne(ctx context.Context) {
	p.mu.Lock()
	if len(p.available) >= p.size {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	cfg := *p.template
	cfg.Name = fmt.Sprintf("%s-pool-r", cfg.Name)

	ctr, err := p.engine.CreateContainer(ctx, &cfg)
	if err != nil {
		slog.Warn("container: pool replenish failed", "error", err)
		return
	}

	if err := ctr.Start(ctx); err != nil {
		ctr.Remove(ctx, true)
		slog.Warn("container: pool replenish start failed", "error", err)
		return
	}

	p.mu.Lock()
	p.available = append(p.available, ctr)
	p.mu.Unlock()
}
