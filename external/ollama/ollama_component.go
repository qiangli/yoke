// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package ollama

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ollama/ollama/envconfig"
	ollamaserver "github.com/ollama/ollama/server"
	"golang.org/x/crypto/ssh"

	runnerEmbed "github.com/qiangli/yoke/external/ollama/runner_embed"
	ollamaweb "github.com/qiangli/yoke/external/ollama/web"
)

// ErrRunnerNotInstalled is returned when the embedded inference runner
// is not compiled into this binary.
var ErrRunnerNotInstalled = errors.New("missing the embedded inference runner; please reinstall the binary or set inference.enabled: false to opt out")

// Config holds configuration parameters for the Ollama component.
type Config struct {
	Enabled   *bool  `json:"enabled,omitempty"`
	ModelsDir string `json:"modelsDir,omitempty"`
	UseSystem *bool  `json:"useSystem,omitempty"`
}

// IsEnabled returns true if the component is enabled.
func (c *Config) IsEnabled() bool {
	if c == nil || c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// UseSystemBinary returns true if the component should defer to a system binary.
func (c *Config) UseSystemBinary() bool {
	if c == nil || c.UseSystem == nil {
		return false
	}
	return *c.UseSystem
}

// TelemetryHooks holds callback hooks to delegate metrics/traces recording.
type TelemetryHooks struct {
	OnRunnerStart  func(ctx context.Context, port int)
	OnStatusChange func(healthy bool, port int)
}

// OllamaComponent implements the Component interface for
// the embedded Ollama HTTP server. It drives ollama's own server package
// in-process — same handler set as the standalone `ollama serve` daemon
// (api/tags, api/chat, api/embed, api/pull, /v1/...), with the model
// scheduler spawning runner subprocesses as needed.
type OllamaComponent struct {
	cfg     *Config
	dataDir string

	mu    sync.Mutex
	ln    net.Listener
	serve chan error // closed when ollamaserver.Serve returns

	healthy atomic.Bool
	Hooks   TelemetryHooks
}

// serveOnce gates the lifetime of ollamaserver.Serve to ONE call per
// process. The stdlib mux panics on duplicate registration.
var serveOnce sync.Once

// NewOllamaComponent creates a component that runs the embedded Ollama
// HTTP server. dataDir is the directory for model storage and runtime
// data (used as $OLLAMA_MODELS fallback).
func NewOllamaComponent(cfg *Config, dataDir string) *OllamaComponent {
	return &OllamaComponent{
		cfg:     cfg,
		dataDir: dataDir,
	}
}

// Name returns the component name.
func (o *OllamaComponent) Name() string { return "ollama" }

// prepare runs the side-effects Start needs: create the data dir, set
// $OLLAMA_MODELS, and ensure the ed25519 keypair exists.
func (o *OllamaComponent) prepare() error {
	if err := os.MkdirAll(o.dataDir, 0o755); err != nil {
		return fmt.Errorf("ollama: create data dir: %w", err)
	}
	if o.cfg != nil && o.cfg.ModelsDir != "" {
		os.Setenv("OLLAMA_MODELS", o.cfg.ModelsDir)
	}
	if err := ensureOllamaKeypair(); err != nil {
		return fmt.Errorf("ollama: ensure keypair: %w", err)
	}
	return nil
}

// ensureOllamaKeypair generates the ed25519 keypair at
// ~/.ollama/id_ed25519{,.pub} if it doesn't already exist.
func ensureOllamaKeypair() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	privKeyPath := filepath.Join(home, ".ollama", "id_ed25519")
	pubKeyPath := filepath.Join(home, ".ollama", "id_ed25519.pub")

	if _, err := os.Stat(privKeyPath); err == nil {
		return nil // already present
	} else if !os.IsNotExist(err) {
		return err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	privBytes, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(privKeyPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(privKeyPath, pem.EncodeToMemory(privBytes), 0o600); err != nil {
		return err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	if err := os.WriteFile(pubKeyPath, ssh.MarshalAuthorizedKey(sshPub), 0o644); err != nil {
		return err
	}
	slog.Info("ollama: generated identity keypair", "path", privKeyPath)
	return nil
}

// Start binds the ollama HTTP listener (default 127.0.0.1:11434, override
// via OLLAMA_HOST) and runs ollama's server.Serve in a goroutine.
func (o *OllamaComponent) Start(ctx context.Context) error {
	if o.cfg.UseSystemBinary() {
		baseURL := DefaultURL()
		hostAddr := envconfig.Host().Host
		if err := waitTCPReady(hostAddr, 1*time.Second); err != nil {
			return fmt.Errorf("ollama: system mode requested but no daemon reachable at %s — start `ollama serve` yourself or omit --use-system-binaries to use the embedded runtime", baseURL)
		}
		slog.Info("ollama: deferring to system daemon", "url", baseURL)
		o.healthy.Store(true)
		if o.Hooks.OnStatusChange != nil {
			o.Hooks.OnStatusChange(true, o.Port())
		}
		return nil
	}

	if !runnerEmbed.Available() {
		return fmt.Errorf("ollama: %w", ErrRunnerNotInstalled)
	}

	if err := o.prepare(); err != nil {
		return err
	}

	hostAddr := envconfig.Host().Host
	ln, err := net.Listen("tcp", hostAddr)
	if err != nil {
		return fmt.Errorf("ollama: listen on %s: %w", hostAddr, err)
	}

	o.mu.Lock()
	o.ln = ln
	o.serve = make(chan error, 1)
	o.mu.Unlock()

	started := false
	serveOnce.Do(func() {
		started = true
		go func(ln net.Listener, done chan<- error) {
			slog.Info("ollama: serving on", "addr", ln.Addr().String())
			err := ollamaserver.Serve(ln)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("ollama: server exited", "error", err)
				o.healthy.Store(false)
				if o.Hooks.OnStatusChange != nil {
					o.Hooks.OnStatusChange(false, 0)
				}
			}
			done <- err
			close(done)
		}(ln, o.serve)
	})
	if !started {
		_ = ln.Close()
		o.mu.Lock()
		o.ln = nil
		o.mu.Unlock()
		slog.Warn("ollama: server already running in this process; Start is a no-op")
		o.healthy.Store(true)
		return nil
	}

	if err := waitTCPReady(ln.Addr().String(), 5*time.Second); err != nil {
		slog.Warn("ollama: server slow to accept connections", "error", err)
	}

	o.healthy.Store(true)
	if o.Hooks.OnRunnerStart != nil {
		o.Hooks.OnRunnerStart(ctx, o.Port())
	}
	if o.Hooks.OnStatusChange != nil {
		o.Hooks.OnStatusChange(true, o.Port())
	}
	return nil
}

// Stop gracefully shuts down the component.
func (o *OllamaComponent) Stop(ctx context.Context) error {
	o.healthy.Store(false)
	if o.Hooks.OnStatusChange != nil {
		o.Hooks.OnStatusChange(false, 0)
	}

	if o.cfg.UseSystemBinary() {
		return nil
	}

	o.mu.Lock()
	ln := o.ln
	serve := o.serve
	o.ln = nil
	o.mu.Unlock()

	if ln == nil {
		return nil
	}
	closeErr := ln.Close()

	if serve != nil {
		select {
		case <-serve:
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
		}
	}
	return closeErr
}

// Healthy returns true if the component is healthy.
func (o *OllamaComponent) Healthy() bool { return o.healthy.Load() }

// HTTPHandler returns a reverse-proxy mounted at /ollama/ on the
// dashboard.
func (o *OllamaComponent) HTTPHandler() http.Handler {
	base := o.BaseURL()
	if base == "" {
		return nil
	}
	target, err := url.Parse(base)
	if err != nil {
		return nil
	}
	apiProxy := httputil.NewSingleHostReverseProxy(target)
	staticHandler := ollamaweb.Handler()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/v1/") {
			apiProxy.ServeHTTP(w, r)
			return
		}
		staticHandler.ServeHTTP(w, r)
	})
}

// Port returns the bound port.
func (o *OllamaComponent) Port() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ln == nil {
		return 0
	}
	if addr, ok := o.ln.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

// BaseURL returns the base URL client should hit.
func (o *OllamaComponent) BaseURL() string {
	if o.cfg.UseSystemBinary() {
		return "http://" + envconfig.ConnectableHost().Host
	}
	o.mu.Lock()
	ln := o.ln
	o.mu.Unlock()
	if ln == nil {
		return ""
	}
	return "http://" + envconfig.ConnectableHost().Host
}

// waitTCPReady probes the listener address until it accepts connections
// or the deadline expires.
func waitTCPReady(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("%s not accepting after %v", addr, timeout)
}
