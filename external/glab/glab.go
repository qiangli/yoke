// Package glab fronts the GitLab CLI (gitlab-org/cli, "glab") as a managed
// external binary: resolved on the host, never compiled in. `bashy glab …`
// is a transparent passthrough — it ensures the glab binary, then execs it
// with your args.
//
// Admission (Sprint 457 story bedc320542eb): glab is MIT, developed at
// gitlab.com/gitlab-org/cli. It is a short-lived client under the service
// supply-chain policy (Rule 2): one bounded invocation, exits on completion,
// no listener or resident child (see Runtime).
//
// Provisioning posture: host-first, like gcloud. glab publishes releases
// outside GitHub (gitlab.com releases), and bashy must not TOFU-download an
// unverified binary, so this package prefers the host's own glab and
// Resolve refuses every version with an actionable vendor-install hint
// instead of provisioning. No sha256 pin is committed here; release
// selection plus pins are a named Sprint 465 gap (see EVIDENCE.md).
// Live GitLab verification needs an authorized test account, which this
// environment does not have; that stays a named unverified gap, never
// inferred support. A successful login never implies provisioning
// permission (see ProvisioningPermitted in auth.go).
package glab

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/external/notrack"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// Admission facts. License and source are recorded for `bashy doctor`;
// download+exec != bundle, so nothing propagates.
const (
	// License is the SPDX id of gitlab-org/cli.
	License = "MIT"
	// Source is the upstream project of record (developed on GitLab, not GitHub).
	Source = "gitlab-org/cli"
	// SourceURL is the human-checkable project page.
	SourceURL = "https://gitlab.com/gitlab-org/cli"
	// HostDefault is the default API host when GITLAB_HOST is unset.
	HostDefault = "https://gitlab.com"
)

// InstallHint returns the vendor-recommended install path for the current OS.
func InstallHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "install glab — `brew install glab` or https://gitlab.com/gitlab-org/cli#installation"
	case "windows":
		return "install glab — `winget install glab` or https://gitlab.com/gitlab-org/cli#installation (Windows build), then reopen the shell"
	default:
		return "install glab — https://gitlab.com/gitlab-org/cli#installation (or your distro package), then ensure `glab` is on PATH"
	}
}

// Resolve is the managed-provisioning entry point. Like gcloud.Resolve, it
// never auto-downloads: glab releases live outside GitHub and there is no
// committed pin to verify against, so every version (including "") refuses
// with the actionable install hint. The host copy is used first (the
// registry entry sets PreferHost).
func Resolve(ctx context.Context, version string) (binmgr.Tool, error) {
	return binmgr.Tool{}, fmt.Errorf("glab not found on PATH — %s", InstallHint())
}

// Path returns the host glab binary path, or the actionable hint when absent.
func Path() (string, error) {
	if p, err := exec.LookPath("glab"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("glab not found on PATH — %s", InstallHint())
}

// Run execs the host glab with the given args, inheriting stdio — a
// transparent passthrough so `bashy glab …` works. Returns the child's exit
// code via the returned error (an executed foreign status passes through;
// it is never rewritten into a native exit).
func Run(ctx context.Context, args []string) error {
	bin, err := Path()
	if err != nil {
		return err
	}
	c := binmgr.Command(ctx, bin, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	c.Env = notrack.Env(os.Environ())
	return c.Run()
}

// ChildEnv returns env with tracking disabled.
func ChildEnv(env []string) []string { return notrack.Env(env) }

// Runtime describes the per-client runtime posture: glab runs as a
// short-lived child (one bounded invocation, exits on completion). No
// server, watch, bridge, or resident mode is admitted by this package.
type Runtime struct {
	Mode      string
	LongLived bool
	Note      string
}

// ClientRuntime reports the admitted runtime posture.
func ClientRuntime() Runtime {
	return Runtime{
		Mode:      "short-lived-client",
		LongLived: false,
		Note:      "one bounded invocation per command; no listener, watch, or resident child",
	}
}

// NewGlabCmd builds the `glab` passthrough command (bashy front-door).
// All args pass through to glab. Credential selection stays explicit (see
// auth.go): this command never falls back to an ambient account, and a
// successful login never implies provisioning permission.
func NewGlabCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "glab",
		Short: "Run the GitLab CLI glab (managed external binary, MIT)",
		Long: `glab runs the GitLab CLI (gitlab-org/cli, MIT) — the host's own install
(prefer-host; no auto-download). All args pass through to glab.

Authenticate explicitly per invocation (GITLAB_TOKEN or GITLAB_TOKEN_FILE);
there is no ambient-account fallback, and login never implies provisioning
permission. glab has no dry-run: previews refuse without execution.`,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return Run(cmd.Context(), args)
		},
	}
}
