// Package az fronts the Microsoft Azure CLI ("az") as a managed external
// binary: resolved on the host, never compiled in. `bashy az …` is a
// transparent passthrough — it ensures the az binary, then execs it with
// your args.
//
// Admission (Sprint 457 story bedc320542eb): the Azure CLI is MIT
// (Azure/azure-cli), distributed by Microsoft as a per-platform vendor
// installer (not a single-binary GitHub release). It is a short-lived
// client under the service supply-chain policy (Rule 2): one bounded
// invocation, exits on completion, no listener or resident child (see
// ClientRuntime).
//
// Provisioning posture: host-first, like gcloud. The CLI's vendor
// installer is the trusted, self-updating path, so this package prefers
// the host's own az and Resolve refuses every version with an actionable
// vendor-install hint instead of TOFU-downloading. No sha256 pin is
// committed here; release selection plus pins are a named Sprint 465 gap
// (see EVIDENCE.md). Live Azure verification needs a real subscription,
// which this environment does not have; that stays a named unverified gap,
// never inferred support. A successful login never implies provisioning
// permission (see ProvisioningPermitted in auth.go).
package az

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
	// License is the SPDX id of the Azure CLI.
	License = "MIT"
	// Source is the upstream project of record.
	Source = "Azure/azure-cli"
	// SourceURL is the human-checkable vendor page.
	SourceURL = "https://learn.microsoft.com/cli/azure/"
)

// InstallHint returns the vendor-recommended install path for the current OS.
func InstallHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "install the Azure CLI — `brew install azure-cli` or https://learn.microsoft.com/cli/azure/install-azure-cli-macos"
	case "windows":
		return "install the Azure CLI — https://learn.microsoft.com/cli/azure/install-azure-cli-windows (MSI installer), then reopen the shell"
	default:
		return "install the Azure CLI — https://learn.microsoft.com/cli/azure/install-azure-cli-linux (or your distro package), then ensure `az` is on PATH"
	}
}

// Resolve is the managed-provisioning entry point. Like gcloud.Resolve, it
// never auto-downloads: the Azure CLI is a vendor distribution and there
// is no committed pin to verify against, so every version (including "")
// refuses with the actionable install hint. The host copy is used first
// (the registry entry sets PreferHost).
func Resolve(ctx context.Context, version string) (binmgr.Tool, error) {
	return binmgr.Tool{}, fmt.Errorf("az not found on PATH — %s", InstallHint())
}

// Path returns the host az binary path, or the actionable hint when absent.
func Path() (string, error) {
	if p, err := exec.LookPath("az"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("az not found on PATH — %s", InstallHint())
}

// Run execs the host az with the given args, inheriting stdio — a
// transparent passthrough so `bashy az …` works. Returns the child's exit
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

// Runtime describes the per-client runtime posture: az runs as a
// short-lived child (one bounded invocation, exits on completion). No
// server, daemon, or resident mode is admitted by this package.
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
		Note:      "one bounded invocation per command; no daemon, server, or resident child",
	}
}

// NewAzCmd builds the `az` passthrough command (bashy front-door).
// All args pass through to az. Credential selection stays explicit (see
// auth.go): this command never falls back to an ambient subscription, and
// a successful login never implies provisioning permission.
func NewAzCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "az",
		Short: "Run the Azure CLI (managed external binary, MIT)",
		Long: `az runs the Microsoft Azure CLI (Azure/azure-cli, MIT) — the host's own
install (prefer-host; no auto-download). All args pass through to az.

Authenticate explicitly per invocation (AZURE_SUBSCRIPTION_ID plus a
credential source); there is no ambient-subscription fallback, and login
never implies provisioning permission. ARM deployment mutations preview
with --what-if; other dry runs refuse without execution.`,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return Run(cmd.Context(), args)
		},
	}
}
