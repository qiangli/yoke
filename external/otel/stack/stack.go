package stack

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// StackManager orchestrates all embedded observability components and the reverse proxy.
type StackManager struct {
	cfg     *Config
	dataDir string

	mu         sync.Mutex
	components []Component
	proxy      *ProxyServer
	otlpRoutes map[string]*url.URL
	started    bool
}

// NewStackManager creates a stack manager.
func NewStackManager(cfg *Config, dataDir string) *StackManager {
	return &StackManager{
		cfg:     cfg,
		dataDir: dataDir,
	}
}

// AddComponent registers a component to be managed by the stack.
// Components are started in the order they are added (dependency ordering).
// AddOTLPRoute registers an OTLP/HTTP ingest path on the proxy, pointing at the Victoria
// component that stores that signal.
//
// This is what replaced the OpenTelemetry Collector. OTLP/HTTP is defined as
// POST {endpoint}/v1/{traces,logs,metrics}, and every Victoria component ingests OTLP
// natively — so the collector's entire remaining job was to be one address that fans three
// signals out to three stores. The proxy already reverse-proxies by path prefix, so it IS
// the fan-out, and it costs nothing.
//
//	collector    833 transitive dependencies
//	this          3 map entries
//
// A middleman that only forwards is a dependency, not a feature.
func (s *StackManager) AddOTLPRoute(path string, backend *url.URL) {
	if s.otlpRoutes == nil {
		s.otlpRoutes = map[string]*url.URL{}
	}
	s.otlpRoutes[path] = backend
}

func (s *StackManager) AddComponent(c Component) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.components = append(s.components, c)
}

// Start launches all components (each in its own goroutine) and the reverse proxy.
func (s *StackManager) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return fmt.Errorf("stack already started")
	}

	slog.Info("observability: starting stack")

	// Assign path prefixes to components that need them before starting.
	for _, c := range s.components {
		if pc, ok := c.(PrefixConfigurable); ok {
			if prefix, exists := componentPathMap[c.Name()]; exists {
				pc.SetPathPrefix(strings.TrimSuffix(prefix, "/"))
			}
		}
	}

	// Start components in dependency order. Each Start() is non-blocking
	// (launches work in a goroutine internally).
	for _, c := range s.components {
		if err := c.Start(ctx); err != nil {
			slog.Warn("observability: component start failed", "component", c.Name(), "error", err)
			// Continue starting other components — non-fatal.
		}
	}

	// Start reverse proxy.
	s.proxy = NewProxyServer(s.cfg.proxyBindAddr(), s.cfg.proxyPort())
	s.registerRoutes()
	for path, backend := range s.otlpRoutes {
		s.proxy.AddOTLPRoute(path, backend)
	}
	if err := s.proxy.Start(ctx); err != nil {
		return fmt.Errorf("start proxy: %w", err)
	}

	s.started = true
	slog.Info("observability: stack started", "proxy", s.proxy.Addr())
	return nil
}

// Stop gracefully shuts down all components and the proxy (reverse order).
func (s *StackManager) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		return nil
	}

	slog.Info("observability: stopping stack")

	if s.proxy != nil {
		_ = s.proxy.Stop(ctx)
	}

	// Stop in reverse order.
	for i := len(s.components) - 1; i >= 0; i-- {
		c := s.components[i]
		if err := c.Stop(ctx); err != nil {
			slog.Warn("observability: component stop failed", "component", c.Name(), "error", err)
		}
	}

	s.started = false
	return nil
}

// Status returns the health status of each component.
func (s *StackManager) Status() []ComponentStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	var statuses []ComponentStatus
	for _, c := range s.components {
		statuses = append(statuses, ComponentStatus{
			Name:    c.Name(),
			Healthy: c.Healthy(),
		})
	}
	return statuses
}

// Healthy returns true if all components report healthy.
func (s *StackManager) Healthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, c := range s.components {
		if !c.Healthy() {
			return false
		}
	}
	return s.started
}

// ProxyAddr returns the proxy listen address (e.g. "127.0.0.1:31415").
func (s *StackManager) ProxyAddr() string {
	if s.proxy != nil {
		return s.proxy.Addr()
	}
	return ""
}

// AddRoute registers a reverse-proxy route on the proxy after the stack has started.
func (s *StackManager) AddRoute(pathPrefix string, backend *url.URL) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proxy != nil {
		s.proxy.AddRoute(pathPrefix, backend)
	}
}

// AddHandler registers an in-process HTTP handler on the proxy after the stack
// has started. Use for endpoints that don't need component lifecycle (e.g.
// the /.well-known manifest and /manifest endpoints), where a full Component
// shape would be ceremony for nothing.
func (s *StackManager) AddHandler(pathPrefix string, handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proxy != nil {
		s.proxy.AddHandler(pathPrefix, handler)
	}
}

// AddHandlerNoStrip registers an in-process HTTP handler that needs to see
// the full request path (no prefix strip). Required for handlers that
// self-dispatch on the URL — net/http/pprof is the canonical example.
func (s *StackManager) AddHandlerNoStrip(pathPrefix string, handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proxy != nil {
		s.proxy.AddHandlerNoStrip(pathPrefix, handler)
	}
}

// AddLateComponent registers and starts a component after the stack is already running.
// Its HTTP handler (if any) is mounted on the proxy.
func (s *StackManager) AddLateComponent(ctx context.Context, c Component) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.components = append(s.components, c)

	if err := c.Start(ctx); err != nil {
		return err
	}

	// Mount on proxy.
	if s.proxy != nil {
		path, ok := componentPathMap[c.Name()]
		if !ok {
			path = "/" + c.Name() + "/"
		}
		if handler := c.HTTPHandler(); handler != nil {
			s.proxy.AddHandler(path, handler)
		} else {
			type portProvider interface{ Port() int }
			if pp, ok := c.(portProvider); ok && pp.Port() > 0 {
				backend, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", pp.Port()))
				s.proxy.AddRoute(path, backend)
			}
		}
	}

	return nil
}

// componentPathMap defines the proxy path prefix for each well-known component.
var componentPathMap = map[string]string{
	"otel-collector":   "/collector/",
	"prometheus":       "/prometheus/",
	"alertmanager":     "/alerts/",
	"perses":           "/dashboard/",
	"victoria-logs":    "/logs/",
	"victoria-traces":  "/traces/",
	"victoria-metrics": "/metrics/",
	"jaeger":           "/traces/",
}

// registerRoutes mounts each component's HTTP handler on the proxy mux.
func (s *StackManager) registerRoutes() {
	for _, c := range s.components {
		path, ok := componentPathMap[c.Name()]
		if !ok {
			path = "/" + c.Name() + "/"
		}

		type portProvider interface{ Port() int }

		// Components with their own HTTP handler get mounted in-process.
		if handler := c.HTTPHandler(); handler != nil {
			s.proxy.AddHandler(path, handler)
		} else if pp, ok := c.(portProvider); ok && pp.Port() > 0 {
			// Components running as external processes with a port get reverse-proxied.
			backend, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", pp.Port()))
			s.proxy.AddRoute(path, backend)
		}

		// Jaeger uses QueryPort for UI.
		type queryPortProvider interface{ QueryPort() int }
		if qp, ok := c.(queryPortProvider); ok && qp.QueryPort() > 0 {
			backend, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", qp.QueryPort()))
			s.proxy.AddRoute(path, backend)
		}

	}
}
