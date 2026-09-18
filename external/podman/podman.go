// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package podman is a thin shell-out front-door to an externally installed
// podman binary. It is Layer 2 ("Sandbox tier") of the AgentOS substrate plan
// (see dhnt/docs/agentos-substrate-extraction-plan.md): deliberately NOT
// pure-Go and NOT self-embedded. Unlike ycode's historical in-process embedded
// engine, this neither links libpod nor ships a podman blob — it resolves an
// existing podman binary and execs it as a transparent pass-through.
//
// The fork ycode carried (github.com/qiangli/podman) existed only to embed
// podman in-process under the pure-Go constraint; once we exec a stock binary
// that reason is gone, so this consumes vanilla upstream podman with no patch.
//
// Pass-through is transparent: the caller's environment is inherited verbatim,
// including any CONTAINER_HOST/DOCKER_HOST pointing at a running engine. This
// package does not auto-wire any particular engine socket or manage a
// `podman machine` VM — that is the system podman's job (or the caller's env).
package podman

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// ErrNotFound is returned by Resolve when no podman binary can be located.
var ErrNotFound = errors.New("podman binary not found")

// Resolve locates a podman binary to exec, in priority order:
//  1. $PODMAN_BIN (explicit override).
//  2. "podman" on $PATH.
//  3. Well-known install locations, including a binary a sibling tool (e.g.
//     ycode) may have already fetched into the per-user cache.
//
// It returns ErrNotFound (wrapped) when nothing usable is present.
func Resolve() (string, error) {
	if p := os.Getenv("PODMAN_BIN"); p != "" {
		if isExecutable(p) {
			return p, nil
		}
		return "", fmt.Errorf("PODMAN_BIN=%q is not an executable file: %w", p, ErrNotFound)
	}
	if p, err := exec.LookPath("podman"); err == nil {
		return p, nil
	}
	for _, p := range candidatePaths() {
		if isExecutable(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w on PATH or in known locations; install podman or set PODMAN_BIN", ErrNotFound)
}

// candidatePaths lists fallback locations to probe when podman is not on PATH.
func candidatePaths() []string {
	var paths []string
	bin := podmanName()
	// A podman a sibling agent tool already fetched (per-user cache).
	if dir, err := os.UserCacheDir(); err == nil {
		paths = append(paths, filepath.Join(dir, "ycode", "bin", bin))
		paths = append(paths, filepath.Join(dir, "bashy", "bin", bin))
	}
	if runtime.GOOS != "windows" {
		paths = append(paths,
			"/opt/homebrew/bin/podman",
			"/usr/local/bin/podman",
			"/usr/bin/podman",
		)
	}
	return paths
}

func podmanName() string {
	if runtime.GOOS == "windows" {
		return "podman.exe"
	}
	return "podman"
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

// Run resolves podman and execs it with args as a transparent pass-through,
// wiring the given stdio and inheriting the process environment. It returns
// the child's exit code (127 if podman cannot be located, matching the shell
// "command not found" convention).
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	bin, err := Resolve()
	if err != nil {
		fmt.Fprintln(stderr, "bashy podman:", err)
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
		fmt.Fprintln(stderr, "bashy podman:", err)
		return 1
	}
	return 0
}
