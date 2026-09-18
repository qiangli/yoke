// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// The managed ollama is ALWAYS OUR OWN, fully isolated from any host install:
// its own port (never 11434), its own models dir (never ~/.ollama). It is
// reached over the mesh by the `ollama` service name (consumers bind ephemeral
// local ports), so the fixed port only needs to be unique on the serving host.
// See dhnt/docs/external-binary-builtins.md + docs/local-p2p-cicd.md.
const (
	// DefaultManagedPort is bashy's own ollama port — deliberately NOT 11434, so
	// a managed daemon never fights a host ollama. Override with --port /
	// $BASHY_OLLAMA_PORT (0 = OS-ephemeral).
	DefaultManagedPort = 11435
	managedBindAddr    = "127.0.0.1"
)

// ManagedDataDir is the bashy-owned ollama work dir: $BASHY_OLLAMA_DIR, else
// ~/.agents/bashy/ollama. Kept separate from a host ollama's ~/.ollama.
func ManagedDataDir() string {
	if d := strings.TrimSpace(os.Getenv("BASHY_OLLAMA_DIR")); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "bashy-ollama")
	}
	return filepath.Join(home, ".agents", "bashy", "ollama")
}

// ManagedModelsDir is the bashy-owned model store: $OLLAMA_MODELS if set, else
// <ManagedDataDir>/models. Never the host's ~/.ollama/models.
func ManagedModelsDir() string {
	if d := strings.TrimSpace(os.Getenv("OLLAMA_MODELS")); d != "" {
		return d
	}
	return filepath.Join(ManagedDataDir(), "models")
}

// managedPort resolves the bashy-owned bind port: $BASHY_OLLAMA_PORT (incl. 0
// for OS-ephemeral) else DefaultManagedPort.
func managedPort() int {
	if v := strings.TrimSpace(os.Getenv("BASHY_OLLAMA_PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return DefaultManagedPort
}

// ManagedURL is the bashy-owned daemon URL the managed front-door targets.
func ManagedURL() string {
	return fmt.Sprintf("http://%s:%d", managedBindAddr, managedPort())
}

// applyManagedEnv points this process at the bashy-owned daemon WITHOUT touching
// a host ollama: it sets OLLAMA_HOST (bind + client target) and OLLAMA_MODELS in
// our own process env only, and only when unset — an explicit override wins.
// Crucially it NEVER lets the effective host fall through to the upstream 11434
// default. Returns the resolved base URL.
func applyManagedEnv(port int) string {
	if strings.TrimSpace(os.Getenv("OLLAMA_HOST")) == "" {
		os.Setenv("OLLAMA_HOST", fmt.Sprintf("%s:%d", managedBindAddr, port))
	}
	if strings.TrimSpace(os.Getenv("OLLAMA_MODELS")) == "" {
		os.Setenv("OLLAMA_MODELS", ManagedModelsDir())
	}
	return DefaultURL()
}

// RunManagedServe binds + runs the embedded ollama server on the bashy-owned
// port (never 11434), with models under the bashy-owned dir, blocking until the
// context is cancelled or SIGINT/SIGTERM arrives.
func RunManagedServe(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	port := managedPort()
	url := applyManagedEnv(port)
	dataDir := ManagedDataDir()
	models := ManagedModelsDir()

	comp := NewOllamaComponent(&Config{ModelsDir: models}, dataDir)
	if err := comp.Start(ctx); err != nil {
		return err
	}
	fmt.Printf("ollama (managed, isolated) serving on %s — models %s\n", url, models)
	fmt.Println("expose it over the mesh:  outpost mesh service add ollama " + strings.TrimPrefix(comp.BaseURL(), "http://"))

	<-ctx.Done()
	fmt.Println("ollama: shutting down…")
	return comp.Stop(context.Background())
}

// NewManagedOllamaCmd builds the `ollama` command tree backed by the managed,
// isolated daemon — the bashy front-door. `serve` runs the embedded server;
// every other verb targets the bashy-owned daemon by default (OLLAMA_HOST
// override still honored). No host ollama binary required.
func NewManagedOllamaCmd() *cobra.Command {
	cmd := NewOllamaCmd(CmdOptions{
		RunEmbeddedServe: func(ctx context.Context) error { return RunManagedServe(ctx) },
		RunDelegate:      runManagedModel,
	})
	// Default every subcommand at the bashy-owned daemon (never 11434) unless the
	// caller set OLLAMA_HOST explicitly.
	cmd.PersistentPreRun = func(*cobra.Command, []string) { applyManagedEnv(managedPort()) }
	return cmd
}

// runManagedModel implements `ollama run MODEL [PROMPT...]` against the managed
// daemon's HTTP API — one-shot when a prompt is given, otherwise a minimal
// stdin REPL. No host ollama binary required (the embedded daemon serves it).
func runManagedModel(ctx context.Context, model string, args []string) error {
	base := DefaultURL()
	if prompt := strings.TrimSpace(strings.Join(args, " ")); prompt != "" {
		return streamGenerate(ctx, base, model, prompt)
	}
	fmt.Printf("ollama run %s — empty line or Ctrl-D to exit\n", model)
	sc := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print(">>> ")
		if !sc.Scan() {
			fmt.Println()
			return sc.Err()
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			return nil
		}
		if err := streamGenerate(ctx, base, model, line); err != nil {
			return err
		}
	}
}

// streamGenerate posts to /api/generate and streams the response tokens.
func streamGenerate(ctx context.Context, base, model, prompt string) error {
	payload, _ := json.Marshal(map[string]any{"model": model, "prompt": prompt, "stream": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/generate", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama run: HTTP %d", resp.StatusCode)
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var chunk struct {
			Response string `json:"response"`
			Done     bool   `json:"done"`
		}
		if err := dec.Decode(&chunk); err != nil {
			break
		}
		fmt.Print(chunk.Response)
		if chunk.Done {
			break
		}
	}
	fmt.Println()
	return nil
}
