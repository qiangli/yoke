package multierror

import "sync"

// Group runs functions concurrently and aggregates their errors,
// written for the buildah imagebuildah caller (S488.3d Linux engine
// graph): a zero-value Group, Go per task, then one Wait.
type Group struct {
	wg  sync.WaitGroup
	mu  sync.Mutex
	err *Error
}

// Go runs f in a new goroutine, recording a non-nil error it returns.
func (g *Group) Go(f func() error) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := f(); err != nil {
			g.mu.Lock()
			g.err = Append(g.err, err)
			g.mu.Unlock()
		}
	}()
}

// Wait blocks until every function started with Go has returned and
// yields their errors, or nil when none failed.
func (g *Group) Wait() *Error {
	g.wg.Wait()
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}
