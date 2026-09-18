package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
)

// ContainerComponent implements observability.Component for the container subsystem.
// It manages the container engine lifecycle, session network, and provides
// a management HTTP API.
type ContainerComponent struct {
	cfg       *ComponentConfig
	dataDir   string
	sessionID string

	engine      *Engine
	networkName string
	pool        *Pool
	healthy     atomic.Bool
	otel        *otelState

	// Service discovery ports (set by StackManager wiring before Start).
	ollamaPort        int
	collectorGRPCPort int
	proxyPort         int

	// Track active containers.
	containers sync.Map // name → *Container
}

// ComponentConfig holds configuration for the container component.
type ComponentConfig struct {
	Enabled      bool   `json:"enabled"`                // enable container isolation
	SocketPath   string `json:"socketPath,omitempty"`   // explicit podman socket path
	Image        string `json:"image,omitempty"`        // default sandbox image
	Network      string `json:"network,omitempty"`      // network mode: "bridge" (default), "host", "none"
	ReadOnlyRoot bool   `json:"readOnlyRoot,omitempty"` // read-only root filesystem (default true)
	PoolSize     int    `json:"poolSize,omitempty"`     // warm pool size (0 = no pool)
	CPUs         string `json:"cpus,omitempty"`         // per-container CPU limit
	Memory       string `json:"memory,omitempty"`       // per-container memory limit
	UseSystem    bool   `json:"useSystem,omitempty"`    // defer to system-installed podman (no auto-provisioned VM/in-process service)
}

// NewContainerComponent creates a new container component.
func NewContainerComponent(cfg *ComponentConfig, dataDir string) *ContainerComponent {
	if cfg.Image == "" {
		cfg.Image = "ycode-sandbox:latest"
	}
	if cfg.Network == "" {
		cfg.Network = "bridge"
	}

	return &ContainerComponent{
		cfg:       cfg,
		dataDir:   dataDir,
		sessionID: uuid.New().String()[:8],
	}
}

// Name returns the component name.
func (c *ContainerComponent) Name() string { return "containers" }

// Start initializes the container engine, creates the session network,
// cleans up orphans, and optionally warms the container pool.
func (c *ContainerComponent) Start(ctx context.Context) error {
	slog.Info("container: starting component")

	engine, err := NewEngine(ctx, &EngineConfig{
		SocketPath: c.cfg.SocketPath,
		DataDir:    c.dataDir,
		UseSystem:  c.cfg.UseSystem,
	})
	if err != nil {
		return fmt.Errorf("container: init engine: %w", err)
	}
	c.engine = engine

	// Log version.
	if version, err := engine.Version(ctx); err == nil {
		slog.Info("container: podman version", "version", version)
	}

	// Clean up orphans from previous sessions.
	if err := engine.CleanupOrphans(ctx, c.sessionID); err != nil {
		slog.Warn("container: orphan cleanup failed", "error", err)
	}

	// Create session network.
	c.networkName = "ycode-" + c.sessionID
	if c.cfg.Network == "bridge" {
		if err := engine.CreateNetwork(ctx, c.networkName); err != nil {
			slog.Warn("container: failed to create session network", "error", err)
			c.networkName = "" // fall back to default
		}
	}

	// Build sandbox image if needed.
	go func() {
		if err := engine.BuildSandboxImage(ctx, c.cfg.Image); err != nil {
			slog.Warn("container: sandbox image build failed, will try pull on demand", "error", err)
		}
	}()

	// Initialize pool if configured.
	if c.cfg.PoolSize > 0 {
		c.pool = NewPool(engine, c.defaultContainerConfig(), c.cfg.PoolSize)
		go func() {
			if err := c.pool.Warm(ctx); err != nil {
				slog.Warn("container: pool warm failed", "error", err)
			}
		}()
	}

	c.healthy.Store(true)
	c.traceComponentStart(ctx)
	slog.Info("container: component started",
		"session", c.sessionID,
		"network", c.networkName,
		"image", c.cfg.Image,
	)
	return nil
}

// Stop shuts down all containers, removes the session network, and closes the engine.
func (c *ContainerComponent) Stop(ctx context.Context) error {
	c.healthy.Store(false)
	slog.Info("container: stopping component")

	// Drain pool first.
	if c.pool != nil {
		c.pool.Close(ctx)
	}

	// Stop and remove all tracked containers.
	c.containers.Range(func(key, value any) bool {
		ctr := value.(*Container)
		if err := ctr.Remove(ctx, true); err != nil {
			slog.Warn("container: failed to remove container", "name", key, "error", err)
		}
		return true
	})

	// Clean up session resources.
	if c.engine != nil {
		_ = c.engine.CleanupSession(ctx, c.sessionID)
		_ = c.engine.Close(ctx)
	}

	c.traceComponentStop(ctx)
	return nil
}

// Healthy returns true if the container engine is operational.
func (c *ContainerComponent) Healthy() bool {
	return c.healthy.Load() && c.engine != nil && c.engine.Healthy()
}

// HTTPHandler returns an HTTP handler for the container management API.
func (c *ContainerComponent) HTTPHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		status := map[string]any{
			"healthy":  c.Healthy(),
			"session":  c.sessionID,
			"network":  c.networkName,
			"image":    c.cfg.Image,
			"poolSize": c.cfg.PoolSize,
		}

		// Count active containers.
		count := 0
		c.containers.Range(func(_, _ any) bool {
			count++
			return true
		})
		status["activeContainers"] = count

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	})

	mux.HandleFunc("GET /api/containers", func(w http.ResponseWriter, r *http.Request) {
		if c.engine == nil {
			http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
			return
		}
		containers, err := c.engine.ListContainers(r.Context(), map[string]string{
			"label": SessionLabel + "=" + c.sessionID,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(containers)
	})

	return mux
}

// Engine returns the underlying container engine.
func (c *ContainerComponent) Engine() *Engine { return c.engine }

// SessionID returns the current session identifier.
func (c *ContainerComponent) SessionID() string { return c.sessionID }

// SetServicePorts configures the host service ports for container environment injection.
// Called by StackManager wiring in serve.go before Start().
func (c *ContainerComponent) SetServicePorts(ollamaPort, collectorGRPCPort, proxyPort int) {
	c.ollamaPort = ollamaPort
	c.collectorGRPCPort = collectorGRPCPort
	c.proxyPort = proxyPort
}

// ServiceEnv returns environment variables that containers need to reach host services.
func (c *ContainerComponent) ServiceEnv() map[string]string {
	if c.engine == nil {
		return nil
	}

	gateway := c.engine.HostGateway()
	env := map[string]string{
		"YCODE_SESSION_ID": c.sessionID,
	}

	if c.ollamaPort > 0 {
		env["OLLAMA_HOST"] = fmt.Sprintf("http://%s:%d", gateway, c.ollamaPort)
	}
	if c.collectorGRPCPort > 0 {
		env["OTEL_EXPORTER_OTLP_ENDPOINT"] = fmt.Sprintf("http://%s:%d", gateway, c.collectorGRPCPort)
		env["OTEL_EXPORTER_OTLP_PROTOCOL"] = "grpc"
	}
	if c.proxyPort > 0 {
		env["YCODE_PROXY_URL"] = fmt.Sprintf("http://%s:%d", gateway, c.proxyPort)
	}

	return env
}

// CreateAgentContainer creates an isolated container for an agent.
func (c *ContainerComponent) CreateAgentContainer(ctx context.Context, agentID string, mounts []Mount) (*Container, error) {
	// Try pool first.
	if c.pool != nil {
		if ctr, err := c.pool.Acquire(ctx); err == nil {
			c.containers.Store(agentID, ctr)
			c.updateOTELGauges()
			return ctr, nil
		}
	}

	cfg := c.defaultContainerConfig()
	cfg.Name = fmt.Sprintf("ycode-%s-agent-%s", c.sessionID, agentID[:8])
	cfg.Mounts = append(cfg.Mounts, mounts...)

	ctr, err := c.engine.CreateContainer(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create agent container: %w", err)
	}

	if err := ctr.Start(ctx); err != nil {
		ctr.Remove(ctx, true)
		return nil, fmt.Errorf("start agent container: %w", err)
	}

	c.containers.Store(agentID, ctr)
	c.traceContainerCreate(ctx, agentID, cfg.Name)
	c.updateOTELGauges()
	return ctr, nil
}

// RemoveAgentContainer stops and removes an agent's container.
func (c *ContainerComponent) RemoveAgentContainer(ctx context.Context, agentID string) error {
	val, ok := c.containers.LoadAndDelete(agentID)
	if !ok {
		return nil
	}

	ctr := val.(*Container)

	// Return to pool if possible.
	if c.pool != nil {
		c.pool.Release(ctr)
		c.updateOTELGauges()
		return nil
	}

	err := ctr.Remove(ctx, true)
	c.traceContainerRemove(ctx, agentID)
	c.updateOTELGauges()
	return err
}

// defaultContainerConfig returns the default container configuration
// with security hardening and service environment.
func (c *ContainerComponent) defaultContainerConfig() *ContainerConfig {
	cfg := &ContainerConfig{
		Image:    c.cfg.Image,
		ReadOnly: c.cfg.ReadOnlyRoot,
		Init:     true,
		CapDrop:  []string{"ALL"},
		Tmpfs:    []string{"/tmp", "/var/tmp", "/run"},
		Env:      c.ServiceEnv(),
		Labels: map[string]string{
			SessionLabel:    c.sessionID,
			"ycode.managed": "true",
		},
	}

	if c.networkName != "" {
		cfg.Network = c.networkName
	}
	if c.cfg.CPUs != "" {
		cfg.CPUs = c.cfg.CPUs
	}
	if c.cfg.Memory != "" {
		cfg.Memory = c.cfg.Memory
	}

	return cfg
}
