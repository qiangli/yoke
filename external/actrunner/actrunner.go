// Package actrunner runs Gitea's act_runner (the GitHub-Actions-compatible CI
// runner) as a managed external binary (pkg/binmgr) — the CI executor for the
// dhnt mesh. The binary is downloaded → Sigstore-verified → sha256-verified →
// cached by binmgr, never compiled in. act_runner is the documented CI pick
// (docs/mesh-wrap-stack.md): it registers against loom/Gitea and dials OUT (so
// it's NAT-friendly over the mesh), executing .gitea/workflows/*.yml.
//
// Upstream gitea/runner is MIT; the bashy fork qiangli/act_runner is the
// signed supply-chain pin (builds/act_runner at the pin below).
// See dhnt/docs/local-p2p-cicd.md.
package actrunner

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/external/internal/serviceenv"
	"github.com/qiangli/yoke/pkg/binmgr"
)

const (
	// Upstream release that the bashy fork was built from (builds/act_runner
	// at the pinned commit). The fork tag is the upstream tag plus the bashy
	// build counter.
	actrunnerVersion = "v5.0.0"
	// Bashy-signed fork release tag (builds/act_runner/tool.env at the pin):
	// built by the bashy-build workflow on qiangli/act_runner and published as
	// raw executables with per-asset SHA256SUMS and <asset>.sigstore.json bundles.
	// Workflow run 38364503714 green: go1.27.2, 0 reachable findings.
	actrunnerBashyTag = "bashy-v5.0.0-1"

	// DefaultVersion is the pinned bashy tag. There is no "latest" — that was
	// the upstream archive and it is intentionally absent (fail closed).
	// Override via $ACT_RUNNER_VERSION / --version, within the aliases
	// resolveTool accepts.
	DefaultVersion = actrunnerBashyTag
	// DefaultLabels maps the `host` runs-on label to the HOST executor, so jobs
	// run directly on the host shell — the runner needs no container runtime of
	// its own. This is the tier-1 (userland) / build lane: an outpost/bashy Go
	// build wants the host's real Go toolchain, not a container.
	DefaultLabels = "host:host"

	// SandboxLabelName is the runs-on label for the tier-3 SANDBOX executor —
	// act_runner's docker (container) executor. `runs-on: sandbox` runs the job
	// inside an OCI container instead of on the host, so a job gets an identical
	// Linux toolchain on every host (the Windows host-executor's missing-git /
	// no-workspace failure mode disappears — the container carries git+node).
	SandboxLabelName = "sandbox"

	// DefaultSandboxImage is the OCI image the `sandbox` executor runs jobs in.
	// A full node image carries the Actions toolchain (node20 for `uses:` steps,
	// git for checkout, bash for `run:` steps) — the point of the sandbox tier.
	DefaultSandboxImage = "docker.io/library/node:20-bookworm"
)

// SandboxLabel returns the act_runner label mapping `sandbox` → the docker
// (container) executor on the given image (DefaultSandboxImage when empty).
func SandboxLabel(image string) string {
	if strings.TrimSpace(image) == "" {
		image = DefaultSandboxImage
	}
	return SandboxLabelName + ":docker://" + image
}

// SHA256 per asset read from the published SHA256SUMS of the bashy release
// (downloaded via curl -sL, not from the GitHub API asset digest).
var actrunnerSHAs = map[string]string{
	"linux/amd64":  "1e2801abb2f6fd234eddd9feba78d80c299b276d12677b5bbfc3dcd96974bd93",
	"linux/arm64":  "93f33200df0e671aa25d172a8789c5ec7c4113f70e48276fa1caf7b749503ca4",
	"darwin/amd64": "44799c27616cdec00b1034e8af974d251707221c7233422555cbc897cedfbd65",
	"darwin/arm64": "70373f454aa449218daf04c03ff32f0c459cb90733ac572eb86f2b1fdbfb9bfb",
}

func actrunnerTool() binmgr.Tool {
	assets := map[string]binmgr.Asset{}
	for plat, sha := range actrunnerSHAs {
		goos, goarch, _ := strings.Cut(plat, "/")
		assetName := "act_runner-" + goos + "-" + goarch
		url := fmt.Sprintf("https://github.com/qiangli/act_runner/releases/download/%s/%s", actrunnerBashyTag, assetName)
		assets[plat] = binmgr.Asset{URL: url, SHA256: sha}
	}
	identity := fmt.Sprintf("https://github.com/qiangli/act_runner/.github/workflows/bashy-build.yml@refs/tags/%s", actrunnerBashyTag)
	return binmgr.Tool{
		Name:    "act_runner",
		Version: actrunnerBashyTag,
		Assets:  assets,
		Sigstore: &binmgr.Sigstore{
			Identity: identity,
			Issuer:   binmgr.GitHubActionsIssuer,
		},
	}
}

// ActrunnerTool is the pinned, Sigstore-verified tool declaration. Exported
// for tests and for callers that need to inspect the pin without launching.
var ActrunnerTool = actrunnerTool()

// Spec is retained for compatibility with callers that probe the resolver's
// asset matcher. It returns a GitHubSpec for the qiangli fork, not the
// upstream dl.gitea.com archive, and never returns "latest".
func Spec(version string) binmgr.GitHubSpec {
	if version == "" {
		version = DefaultVersion
	}
	// Normalize the upstream tag to the bashy tag so "v5.0.0" still resolves
	// to the signed fork without opening an upstream path.
	if version == actrunnerVersion {
		version = actrunnerBashyTag
	}
	return binmgr.GitHubSpec{Name: "act_runner", Repo: "qiangli/act_runner", Version: version, AssetMatch: assetMatch}
}

// assetMatch picks the raw `act_runner-<goos>-<goarch>` executable published by
// the bashy fork. The dl.gitea.com archive was
// `act_runner-<version>-<goos>-<arch>` with a .sha256 sidecar; the fork
// publishes only raw executables plus <asset>.sigstore.json bundles.
func assetMatch(name, goos, goarch string) bool {
	return strings.EqualFold(name, "act_runner-"+goos+"-"+goarch)
}

func resolveTool(version string) (binmgr.Tool, error) {
	if runtime.GOOS == "windows" {
		return binmgr.Tool{}, fmt.Errorf("act_runner: supply-chain policy: no bashy-signed asset for windows (no upstream fallback) — only linux and darwin assets are signed")
	}
	tool := ActrunnerTool
	// Accept empty, the bashy tag, or the upstream version as aliases for the pinned tool.
	// "latest" and any other version are refused — there is no upstream archive fallback.
	switch version {
	case "", actrunnerBashyTag, actrunnerVersion:
		return tool, nil
	default:
		return binmgr.Tool{}, fmt.Errorf("act_runner: version %q not supported: only %q (bashy-signed) is available (upstream %q alias accepted)", version, actrunnerBashyTag, actrunnerVersion)
	}
}

// DefaultDataDir is act_runner's work dir (its .runner registration + state):
// $ACT_RUNNER_DIR or ~/.agents/bashy/act_runner.
func DefaultDataDir() string {
	if d := strings.TrimSpace(os.Getenv("ACT_RUNNER_DIR")); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "act_runner")
	}
	return filepath.Join(home, ".agents", "bashy", "act_runner")
}

// RegisterOptions configures a one-shot registration against a Gitea instance.
type RegisterOptions struct {
	Version  string
	DataDir  string
	Instance string // Gitea base URL (e.g. the loopback addr from `mesh dial git`)
	Token    string // runner registration token (minted in Gitea)
	Name     string // runner name (default: hostname)
	Labels   string // executor labels (default DefaultLabels)

	// Sandbox appends the tier-3 `sandbox` docker-executor label (on
	// SandboxImage, or DefaultSandboxImage) to Labels, so one runner offers
	// BOTH the host build lane (`runs-on: host`) and the container lane
	// (`runs-on: sandbox`). The job's runs-on picks; a container job needs a
	// reachable DOCKER_HOST at daemon time (see Daemon / --docker-host).
	Sandbox      bool
	SandboxImage string
}

func (o *RegisterOptions) defaults() {
	if o.Version == "" {
		o.Version = strings.TrimSpace(os.Getenv("ACT_RUNNER_VERSION"))
	}
	if o.DataDir == "" {
		o.DataDir = DefaultDataDir()
	}
	if o.Labels == "" {
		o.Labels = DefaultLabels
	}
	if o.Sandbox {
		if !strings.Contains(o.Labels, SandboxLabelName+":") {
			o.Labels = strings.TrimSuffix(o.Labels, ",") + "," + SandboxLabel(o.SandboxImage)
		}
	}
	if o.Name == "" {
		if h, err := os.Hostname(); err == nil {
			o.Name = h
		}
	}
}

// Register ensures the binary and runs `act_runner register --no-interactive`,
// writing the .runner config into the data dir. Idempotent only insofar as Gitea
// allows re-registration; callers typically guard on the .runner file existing.
func Register(ctx context.Context, o RegisterOptions) error {
	o.defaults()
	if o.Instance == "" || o.Token == "" {
		return fmt.Errorf("actrunner: register needs --instance and --token")
	}
	if err := os.MkdirAll(o.DataDir, 0o755); err != nil {
		return err
	}
	bin, err := resolve(ctx, o.Version)
	if err != nil {
		return err
	}
	args := []string{
		"register", "--no-interactive",
		"--instance", o.Instance,
		"--token", o.Token,
		"--labels", o.Labels,
	}
	if o.Name != "" {
		args = append(args, "--name", o.Name)
	}
	if o.Sandbox {
		// Persist the sandbox executor config next to the .runner so the daemon
		// picks it up automatically (docker_host "-" — see sandboxConfigYAML).
		if err := writeSandboxConfig(o.DataDir); err != nil {
			return err
		}
	}
	cmd := binmgr.Command(ctx, bin, args...)
	cmd.Dir = o.DataDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	// act_runner has no update/analytics switch and no native OTLP exporter;
	// still pin every standard OTLP endpoint so jobs cannot inherit a vendor
	// destination from the launching shell.
	cmd.Env = managedEnv()
	return cmd.Run()
}

// sandboxConfigYAML is the act_runner config the sandbox executor needs.
// The load-bearing line is `container.docker_host: "-"`: it tells act_runner to
// connect to the container engine via the DOCKER_HOST env (bashy podman's
// socket) but NOT bind-mount that socket into the job container. With a
// podman-machine VM backend the host socket path doesn't exist inside the VM,
// so the default socket-mount makes `docker create` fail — this is exactly the
// failure that surfaced proving the tier on macOS. Harmless on a native-podman
// Linux host too (jobs just don't get docker-in-docker, which they don't need).
const sandboxConfigYAML = `# generated by ` + "`bashy act-runner register --sandbox`" + ` — tier-3 sandbox executor.
log:
  level: info
runner:
  capacity: 1
container:
  docker_host: "-"
  force_pull: false
`

// ConfigPath is the act_runner config file the sandbox executor uses, in the
// data dir. Daemon auto-passes --config when this file exists.
func ConfigPath(dataDir string) string {
	if dataDir == "" {
		dataDir = DefaultDataDir()
	}
	return filepath.Join(dataDir, "config.yaml")
}

// writeSandboxConfig writes sandboxConfigYAML into the data dir (idempotent).
func writeSandboxConfig(dataDir string) error {
	return os.WriteFile(ConfigPath(dataDir), []byte(sandboxConfigYAML), 0o644)
}

// EnsureSandboxConfig writes the sandbox executor config into the data dir
// (idempotent). Register --sandbox already writes it; callers that enable
// sandbox mode on an ALREADY-registered runner (which skips Register) call this
// before Daemon so the docker_host "-" setting takes effect.
func EnsureSandboxConfig(dataDir string) error {
	if dataDir == "" {
		dataDir = DefaultDataDir()
	}
	return writeSandboxConfig(dataDir)
}

// Registered reports whether a .runner config already exists in the data dir.
func Registered(dataDir string) bool {
	if dataDir == "" {
		dataDir = DefaultDataDir()
	}
	_, err := os.Stat(filepath.Join(dataDir, ".runner"))
	return err == nil
}

// Daemon runs `act_runner daemon` in the foreground from the data dir, blocking
// until the context is cancelled or SIGINT/SIGTERM arrives. The daemon dials OUT
// to the registered Gitea instance — no inbound port, mesh/NAT-friendly. extraEnv
// (may be nil) is appended to the inherited environment — e.g. OTEL_* so the CI
// runner + the deploy jobs it launches self-export into the caller's telemetry
// plane under their own service.name.
func Daemon(ctx context.Context, version, dataDir string, extraEnv ...string) error {
	if dataDir == "" {
		dataDir = DefaultDataDir()
	}
	if !Registered(dataDir) {
		return fmt.Errorf("actrunner: not registered (no %s/.runner) — run `register` first", dataDir)
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	bin, err := resolve(ctx, version)
	if err != nil {
		return err
	}
	daemonArgs := []string{"daemon"}
	if _, err := os.Stat(ConfigPath(dataDir)); err == nil {
		// A sandbox executor config was written at register time — use it, so
		// the docker_host "-" setting takes effect (required for podman backends).
		daemonArgs = append(daemonArgs, "--config", ConfigPath(dataDir))
	}
	cmd := binmgr.Command(ctx, bin, daemonArgs...)
	cmd.Dir = dataDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = managedEnv(extraEnv...)
	return cmd.Run()
}

func managedEnv(extra ...string) []string {
	return serviceenv.LocalOTLP(append(os.Environ(), extra...), "act_runner")
}

func resolve(ctx context.Context, version string) (string, error) {
	tool, err := resolveTool(version)
	if err != nil {
		return "", err
	}
	return binmgr.Ensure(ctx, tool)
}

// NewCmd builds the `act-runner` command tree (bashy front-door + any cobra host).
func NewCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "act-runner",
		Short: "Run Gitea act_runner (the mesh CI executor) as a managed external binary",
		Long: `act-runner runs Gitea's act_runner — downloaded, sha256-verified, and cached
by binmgr (not compiled in). Register it against loom/Gitea, then run the daemon;
it dials OUT to Gitea so it works over the mesh behind NAT. See
dhnt/docs/local-p2p-cicd.md.`,
	}

	var ro RegisterOptions
	register := &cobra.Command{
		Use:   "register",
		Short: "Register this runner against a Gitea instance (writes .runner)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return Register(cmd.Context(), ro)
		},
	}
	register.Flags().StringVar(&ro.Version, "version", "", "act_runner version (default: pinned)")
	register.Flags().StringVar(&ro.DataDir, "data", "", "work dir (default ~/.agents/bashy/act_runner)")
	register.Flags().StringVar(&ro.Instance, "instance", "", "Gitea base URL (e.g. from `outpost mesh dial git`)")
	register.Flags().StringVar(&ro.Token, "token", "", "runner registration token (minted in Gitea)")
	register.Flags().StringVar(&ro.Name, "name", "", "runner name (default: hostname)")
	register.Flags().StringVar(&ro.Labels, "labels", DefaultLabels, "executor labels")
	register.Flags().BoolVar(&ro.Sandbox, "sandbox", false, "also offer the tier-3 `sandbox` docker executor (runs-on: sandbox → OCI container)")
	register.Flags().StringVar(&ro.SandboxImage, "sandbox-image", "", "OCI image for the sandbox executor (default "+DefaultSandboxImage+")")

	var dVersion, dData, dDockerHost string
	daemon := &cobra.Command{
		Use:   "daemon",
		Short: "Run the act_runner daemon (foreground; dials out to Gitea)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var extra []string
			if strings.TrimSpace(dDockerHost) != "" {
				// The sandbox executor's container runtime — point it at
				// `bashy podman`'s host-side socket so tier-3 jobs run in
				// bashy-owned containers (no host Docker Desktop required).
				extra = append(extra, "DOCKER_HOST="+dDockerHost)
			}
			return Daemon(cmd.Context(), dVersion, dData, extra...)
		},
	}
	daemon.Flags().StringVar(&dVersion, "version", "", "act_runner version (default: pinned)")
	daemon.Flags().StringVar(&dData, "data", "", "work dir (default ~/.agents/bashy/act_runner)")
	daemon.Flags().StringVar(&dDockerHost, "docker-host", "", "DOCKER_HOST for the sandbox executor (e.g. bashy podman's socket)")

	var pVersion string
	pathCmd := &cobra.Command{
		Use:   "path",
		Short: "Print the cached act_runner binary path (fetching it if needed)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := resolve(cmd.Context(), pVersion)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t(act_runner %s)\n", p, func() string {
				if pVersion == "" {
					return DefaultVersion
				}
				return pVersion
			}())
			return nil
		},
	}
	pathCmd.Flags().StringVar(&pVersion, "version", "", "act_runner version (default: pinned)")

	root.AddCommand(register, daemon, pathCmd)
	return root
}
