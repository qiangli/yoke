// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package ollama

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	ollamaweb "github.com/qiangli/yoke/external/ollama/web"
)

// OllamaUIComponent provides the Ollama management web UI.
// It auto-detects a running Ollama server (managed by ycode or standalone)
// and proxies API requests to it. The management SPA is always available.
type OllamaUIComponent struct {
	ollamaURL string // resolved Ollama API URL
	healthy   atomic.Bool
}

// NewOllamaUIComponent creates a UI component that auto-detects Ollama.
func NewOllamaUIComponent() *OllamaUIComponent {
	return &OllamaUIComponent{}
}

// Name returns the component name.
func (u *OllamaUIComponent) Name() string { return "ollama" }

// Start starts the UI component.
func (u *OllamaUIComponent) Start(ctx context.Context) error {
	// Auto-detect Ollama server.
	candidates := []string{
		DefaultURL(), // OLLAMA_HOST env or localhost:11434
	}

	for _, url := range candidates {
		if Detect(ctx, url) {
			u.ollamaURL = url
			u.healthy.Store(true)
			slog.Info("ollama-ui: detected Ollama server", "url", url)
			return nil
		}
	}

	// No running Ollama found — UI still works, just shows "disconnected".
	u.ollamaURL = DefaultURL()
	slog.Info("ollama-ui: no running Ollama detected, UI will show disconnected state", "default", u.ollamaURL)
	return nil
}

// Stop stops the UI component.
func (u *OllamaUIComponent) Stop(ctx context.Context) error {
	u.healthy.Store(false)
	return nil
}

// Healthy returns true if the component is healthy.
func (u *OllamaUIComponent) Healthy() bool {
	return true // UI is always healthy; Ollama connectivity is shown in the SPA
}

// HTTPHandler returns a composite handler: /api/* proxied to Ollama,
// everything else served from the embedded management SPA.
func (u *OllamaUIComponent) HTTPHandler() http.Handler {
	staticHandler := ollamaweb.Handler()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/registry/tags" {
			// Proxy to Ollama registry (ollama.com) server-side to avoid CORS.
			proxyRegistryTags(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			// Proxy to Ollama server.
			target, err := url.Parse(u.ollamaURL)
			if err != nil {
				http.Error(w, fmt.Sprintf("invalid ollama URL: %v", err), http.StatusBadGateway)
				return
			}
			proxy := httputil.NewSingleHostReverseProxy(target)
			proxy.ServeHTTP(w, r)
			return
		}
		staticHandler.ServeHTTP(w, r)
	})
}

const ollamaRegistryURL = "https://ollama.com/api/tags"

// proxyRegistryTags fetches the Ollama model registry server-side,
// avoiding CORS issues from direct browser-to-ollama.com requests.
func proxyRegistryTags(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ollamaRegistryURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[]}`))
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
