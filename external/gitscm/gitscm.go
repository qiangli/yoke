// Package gitscm provisions a REAL git — the official git-for-windows MinGit on
// Windows — as a managed, checksum-verified toolchain. It is distinct from the
// pure-Go `bashy git` client (coreutils/git), which is a deliberate SUBSET for
// the common clone→commit→push cycle. Some consumers need a full git: e.g. the
// Gitea act_runner host executor's checkout/prepare flow probes `git version`
// and drives operations bashy git doesn't implement. `bashy git-scm` gives them
// one, download-on-demand, no system install. MinGit is a portable no-installer
// build (MIT/GPL — downloaded + run as a separate program, never linked).
package gitscm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// Pinned git-for-windows MinGit (64-bit). git-for-windows publishes no checksum
// file, so the sha256 is pinned in-source (downloaded + verified in dev) — the
// strongest supply-chain anchor. Bump all four together when updating.
const (
	minGitTag    = "v2.55.0.windows.2"
	minGitAsset  = "MinGit-2.55.0.2-64-bit.zip"
	minGitVer    = "2.55.0.2"
	minGitSHA256 = "e3ea2944cea4b3fabcd69c7c1669ef69b1b66c05ac7806d81224d0abad2dec31"
)

// Ensure returns a path to a real git executable. On Windows it provisions the
// pinned, checksum-verified MinGit (cmd/git.exe — the env-setting wrapper). On
// unix it uses the system git on PATH (which every unix host executor already
// has); provisioning a portable unix git is out of scope.
func Ensure(ctx context.Context) (string, error) {
	if runtime.GOOS != "windows" {
		if p, err := exec.LookPath("git"); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("gitscm: no system git on PATH (unix hosts use the platform git)")
	}
	if runtime.GOARCH != "amd64" {
		return "", fmt.Errorf("gitscm: MinGit is published only for windows/amd64 (got %s)", runtime.GOARCH)
	}
	url := "https://github.com/git-for-windows/git/releases/download/" + minGitTag + "/" + minGitAsset
	tool := binmgr.Tool{
		Name: "mingit", Version: minGitVer,
		Assets: map[string]binmgr.Asset{
			binmgr.Platform(): {URL: url, SHA256: minGitSHA256, Tree: true, Entrypoint: "cmd/git.exe"},
		},
	}
	return binmgr.Ensure(ctx, tool)
}

// NewGitSCMCmd is the `bashy git-scm` front-door: provision a real git, then exec
// it with the user's args. All flags pass through unchanged.
func NewGitSCMCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "git-scm",
		Short:              "Real Git (git-for-windows MinGit on Windows; system git on unix), auto-provisioned + verified",
		Long:               "A full git for consumers that need more than the pure-Go `bashy git` subset (e.g. the act_runner host executor). On Windows it downloads + sha256-verifies + caches the pinned git-for-windows MinGit; on unix it uses the system git. All args pass through.",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			git, err := Ensure(cmd.Context())
			if err != nil {
				return err
			}
			c := exec.Command(git, args...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if runtime.GOOS == "windows" {
				c.Env = appendGitWindowsEnv(os.Environ(), gitPromptsNonInteractive(
					os.Environ(),
					term.IsTerminal(int(os.Stdin.Fd())),
					term.IsTerminal(int(os.Stderr.Fd())),
				))
			}
			return runGitCommand(cmd.Context(), c)
		},
	}
}

func runGitCommand(ctx context.Context, c *exec.Cmd) error {
	configureGitCommand(c)
	if err := c.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		killGitProcessTree(c.Process.Pid)
		_ = c.Process.Kill()
		err := <-done
		if err != nil {
			return err
		}
		return ctx.Err()
	}
}

func appendGitWindowsEnv(env []string, nonInteractive bool) []string {
	if !nonInteractive {
		return env
	}
	if !hasEnv(env, "GIT_TERMINAL_PROMPT") {
		env = append(env, "GIT_TERMINAL_PROMPT=0")
	}
	if !hasEnv(env, "GCM_INTERACTIVE") {
		env = append(env, "GCM_INTERACTIVE=never")
	}
	return env
}

func gitPromptsNonInteractive(env []string, stdinTTY, stderrTTY bool) bool {
	if envTruthy(env, "BASHY_GIT_INTERACTIVE") {
		return false
	}
	if envTruthy(env, "BASHY_GIT_NONINTERACTIVE") || envTruthy(env, "BASHY_AGENTIC") {
		return true
	}
	return !stdinTTY || !stderrTTY
}

func envTruthy(env []string, name string) bool {
	v, ok := envValue(env, name)
	if !ok {
		return false
	}
	switch strings.ToLower(v) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

func envValue(env []string, name string) (string, bool) {
	prefix := name + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):], true
		}
	}
	return "", false
}

func hasEnv(env []string, name string) bool {
	prefix := name + "="
	for _, kv := range env {
		if len(kv) >= len(prefix) && kv[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
