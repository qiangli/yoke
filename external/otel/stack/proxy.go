package stack

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ProxyServer routes all user-facing OTEL services through one loopback port.
type ProxyServer struct {
	listenAddr  string
	mu          sync.RWMutex
	routes      map[string]*url.URL
	otlpRoutes  map[string]*url.URL
	handlers    map[string]http.Handler
	rawHandlers map[string]http.Handler
	server      *http.Server
	mux         *http.ServeMux
}

func NewProxyServer(bindAddr string, port int) *ProxyServer {
	return &ProxyServer{
		listenAddr:  fmt.Sprintf("%s:%d", bindAddr, port),
		routes:      make(map[string]*url.URL),
		otlpRoutes:  make(map[string]*url.URL),
		handlers:    make(map[string]http.Handler),
		rawHandlers: make(map[string]http.Handler),
	}
}

func (p *ProxyServer) AddRoute(pathPrefix string, backend *url.URL) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.routes[pathPrefix] = backend
}

// AddOTLPRoute registers an exact-path reverse proxy. Unlike query routes, an
// OTLP request must be sent to the backend URL as-is rather than joined with
// the incoming /v1/{signal} path.
func (p *ProxyServer) AddOTLPRoute(path string, backend *url.URL) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.otlpRoutes[path] = backend
}

func (p *ProxyServer) AddHandler(pathPrefix string, handler http.Handler) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handlers[pathPrefix] = handler
	if p.mux != nil {
		p.mux.Handle(pathPrefix, http.StripPrefix(strings.TrimSuffix(pathPrefix, "/"), handler))
	}
}

func (p *ProxyServer) AddHandlerNoStrip(pathPrefix string, handler http.Handler) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rawHandlers[pathPrefix] = handler
	if p.mux != nil {
		p.mux.Handle(pathPrefix, handler)
	}
}

func (p *ProxyServer) Start(_ context.Context) error {
	_, portStr, _ := net.SplitHostPort(p.listenAddr)
	if portStr != "" {
		port, _ := strconv.Atoi(portStr)
		if port > 0 && !IsPortAvailable(port) {
			return fmt.Errorf("proxy: port %d already in use", port)
		}
	}

	mux := http.NewServeMux()
	p.mu.RLock()
	for _, prefix := range sortedURLPrefixes(p.routes) {
		mux.Handle(prefix, p.reverseProxy(p.routes[prefix]))
	}
	for _, path := range sortedURLPrefixes(p.otlpRoutes) {
		mux.Handle(path, p.otlpReverseProxy(p.otlpRoutes[path]))
	}
	for _, prefix := range sortedHandlerPrefixes(p.handlers) {
		mux.Handle(prefix, http.StripPrefix(strings.TrimSuffix(prefix, "/"), p.handlers[prefix]))
	}
	for _, prefix := range sortedHandlerPrefixes(p.rawHandlers) {
		mux.Handle(prefix, p.rawHandlers[prefix])
	}
	p.mu.RUnlock()

	mux.HandleFunc("/healthz", p.healthz)
	mux.HandleFunc("/", p.landingPage)

	p.mux = mux
	p.server = &http.Server{Addr: p.listenAddr, Handler: mux}

	slog.Info("otel proxy: starting", "addr", p.listenAddr)
	go func() {
		if err := p.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("otel proxy: listen failed", "error", err)
		}
	}()
	return nil
}

func (p *ProxyServer) Stop(ctx context.Context) error {
	if p.server == nil {
		return nil
	}
	return p.server.Shutdown(ctx)
}

func (p *ProxyServer) Addr() string { return p.listenAddr }

func (p *ProxyServer) reverseProxy(target *url.URL) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(target)
	orig := proxy.Director
	proxy.Director = func(req *http.Request) {
		orig(req)
		req.Host = target.Host
	}
	return proxy
}

func (p *ProxyServer) otlpReverseProxy(target *url.URL) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(target)
	orig := proxy.Director
	proxy.Director = func(req *http.Request) {
		orig(req)
		req.URL.Path = target.Path
		req.URL.RawPath = target.RawPath
		req.Host = target.Host
	}
	return proxy
}

func (p *ProxyServer) landingPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><title>OTEL</title>
<style>body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:0;background:#111827;color:#e5e7eb}main{max-width:920px;margin:0 auto;padding:40px 24px}h1{font-size:28px;margin:0 0 8px}.muted{color:#9ca3af;margin-bottom:28px}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:12px}.card{display:block;text-decoration:none;color:inherit;border:1px solid #374151;background:#1f2937;border-radius:8px;padding:16px}.card:hover{border-color:#60a5fa}.path{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;color:#93c5fd;font-size:13px}.kind{font-size:12px;color:#9ca3af;margin-top:8px}</style></head><body><main><h1>OTEL</h1><div class="muted">Unified traces, metrics, logs, dashboards, and alerts.</div><div class="grid">`)
	for _, route := range p.routeList() {
		fmt.Fprintf(w, `<a class="card" href="%s"><div>%s</div><div class="path">%s</div><div class="kind">%s</div></a>`, route.Path, route.Name, route.Path, route.Kind)
	}
	fmt.Fprint(w, `</div></main></body></html>`)
}

func (p *ProxyServer) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "ok",
		"routes": p.routeList(),
	})
}

type routeInfo struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

func (p *ProxyServer) routeList() []routeInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]routeInfo, 0, len(p.routes)+len(p.otlpRoutes)+len(p.handlers)+len(p.rawHandlers))
	for path := range p.routes {
		out = append(out, routeInfo{Path: path, Name: displayName(path), Kind: "proxy"})
	}
	for path := range p.otlpRoutes {
		out = append(out, routeInfo{Path: path, Name: displayName(path), Kind: "proxy"})
	}
	for path := range p.handlers {
		out = append(out, routeInfo{Path: path, Name: displayName(path), Kind: "handler"})
	}
	for path := range p.rawHandlers {
		out = append(out, routeInfo{Path: path, Name: displayName(path), Kind: "handler"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func sortedURLPrefixes(m map[string]*url.URL) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

func sortedHandlerPrefixes(m map[string]http.Handler) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

func displayName(path string) string {
	s := strings.Trim(path, "/")
	if s == "" {
		return "Home"
	}
	s = strings.ReplaceAll(s, "-", " ")
	return strings.Title(s)
}
