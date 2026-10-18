// Package ollm wraps the Ollama API client, isolating the rest of the
// codebase from the underlying github.com/ollama/ollama dependency.
// Swap the implementation here without touching internal consumers.
package ollm

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ollama/ollama/api"
	ollamaembed "github.com/ollama/ollama/embed"
)

// Client wraps an Ollama API client.
type Client struct {
	inner   *api.Client
	baseURL string
}

// Model describes a locally available model.
type Model struct {
	Name       string
	Size       int64
	ModifiedAt time.Time
	Details    ModelDetails
}

// ModelDetails contains metadata about a model's architecture and quantization.
type ModelDetails struct {
	Family            string
	ParameterSize     string
	QuantizationLevel string
}

// PullProgress reports download progress during a Pull operation.
type PullProgress struct {
	Status    string
	Completed int64
	Total     int64
}

// NewClient creates an Ollama API client for the given base URL.
func NewClient(baseURL string) (*Client, error) {
	c, err := ollamaembed.NewClient(baseURL)
	if err != nil {
		return nil, fmt.Errorf("create ollama client: %w", err)
	}
	return &Client{inner: c, baseURL: baseURL}, nil
}

// List returns all models available on the connected Ollama server.
func (c *Client) List(ctx context.Context) ([]Model, error) {
	resp, err := c.inner.List(ctx)
	if err != nil {
		return nil, err
	}
	models := make([]Model, 0, len(resp.Models))
	for _, m := range resp.Models {
		models = append(models, Model{
			Name:       m.Name,
			Size:       m.Size,
			ModifiedAt: m.ModifiedAt,
			Details: ModelDetails{
				Family:            m.Details.Family,
				ParameterSize:     m.Details.ParameterSize,
				QuantizationLevel: m.Details.QuantizationLevel,
			},
		})
	}
	return models, nil
}

// Pull downloads a model from the Ollama registry.
func (c *Client) Pull(ctx context.Context, model string, progress func(PullProgress)) error {
	req := &ollamaembed.PullRequest{Model: model}
	return c.inner.Pull(ctx, req, func(resp ollamaembed.ProgressResponse) error {
		if progress != nil {
			progress(PullProgress{
				Status:    resp.Status,
				Completed: resp.Completed,
				Total:     resp.Total,
			})
		}
		return nil
	})
}

// Delete removes a model from the Ollama server.
func (c *Client) Delete(ctx context.Context, model string) error {
	return c.inner.Delete(ctx, &ollamaembed.DeleteRequest{Model: model})
}

// Import imports a local GGUF file into Ollama's registry under the given name.
func (c *Client) Import(ctx context.Context, model, ggufPath string, progress func(status string)) error {
	req := &ollamaembed.CreateRequest{
		Model: model,
		From:  ggufPath,
	}
	return c.inner.Create(ctx, req, func(resp ollamaembed.ProgressResponse) error {
		if progress != nil && resp.Status != "" {
			progress(resp.Status)
		}
		return nil
	})
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
