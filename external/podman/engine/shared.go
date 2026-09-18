package engine

import (
	"context"
	"sync"
)

// sharedEngine is the process-wide lazy engine used by callers that don't
// own an Engine lifecycle (sandbox shell builtin, sandbox_exec MCP tool).
// First call provisions or connects; subsequent calls reuse the same
// connection. Callers that need lifecycle control (e.g. ycode serve)
// should construct their own Engine and not touch this.
var (
	sharedMu     sync.Mutex
	sharedEng    *Engine
	sharedEngErr error
)

// SharedEngine returns the package-wide Engine, creating it on first use.
// The first call may take minutes on macOS/Windows when the machine VM
// has to be provisioned. Once cached, the engine survives the lifetime of
// the process. Errors are sticky for the call that hit them but not
// memoized — a subsequent call will retry, since the failure may have
// been transient (e.g. socket race during machine boot).
func SharedEngine(ctx context.Context) (*Engine, error) {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedEng != nil && sharedEng.Healthy() {
		return sharedEng, nil
	}
	eng, err := NewEngine(ctx, &EngineConfig{})
	if err != nil {
		sharedEngErr = err
		return nil, err
	}
	sharedEng = eng
	sharedEngErr = nil
	return eng, nil
}
