package dbstore

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Guard makes a handle unusable once it is closed. A memory-mode handle
// shares its engine with every other handle in the owner, so without it a
// closed handle could keep reading and writing after Release — past a later
// exclusive Acquire. Every operation brackets itself with Enter/Exit; Close
// waits for operations in flight, then runs release exactly once. An
// operation must not Close its own handle.
type Guard struct {
	mu     sync.RWMutex
	closed bool
}

// Enter admits one operation, or fails with ErrLifetimeEnded after Close.
func (g *Guard) Enter(id Identity) error {
	g.mu.RLock()
	if g.closed {
		g.mu.RUnlock()
		return fmt.Errorf("%w: handle for %s is closed", ErrLifetimeEnded, id.ID)
	}
	return nil
}

// Exit ends an operation admitted by Enter.
func (g *Guard) Exit() { g.mu.RUnlock() }

// Closed reports whether Close has run.
func (g *Guard) Closed() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.closed
}

// Close marks the handle closed and runs release once; later calls are no-ops.
func (g *Guard) Close(release func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	g.closed = true
	if release == nil {
		return nil
	}
	return release()
}

// Gate is a writer-admission slot whose wait observes ctx and is bounded,
// unlike a sync.Mutex or an engine's internal writer lock. Adapters enter it
// before the engine's own writer lock, so that lock is never contended by
// this handle's writers and the only wait left is the bounded one here.
type Gate chan struct{}

// NewGate returns a gate that admits one writer at a time.
func NewGate() Gate { return make(Gate, 1) }

// Enter waits at most wait for the slot. It fails with ErrBusy when the wait
// elapses and with timeout/cancelled when ctx ends first. The returned func
// releases the slot.
func (g Gate) Enter(ctx context.Context, wait time.Duration) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, FromContext(err)
	}
	select {
	case g <- struct{}{}:
		return func() { <-g }, nil
	default:
	}
	if wait <= 0 {
		wait = DefaultWait
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case g <- struct{}{}:
		return func() { <-g }, nil
	case <-ctx.Done():
		return nil, FromContext(ctx.Err())
	case <-t.C:
		return nil, fmt.Errorf("%w: writer admission not granted within %s", ErrBusy, wait)
	}
}
