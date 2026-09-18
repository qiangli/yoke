// Package weave is the filesystem-based multi-agent workspace orchestrator
// re-homed from ycode into the AgentOS hub. It fans a queue of independent
// issues out to parallel agent CLIs (claude, codex, opencode, gemini, …),
// each in an isolated git-clone workspace, then converges with verification.
//
// The engine is pure-filesystem (a per-repo queue + git-clone workspaces
// under ~/.bashy/weave); it depends only on weavecli, cobra, and a PTY — no
// Gitea, no
// loom service, no agent-specific coupling. The AgentOS shell `bashy` mounts
// it as `bashy weave`; ycode's `ycode weave` is deprecated in favor of it.
package weave

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// NewWeaveCmd returns the `weave` cobra command tree — the host-agnostic
// entry point a front-end mounts (e.g. `bashy weave`). It Execute()s with
// the host's os.Stdin/Stdout/Stderr so the PTY-attached verbs (start, log,
// shell) drive a real terminal.
func NewWeaveCmd() *cobra.Command { return newWeaveCmd() }

// IsStructuredExit reports whether err is (or wraps) the *exitCodeError a
// weave subverb propagates after already emitting its own structured
// envelope or human-readable failure line. The root command sets
// SilenceErrors so cobra never double-prints on top of that output — but
// the same silence swallows genuinely unhandled errors cobra raises
// itself (an unknown subcommand, a bad flag) before any subverb ran and
// before anything was printed.
//
// A host driving NewWeaveCmd() (bashy's `case "weave"` dispatch) should
// call this on the error Execute() returns: false means nothing was
// printed yet and the host should surface err itself (see ExitCode);
// true means a subverb already reported the failure and the host must
// stay silent to avoid a double message.
func IsStructuredExit(err error) bool {
	var ec *exitCodeError
	return errors.As(err, &ec)
}

// ExitCode resolves the process exit code a host should use for the
// error Execute() returned: 0 for a nil err, the subverb's own stable
// code for a structured exit (see IsStructuredExit), and
// weavecli.ExitInvalidArg for anything else — cobra's own usage/
// structural errors (unknown subcommand, bad flag) are exit-2 usage
// failures by convention.
func ExitCode(err error) int {
	if err == nil {
		return weavecli.ExitOK
	}
	var ec *exitCodeError
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return weavecli.ExitInvalidArg
}
