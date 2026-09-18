package meet

import (
	"context"
	"net/http"
)

// MountOptions configures the room when a host other than `meet serve` mounts it.
type MountOptions struct {
	// Gate wraps every room-shaped route. Nil means the default two-entrance
	// rule: ungated on direct loopback, cloud-vouched otherwise, 403 for anyone
	// else.
	//
	// An embedder that has ALREADY authenticated the caller passes a
	// pass-through. It must, because mounting the room under a path prefix
	// requires stamping X-Forwarded-Prefix so the SPA renders the right
	// <base href> — and ArrivedViaCloud is DEFINED as that header being present,
	// so the default gate would read a freshly-mounted loopback request as
	// "arrived via cloud" and 403 the machine owner on their own machine.
	Gate func(http.Handler) http.Handler
}

// Handler is the web room as an http.Handler: the embedded SPA at /, the JSON
// API under /api/, and the /observe WebSocket — the same mux `meet serve`
// listens with.
//
// It exists so a host other than the cobra command can serve the room. The
// first such host is the browser e2e harness (web/e2e), which must serve THIS
// checkout's SPA rather than whatever binary happens to be installed —
// otherwise the tests would silently pass against someone else's build. An
// embedder (outpost, a desktop shell) is the same shape.
//
// The SPA is only present when built with -tags meetspa; without it the
// handler still serves the API and explains the missing UI at /.
func Handler(ctx context.Context) http.Handler {
	return HandlerWith(ctx, MountOptions{})
}

// HandlerWith is Handler with the embedder's options. See MountOptions.
func HandlerWith(ctx context.Context, opts MountOptions) http.Handler {
	return newServeHandler(ctx, opts)
}
