// Package stack provides an embedded OpenTelemetry backend stack.
package stack

import (
	"context"
	"net/http"
)

// Component is the interface that all embedded observability components implement.
// Each component runs its work in a goroutine — Start must be non-blocking.
type Component interface {
	// Name returns the component's human-readable name.
	Name() string

	// Start launches the component in a background goroutine.
	// It must return promptly; long-running work goes in the goroutine.
	Start(ctx context.Context) error

	// Stop gracefully shuts down the component.
	Stop(ctx context.Context) error

	// Healthy returns true if the component is operational.
	Healthy() bool

	// HTTPHandler returns an http.Handler to be mounted on the reverse proxy.
	// Return nil if the component has no HTTP interface.
	HTTPHandler() http.Handler
}

// ComponentStatus describes the runtime status of a stack component.
type ComponentStatus struct {
	Name      string `json:"name"`
	ProxyPath string `json:"proxy_path,omitempty"`
	Healthy   bool   `json:"healthy"`
}

// PrefixConfigurable is implemented by components that need to know
// their reverse-proxy path prefix (e.g. "/traces") before starting.
// The prefix is set by StackManager before Start() is called.
type PrefixConfigurable interface {
	SetPathPrefix(prefix string)
}
