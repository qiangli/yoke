// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	runner_embed "github.com/qiangli/yoke/external/ollama/runner_embed"
)

// CmdOptions defines callbacks and settings for the Cobra command tree.
type CmdOptions struct {
	UseSystemBinaries bool
	RunEmbeddedServe  func(ctx context.Context) error
	RunDelegate       func(ctx context.Context, model string, args []string) error
	VersionDelegate   func(ctx context.Context) error
}

// NewOllamaCmd is a drop-in shim for the upstream `ollama` CLI.
func NewOllamaCmd(opts CmdOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ollama",
		Short: "Ollama-compatible CLI (shim onto ollama server)",
		Long: `Drop-in shim for the upstream ollama CLI. Each verb maps to either
the server's /api/* HTTP surface or to the system/embedded runner:

  ollama serve            → bind ollama on $OLLAMA_HOST
  ollama pull MODEL       → POST /api/pull
  ollama list / ls        → GET  /api/tags
  ollama rm MODEL         → DELETE /api/delete
  ollama ps               → GET  /api/ps
  ollama show MODEL       → POST /api/show
  ollama run MODEL [...]  → run the model interactively or with a prompt

All HTTP calls go to whatever OLLAMA_HOST resolves to.`,
	}

	cmd.AddCommand(
		newOllamaServeCmd(opts),
		newOllamaPullCmd(opts),
		newOllamaListCmd(opts),
		newOllamaRmCmd(opts),
		newOllamaPsCmd(opts),
		newOllamaShowCmd(opts),
		newOllamaRunCmd(opts),
		newOllamaVersionCmd(opts),
	)
	return cmd
}

// useEmbeddedServe reports whether `ollama serve` should run bashy's
// in-process embedded ollama server. That path needs an embedded-serve
// callback, no explicit --use-system-binaries request, AND the inference
// runner actually compiled into this binary (`-tags embed_runner`, see
// runner_embed.Available). Per docs/bashy-service-supply-chain-policy.md
// (Sprint 434) the embedded server is the ONLY allowed runtime; there is
// never a PATH or download fallback. If the runner is absent, serve fails
// closed with a policy error.
// runnerAvailable reports whether the embedded inference runner is compiled
// into this binary. It is a package var so tests can exercise both the
// embedded-serve and the fail-closed path on one build.
var runnerAvailable = runner_embed.Available

func useEmbeddedServe(opts CmdOptions) bool {
	return !opts.UseSystemBinaries && opts.RunEmbeddedServe != nil && runnerAvailable()
}

func newOllamaServeCmd(opts CmdOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Bind the ollama HTTP server on $OLLAMA_HOST (foreground)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if useEmbeddedServe(opts) {
				return opts.RunEmbeddedServe(cmd.Context())
			}
			// Fail closed per supply-chain policy: never fall back to a
			// downloaded archive or a PATH binary.
			return fmt.Errorf("ollama: embedded server unavailable — %w: per %s (supply-chain policy) refusing to exec upstream binary or download archive; build with -tags embed_runner (external/ollama/runner_embed)", ErrRunnerNotInstalled, ollamaSupplyChainPolicy)
		},
	}
}

// runSystemServe runs `bin serve` in the foreground until it exits or ctx ends.
// Cancelling ctx sends the child SIGTERM (Kill on Windows) and waits for it, so
// ollama can stop its own runner children instead of leaving them orphaned.
//
// Retained for the unix graceful-shutdown test (cmd_serve_unix_test.go) but
// no longer used as a fallback from `ollama serve` per supply-chain policy.
func runSystemServe(ctx context.Context, bin string, env []string) error {
	c := exec.CommandContext(ctx, bin, "serve")
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Stdin = os.Stdin
	c.Env = env
	c.Cancel = func() error {
		if runtime.GOOS == "windows" {
			return c.Process.Kill()
		}
		return c.Process.Signal(syscall.SIGTERM)
	}
	c.WaitDelay = 15 * time.Second
	err := c.Run()
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func newOllamaPullCmd(opts CmdOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "pull MODEL",
		Short: "Download a model",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := NewClient(DefaultURL())
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
			defer cancel()
			return client.Pull(ctx, args[0], func(p PullProgress) {
				if p.Total > 0 {
					fmt.Fprintf(os.Stderr, "\r%s: %d/%d", p.Status, p.Completed, p.Total)
				} else if p.Status != "" {
					fmt.Fprintf(os.Stderr, "\r%s", p.Status)
				}
			})
		},
	}
}

func newOllamaListCmd(opts CmdOptions) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List local models",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := NewClient(DefaultURL())
			models, err := client.List(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Printf("%-40s %-20s %s\n", "NAME", "SIZE", "MODIFIED")
			for _, m := range models {
				fmt.Printf("%-40s %-20d %s\n", m.Name, m.Size, m.ModifiedAt.Format(time.RFC3339))
			}
			return nil
		},
	}
}

func newOllamaRmCmd(opts CmdOptions) *cobra.Command {
	return &cobra.Command{
		Use:     "rm MODEL",
		Aliases: []string{"remove", "delete"},
		Short:   "Remove a model",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := NewClient(DefaultURL())
			return client.Delete(cmd.Context(), args[0])
		},
	}
}

func newOllamaPsCmd(opts CmdOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "ps",
		Short: "List running models",
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := ollamaGet(cmd.Context(), DefaultURL(), "/api/ps")
			if err != nil {
				return err
			}
			fmt.Println(string(body))
			return nil
		},
	}
}

func newOllamaShowCmd(opts CmdOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "show MODEL",
		Short: "Show model metadata",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := ollamaPostJSON(cmd.Context(), DefaultURL(), "/api/show", map[string]string{"name": args[0]})
			if err != nil {
				return err
			}
			fmt.Println(string(body))
			return nil
		},
	}
}

func newOllamaRunCmd(opts CmdOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run MODEL [PROMPT...]",
		Short: "Run a model — interactive REPL when no PROMPT, one-shot otherwise",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			model := args[0]
			rest := args[1:]
			if opts.RunDelegate != nil {
				return opts.RunDelegate(cmd.Context(), model, rest)
			}
			// Fail closed per supply-chain policy: never exec a third-party binary.
			return fmt.Errorf("ollama: third-party run disabled per %s — embedded only (use RunDelegate / embedded server; refusing to exec upstream ollama) — %w", ollamaSupplyChainPolicy, ErrRunnerNotInstalled)
		},
	}
	cmd.DisableFlagParsing = true
	return cmd
}

func newOllamaVersionCmd(opts CmdOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.VersionDelegate != nil {
				return opts.VersionDelegate(cmd.Context())
			}
			// Fail closed per supply-chain policy: never exec a third-party binary.
			return fmt.Errorf("ollama: third-party version check disabled per %s — embedded only (refusing to exec upstream ollama) — %w", ollamaSupplyChainPolicy, ErrRunnerNotInstalled)
		},
	}
}

func ollamaGet(ctx context.Context, baseURL, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama %s: HTTP %d: %s", path, resp.StatusCode, body)
	}
	return body, nil
}

func ollamaPostJSON(ctx context.Context, baseURL, path string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama %s: HTTP %d: %s", path, resp.StatusCode, out)
	}
	return out, nil
}
