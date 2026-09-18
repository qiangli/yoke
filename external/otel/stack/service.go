package stack

import (
	"fmt"
	"net/url"
	"path/filepath"
)

type Service struct {
	Manager       *StackManager
	CollectorAddr string
	HTTPAddr      string
}

func NewService(cfg *Config, dataDir string) (*Service, error) {
	if cfg == nil {
		cfg = &Config{}
	}
	mgr := NewStackManager(cfg, dataDir)

	allocate := AllocatePort
	vlogsPort, err := allocate()
	if err != nil {
		return nil, fmt.Errorf("victorialogs port: %w", err)
	}
	mgr.AddComponent(NewVictoriaLogsComponent(vlogsPort, filepath.Join(dataDir, "vlogs")))

	// JAEGER IS GONE. VictoriaTraces stores the traces and serves its own vmui.
	//
	//	jaeger        2,240 transitive dependencies
	//	victoria      ~113  (the same logstorage engine as VictoriaLogs)
	//
	// Twenty times the weight, for the same job.
	vtracesPort, err := allocate()
	if err != nil {
		return nil, fmt.Errorf("victoria-traces port: %w", err)
	}
	mgr.AddComponent(NewVictoriaTracesComponent(vtracesPort, filepath.Join(dataDir, "vtraces")))

	// METRICS: VictoriaMetrics, ingesting OTLP natively.
	vmetricsPort, err := allocate()
	if err != nil {
		return nil, fmt.Errorf("victoria-metrics port: %w", err)
	}
	mgr.AddComponent(NewVictoriaMetricsComponent(vmetricsPort, filepath.Join(dataDir, "vmetrics")))

	// THE OTEL COLLECTOR IS GONE, AND SO IS PROMETHEUS.
	//
	// The chain is why neither could be deleted alone: the collector received OTLP and
	// handed metrics to a prometheusexporter, and Prometheus SCRAPED that exporter. So
	// deleting the collector deleted the only path metrics had. Both fall together, or
	// neither does.
	//
	//	jaeger        2,240  -> VictoriaTraces
	//	perses        1,478  -> vmui (already in the binary)
	//	collector       833  -> nothing. See below.
	//	prometheus      556  -> VictoriaMetrics
	//	victoria       ~113  <- the engine that stores all three signals
	//
	// EVERY VICTORIA COMPONENT INGESTS OTLP NATIVELY. So the collector's entire remaining
	// job was to be ONE ADDRESS that accepts all three signals and fans them out — 833
	// dependencies to spare a caller from setting three environment variables.
	//
	// The proxy already reverse-proxies by path prefix, and OTLP/HTTP is defined as
	// POST {endpoint}/v1/{traces,logs,metrics}. So the proxy IS the fan-out, in four
	// lines, and an app points at it exactly as it pointed at the collector:
	//
	//	OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:<port>
	//	OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
	//
	// A middleman that only forwards is a dependency, not a feature.
	otlpRoutes := map[string]string{
		"/v1/traces":  fmt.Sprintf("http://127.0.0.1:%d%sinsert/opentelemetry/v1/traces", vtracesPort, componentPathMap["victoria-traces"]),
		"/v1/logs":    fmt.Sprintf("http://127.0.0.1:%d%sinsert/opentelemetry/v1/logs", vlogsPort, componentPathMap["victoria-logs"]),
		"/v1/metrics": fmt.Sprintf("http://127.0.0.1:%d%sopentelemetry/v1/metrics", vmetricsPort, componentPathMap["victoria-metrics"]),

		// Newline-delimited JSON ingest, so a spool file written while nothing
		// was running can be absorbed later. The component ports are ephemeral,
		// so without a stable route here an importer would have to discover
		// them — which is precisely the coupling the proxy exists to remove.
		"/insert/jsonline": fmt.Sprintf("http://127.0.0.1:%d%sinsert/jsonline", vlogsPort, componentPathMap["victoria-logs"]),
	}
	for path, backend := range otlpRoutes {
		u, perr := url.Parse(backend)
		if perr != nil {
			return nil, fmt.Errorf("otlp route %s: %w", path, perr)
		}
		mgr.AddOTLPRoute(path, u)
	}

	addr := fmt.Sprintf("%s:%d", cfg.proxyBindAddr(), cfg.proxyPort())
	return &Service{
		Manager: mgr,
		// The OTLP endpoint is now the proxy itself, over HTTP. There is no gRPC
		// receiver any more: nothing in this stack speaks OTLP/gRPC, because nothing
		// needs to — Victoria takes OTLP/HTTP directly.
		CollectorAddr: "http://" + addr,
		HTTPAddr:      addr,
	}, nil
}

func resolveOTLPPort(configured, defaultPort int, label string, allocate func() (int, error)) (int, error) {
	if configured < 0 {
		return allocate()
	}
	port := defaultPort
	if configured > 0 {
		port = configured
	}
	if !IsPortAvailable(port) {
		return 0, fmt.Errorf("%s port %d already in use; configure a different port or set a negative value for ephemeral allocation", label, port)
	}
	return port, nil
}
