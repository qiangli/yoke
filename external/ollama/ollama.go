// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package ollama is a thin shell-out front-door to an externally installed
// ollama binary, and provides Cobra commands to manage and run models.
// It is Layer 2 ("Sandbox tier") of the AgentOS substrate plan (see docs/agentos-substrate-extraction-plan.md).
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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ErrNotFound is returned by Resolve when no ollama binary can be located.
var ErrNotFound = errors.New("ollama binary not found")

// Resolve locates an ollama binary to exec, in priority order:
//  1. $OLLAMA_BIN (explicit override).
//  2. "ollama" on $PATH.
//  3. Well-known install locations, including a binary a sibling tool (e.g.
//     ycode) may have already fetched into the per-user cache.
//
// It returns ErrNotFound (wrapped) when nothing usable is present.
func Resolve() (string, error) {
	if p := os.Getenv("OLLAMA_BIN"); p != "" {
		if isExecutable(p) {
			return p, nil
		}
		return "", fmt.Errorf("OLLAMA_BIN=%q is not an executable file: %w", p, ErrNotFound)
	}
	if p, err := exec.LookPath("ollama"); err == nil {
		return p, nil
	}
	for _, p := range candidatePaths() {
		if isExecutable(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w on PATH or in known locations; install ollama or set OLLAMA_BIN", ErrNotFound)
}

// candidatePaths lists fallback locations to probe when ollama is not on PATH.
func candidatePaths() []string {
	var paths []string
	bin := ollamaName()
	// An ollama a sibling agent tool already fetched (per-user cache).
	if dir, err := os.UserCacheDir(); err == nil {
		paths = append(paths, filepath.Join(dir, "ycode", "bin", bin))
		paths = append(paths, filepath.Join(dir, "bashy", "bin", bin))
	}
	if runtime.GOOS == "darwin" {
		paths = append(paths, "/Applications/Ollama.app/Contents/Resources/ollama")
	}
	if runtime.GOOS != "windows" {
		paths = append(paths,
			"/opt/homebrew/bin/ollama",
			"/usr/local/bin/ollama",
			"/usr/bin/ollama",
		)
	}
	return paths
}

func ollamaName() string {
	if runtime.GOOS == "windows" {
		return "ollama.exe"
	}
	return "ollama"
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return fi.Mode().Perm()&0o111 != 0
}

// Run resolves ollama and execs it with args as a transparent pass-through,
// wiring the given stdio and inheriting the process environment. It returns
// the child's exit code (127 if ollama cannot be located, matching the shell
// "command not found" convention).
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	bin, err := Resolve()
	if err != nil {
		fmt.Fprintln(stderr, "bashy ollama:", err)
		return 127
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if code := ee.ExitCode(); code >= 0 {
				return code
			}
			return 1
		}
		fmt.Fprintln(stderr, "bashy ollama:", err)
		return 1
	}
	return 0
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
