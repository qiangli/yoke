// Package aws fronts the Amazon Web Services CLI v2 ("aws") as a managed
// external binary: resolved on the host, never compiled in. `bashy aws …`
// is a transparent passthrough — it ensures the aws binary, then execs it
// with your args.
//
// Admission (Sprint 457 story bedc320542eb): the AWS CLI v2 is Apache-2.0,
// distributed by Amazon as a per-platform vendor installer (not a
// single-binary GitHub release). It is a short-lived client under the
// service supply-chain policy (Rule 2): one bounded invocation, exits on
// completion, no listener or resident child (see ClientRuntime).
//
// Provisioning posture: host-first, like gcloud. The CLI is a large
// vendor distribution whose installer is the trusted, self-updating path,
// so this package prefers the host's own aws and Resolve refuses every
// version with an actionable vendor-install hint instead of
// TOFU-downloading. No sha256 pin is committed here; release selection
// plus pins are a named Sprint 465 gap (see EVIDENCE.md). Live AWS
// verification needs a real account, which this environment does not have;
// that stays a named unverified gap, never inferred support. A successful
// login never implies provisioning permission (see ProvisioningPermitted
// in auth.go).
package aws

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
	// License is the SPDX id of the AWS CLI v2.
	License = "Apache-2.0"
	// Source is the upstream distributor of record.
	Source = "Amazon Web Services CLI v2"
	// SourceURL is the human-checkable vendor page.
	SourceURL = "https://aws.amazon.com/cli/"
)

// InstallHint returns the vendor-recommended install path for the current OS.
func InstallHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "install the AWS CLI — `brew install awscli` or https://aws.amazon.com/cli/ (macOS installer)"
	case "windows":
		return "install the AWS CLI — https://aws.amazon.com/cli/ (Windows MSI installer), then reopen the shell"
	default:
		return "install the AWS CLI — https://aws.amazon.com/cli/ (or your distro package), then ensure `aws` is on PATH"
	}
}

// Resolve is the managed-provisioning entry point. Like gcloud.Resolve, it
// never auto-downloads: the AWS CLI is a large vendor distribution and
// there is no committed pin to verify against, so every version (including
// "") refuses with the actionable install hint. The host copy is used
// first (the registry entry sets PreferHost).
func Resolve(ctx context.Context, version string) (binmgr.Tool, error) {
	return binmgr.Tool{}, fmt.Errorf("aws not found on PATH — %s", InstallHint())
}

// Path returns the host aws binary path, or the actionable hint when absent.
func Path() (string, error) {
	if p, err := exec.LookPath("aws"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("aws not found on PATH — %s", InstallHint())
}

// Run execs the host aws with the given args, inheriting stdio — a
// transparent passthrough so `bashy aws …` works. Returns the child's exit
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

// Runtime describes the per-client runtime posture: aws runs as a
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

// NewAwsCmd builds the `aws` passthrough command (bashy front-door).
// All args pass through to aws. Credential selection stays explicit (see
// auth.go): this command never falls back to an ambient profile, and a
// successful login never implies provisioning permission.
func NewAwsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "aws",
		Short: "Run the AWS CLI v2 (managed external binary, Apache-2.0)",
		Long: `aws runs the Amazon Web Services CLI v2 (Apache-2.0) — the host's own
install (prefer-host; no auto-download). All args pass through to aws.

Authenticate explicitly per invocation (AWS_PROFILE plus a credential
source); there is no ambient-profile fallback, and login never implies
provisioning permission. EC2 and S3 mutations preview with the provider
--dry-run/--dryrun flag; other dry runs refuse without execution.`,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return Run(cmd.Context(), args)
		},
	}
}
