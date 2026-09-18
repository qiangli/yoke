// Package browser provides the per-session Client surface used by
// the browser_* tool shims. The Client is injected via context (no
// package-level globals) and dispatches wire.Actions to whichever
// backend is configured (today: in-process mcpservers.Manager;
// future: out-of-process daemon).
package browser

import (
	"context"

	"github.com/qiangli/yoke/pkg/browser/wire"
)

// Client is the transport-shaped browser dispatcher. Implementations
// live behind a build tag (stable: none; experimental: inproc adapter
// around mcpservers.Manager). The wire types are build-tag-free, so
// the interface signature compiles in both builds.
type Client interface {
	Execute(ctx context.Context, action wire.Action) (*wire.Result, error)
	Close() error
}

// clientKey is the private context-key type. Mirrors permApprovedKey
// (internal/tools/registry.go), toolEventWriterKey
// (internal/tools/middleware.go), and delegationKey
// (internal/runtime/conversation/delegation.go).
type clientKey struct{}

// WithClient returns a new context carrying c. Pass nil to clear.
func WithClient(ctx context.Context, c Client) context.Context {
	return context.WithValue(ctx, clientKey{}, c)
}

// ClientFromContext returns the Client installed on ctx, if any.
// The second return value reports whether a non-nil client was
// present. Tool handlers use this to either dispatch (client != nil)
// or return the friendly "configure browser.mode" message.
func ClientFromContext(ctx context.Context) (Client, bool) {
	c, ok := ctx.Value(clientKey{}).(Client)
	if !ok || c == nil {
		return nil, false
	}
	return c, true
}
