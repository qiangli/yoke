package chat

import (
	"context"
	"sync"
)

// acpLifetime owns host callbacks and prompts, not the SDK reader. SDK Done
// can close while queued notifications are still being dispatched. Admissions
// are serialized with shutdown so no callback can write after wait returns.
type acpLifetime struct {
	once    sync.Once
	mu      sync.Mutex
	closed  bool
	ctx     context.Context
	cancel  context.CancelFunc
	workers sync.WaitGroup
}

func (l *acpLifetime) init(parent context.Context) {
	l.once.Do(func() {
		if parent == nil {
			parent = context.Background()
		}
		l.ctx, l.cancel = context.WithCancel(parent)
	})
}

func (l *acpLifetime) enter(parent context.Context) (context.Context, func()) {
	l.init(parent)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, nil
	}
	l.workers.Add(1)
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(l.ctx, cancel)
	return ctx, func() { stop(); cancel(); l.workers.Done() }
}

func (l *acpLifetime) stop() {
	l.init(context.Background())
	l.mu.Lock()
	l.closed = true
	l.cancel()
	l.mu.Unlock()
}

func (l *acpLifetime) wait() { l.workers.Wait() }
