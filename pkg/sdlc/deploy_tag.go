package sdlc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// Version-tag idempotency for the deploy baton. A change is deployed to an env
// under a version tag — v0.0.1-dev, v0.0.1-qa, v0.0.1 (prod, no suffix). The
// deploy is a no-op when that tag already exists on the remote, so flipping the
// deploy:<env> label back and forth (or an accidental relabel) never re-deploys,
// and prod only ever ships a version that already exists — never un-qa'd churn.
// The version is declared once in the deploy DAG (a `version` var) and passed to
// `bashy sdlc deploy-once`, which guards the actual deploy command.

// deployTag is the per-env tag: prod (or empty env) is the bare version; every
// other env suffixes it. v0.0.1 + qa -> v0.0.1-qa; v0.0.1 + prod -> v0.0.1.
func deployTag(version, env string) string {
	version = strings.TrimSpace(version)
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "", "prod", "production":
		return version
	default:
		return version + "-" + strings.ToLower(strings.TrimSpace(env))
	}
}

type DeployOnceOptions struct {
	Version string   // e.g. v0.0.1 (usually a ${version} var from the deploy DAG)
	Env     string   // dev|qa|prod
	Remote  string   // git remote holding the deploy tags (default: origin)
	Ref     string   // commit to tag on success (default: HEAD)
	Cwd     string   // working dir
	Command []string // the deploy command to run when not already deployed
	DryRun  bool
	JSON    bool
}

type DeployOnceResult struct {
	SchemaVersion string `json:"schema_version"`
	Status        string `json:"status"` // deployed | already-deployed | dry-run | error
	Tag           string `json:"tag"`
	Env           string `json:"env"`
	Output        string `json:"output,omitempty"`
}

// DeployOnce runs the deploy command ONLY if the env's version tag isn't already
// on the remote, then tags + pushes on success — making the deploy idempotent
// and version-gated. Returns already-deployed (a no-op) when the tag exists.
func DeployOnce(ctx context.Context, opt DeployOnceOptions) (DeployOnceResult, error) {
	if strings.TrimSpace(opt.Version) == "" {
		return DeployOnceResult{Status: "error"}, errors.New("sdlc: deploy-once requires --version (e.g. v0.0.1, usually the deploy DAG's ${version})")
	}
	remote := strings.TrimSpace(opt.Remote)
	if remote == "" {
		remote = "origin"
	}
	ref := strings.TrimSpace(opt.Ref)
	if ref == "" {
		ref = "HEAD"
	}
	cwd := strings.TrimSpace(opt.Cwd)
	if cwd == "" {
		var err error
		if cwd, err = os.Getwd(); err != nil {
			return DeployOnceResult{Status: "error"}, err
		}
	}
	tag := deployTag(opt.Version, opt.Env)
	res := DeployOnceResult{SchemaVersion: schemaVersion, Status: "error", Tag: tag, Env: strings.ToLower(strings.TrimSpace(opt.Env))}

	// The remote tag is the cross-host source of truth for "is this version
	// already deployed to this env" — a tag pushed by one env host is visible to
	// every other.
	deployed, err := remoteTagExists(ctx, cwd, remote, tag)
	if err != nil {
		return res, err
	}
	if deployed {
		res.Status = "already-deployed"
		return res, nil
	}
	if opt.DryRun {
		res.Status = "dry-run"
		return res, nil
	}
	if len(opt.Command) == 0 {
		return res, errors.New("sdlc: deploy-once needs a deploy command after `--`")
	}
	// Run the actual deploy (inherits env → host-vault creds).
	cmd := exec.CommandContext(ctx, opt.Command[0], opt.Command[1:]...)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	res.Output = string(out)
	if err != nil {
		return res, fmt.Errorf("deploy command failed: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	// Record success: tag the deployed commit and push it, so a re-fire is a no-op.
	if terr := runGitErr(ctx, cwd, "tag", "-f", tag, ref); terr != nil {
		return res, fmt.Errorf("tag %s: %w", tag, terr)
	}
	if perr := runGitErr(ctx, cwd, "push", "-f", remote, "refs/tags/"+tag); perr != nil {
		return res, fmt.Errorf("push tag %s: %w", tag, perr)
	}
	res.Status = "deployed"
	return res, nil
}

func remoteTagExists(ctx context.Context, cwd, remote, tag string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--tags", remote, "refs/tags/"+tag)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("git ls-remote %s %s: %w\n%s", remote, tag, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func newDeployOnceCmd() *cobra.Command {
	var opt DeployOnceOptions
	cmd := &cobra.Command{
		Use:   "deploy-once --version V --env ENV -- COMMAND...",
		Short: "deploy only if the env's version tag isn't already deployed (idempotent; a label flip can't re-deploy)",
		RunE: func(cmd *cobra.Command, args []string) error {
			opt.Command = args
			res, err := DeployOnce(cmd.Context(), opt)
			if opt.JSON || os.Getenv("BASHY_AGENTIC") != "" {
				b, _ := json.Marshal(res)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else if err == nil {
				switch res.Status {
				case "already-deployed":
					fmt.Fprintf(cmd.OutOrStdout(), "skip: %s already deployed\n", res.Tag)
				case "deployed":
					fmt.Fprintf(cmd.OutOrStdout(), "deployed %s\n", res.Tag)
				case "dry-run":
					fmt.Fprintf(cmd.OutOrStdout(), "dry-run: would deploy %s\n", res.Tag)
				}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&opt.Version, "version", "", "release version, e.g. v0.0.1 (usually the deploy DAG's ${version})")
	cmd.Flags().StringVar(&opt.Env, "env", "", "target environment (dev|qa|prod); prod uses the bare version tag")
	cmd.Flags().StringVar(&opt.Remote, "remote", "origin", "git remote holding the deploy tags")
	cmd.Flags().StringVar(&opt.Ref, "ref", "HEAD", "commit to tag on a successful deploy")
	cmd.Flags().StringVar(&opt.Cwd, "cwd", "", "working directory")
	cmd.Flags().BoolVar(&opt.DryRun, "dry-run", false, "report the decision without deploying")
	cmd.Flags().BoolVar(&opt.JSON, "json", false, "print JSON")
	return cmd
}
