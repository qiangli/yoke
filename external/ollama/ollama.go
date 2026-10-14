// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package ollama fronts the bashy-managed ollama server and provides Cobra
// commands to manage and run models. It is Layer 2 ("Sandbox tier") of the
// AgentOS substrate plan (see docs/agentos-substrate-extraction-plan.md).
//
// Supply-chain note (Sprint 436, docs/bashy-service-supply-chain-policy.md):
// bashy must never exec a third-party-built ollama. The ONLY supported runtime
// is the bashy-signed embedded runner (Sprint 436 story 732cbb22) compiled
// from source (external/ollama: runner_embed, src, ollama_component.go). The
// downloaded official-archive fallback and the PATH / OLLAMA_BIN fallback are
// removed. If the embedded build is unavailable on a platform, fail closed
// with a clear error naming the supply-chain policy.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const ollamaSupplyChainPolicy = "docs/bashy-service-supply-chain-policy.md"

// signedRunnerPointer names the only supported runtime in refusal errors.
const signedRunnerPointer = "the only supported runtime is the bashy-signed embedded runner (Sprint 436 story 732cbb22; build with -tags embed_runner)"

// ErrNotFound is returned by Resolve, which now always fails per supply-chain
// policy. Retained for compatibility.
var ErrNotFound = errors.New("ollama binary not found")

// Resolve locates an ollama binary to exec.
//
// Disabled per docs/bashy-service-supply-chain-policy.md: bashy never execs a
// third-party-built ollama. This function always returns a fail-closed error
// naming the policy, and never probes PATH, OLLAMA_BIN, or well-known
// locations.
func Resolve() (string, error) {
	return "", fmt.Errorf("ollama: third-party binary execution disabled per %s (supply-chain policy) — %s — refusing to exec upstream ollama: %w", ollamaSupplyChainPolicy, signedRunnerPointer, ErrNotFound)
}

// Run would resolve ollama and exec it with args as a transparent
// pass-through. It returns the child's exit code (127 if ollama cannot be
// located, matching the shell "command not found" convention).
//
// Disabled per docs/bashy-service-supply-chain-policy.md: always fails closed
// with 127 and a policy error, never execs a third-party binary.
func Run(_ context.Context, _ []string, _ io.Reader, _, stderr io.Writer) int {
	fmt.Fprintln(stderr, "bashy ollama: third-party binary execution disabled per", ollamaSupplyChainPolicy, "(supply-chain policy) —", signedRunnerPointer+"; refusing to exec upstream ollama")
	return 127
}

// DefaultURL returns the default Ollama server URL, honoring $OLLAMA_HOST.
func DefaultURL() string {
	if u := os.Getenv("OLLAMA_HOST"); u != "" {
		if !strings.HasPrefix(u, "http") {
			return "http://" + u
		}
		return u
	}
	return "http://127.0.0.1:11434"
}

// Client is a lightweight HTTP client for the Ollama API.
type Client struct {
	baseURL string
	client  *http.Client
}

// Model describes a locally available model.
type Model struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
	Details    Details   `json:"details"`
}

// Details contains metadata about a model's architecture and quantization.
type Details struct {
	Family            string `json:"family"`
	ParameterSize     string `json:"parameter_size"`
	QuantizationLevel string `json:"quantization_level"`
}

// PullProgress reports download progress during a Pull operation.
type PullProgress struct {
	Status    string `json:"status"`
	Completed int64  `json:"completed"`
	Total     int64  `json:"total"`
}

// NewClient creates an Ollama API client for the given base URL.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		client:  &http.Client{Timeout: 30 * time.Minute},
	}
}

// List returns all models available on the connected Ollama server.
func (c *Client) List(ctx context.Context) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama list: HTTP %d: %s", resp.StatusCode, body)
	}
	var res struct {
		Models []Model `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return res.Models, nil
}

// Pull downloads a model from the Ollama registry.
func (c *Client) Pull(ctx context.Context, model string, progress func(PullProgress)) error {
	payload, err := json.Marshal(map[string]string{"name": model})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/pull", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ollama pull: HTTP %d: %s", resp.StatusCode, body)
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var p PullProgress
		if err := dec.Decode(&p); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		if progress != nil {
			progress(p)
		}
	}
	return nil
}

// Delete removes a model from the Ollama server.
func (c *Client) Delete(ctx context.Context, model string) error {
	payload, err := json.Marshal(map[string]string{"name": model})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "DELETE", c.baseURL+"/api/delete", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ollama delete: HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}

// Detect checks if an Ollama server is reachable at the given URL.
func Detect(ctx context.Context, baseURL string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
