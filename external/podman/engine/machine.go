// machine.go provides machine lifecycle management for ycode.
// On macOS/Windows, containers require a Linux VM. This module manages
// machine init/start/stop using podman's Go libraries directly —
// no external podman binary needed.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"go.podman.io/common/pkg/config"

	ociMachine "github.com/qiangli/yoke/pkg/oci/machine"

	gvproxyEmbed "github.com/qiangli/yoke/external/podman/engine/gvproxy_embed"
	vfkitEmbed "github.com/qiangli/yoke/external/podman/engine/vfkit_embed"
)

// defaultMachineImage is the OCI artifact endpoint upstream podman CLI
// uses when containers.conf doesn't override it. Mirrors the constant in
// go.podman.io/common/pkg/config so a stripped-down install without a
// containers.conf on disk still has somewhere to pull the VM image.
const defaultMachineImage = "docker://quay.io/podman/machine-os"

// resolveMachineImage picks an OCI endpoint for the machine VM image.
// Empty → upstream's containers.conf default → hardcoded fallback. The
// libpod shim treats "" as a fatal error (NewOCIArtifactPull rejects it),
// so this must always return a non-empty string when first-time init is
// possible.
func resolveMachineImage() string {
	if cfg, err := config.Default(); err == nil && cfg.Machine.Image != "" {
		return cfg.Machine.Image
	}
	return defaultMachineImage
}

// resolveMachineVolumes returns the list of host:guest mount specs that
// should be added at machine init time. Without these the VM can't see
// any path on the macOS host (or any path outside its rootfs on Linux),
// so docker-style bind mounts via `-v /Users/me/proj:/src` fail with
// `statfs ...: no such file or directory` from inside libpod.
//
// We reuse upstream's defaults via containers.conf (on darwin: /Users,
// /private, /var/folders — see go.podman.io/common/pkg/config's
// default_darwin.go) so behaviour matches the `podman machine init`
// CLI with no `-v` flags. `os.ExpandEnv` mirrors the same step upstream
// performs in cmd/podman/machine/init.go after binding the flag.
//
// Existing machines created without volumes won't grow them — upstream
// has no SetOptions field for Volumes — so this only takes effect on
// machines initialized after this code runs. Users with a pre-existing
// `ycode-default` machine need to `ycode podman machine rm` + re-init
// to pick up host bind-mount support.
func resolveMachineVolumes() []string {
	cfg, err := config.Default()
	if err != nil {
		return nil
	}
	vols := cfg.Machine.Volumes.Get()
	out := make([]string, 0, len(vols))
	for _, v := range vols {
		if v == "" {
			continue
		}
		out = append(out, os.ExpandEnv(v))
	}
	return out
}

// DefaultMachineName is the VM name used when none is supplied. Exposed
// as a const so name-only callers (start/stop/rm) don't pay the cost of
// probing host resources just to look up the default name.
//
// It is "bashy" — the dedicated, ISOLATED machine bashy/outpost own, distinct
// from ycode's `ycode-default` and any host machine, so the managed engine
// never collides with another podman on the same host ("always own").
const DefaultMachineName = "bashy"

// MachineConfig holds configuration for the Linux VM.
type MachineConfig struct {
	Name   string // VM name (default: DefaultMachineName)
	CPUs   int    // number of vCPUs (host-aware default via RecommendMachineSizing)
	Memory int    // memory in MB (host-aware default via RecommendMachineSizing)
	Disk   int    // disk size in GB (host-aware default via RecommendMachineSizing)

	// NoAutoCleanup disables the preflight's auto-cleanup retry. When
	// false (default), a preflight failure triggers CleanupHost (kill
	// orphaned vfkit/gvproxy zombies, remove stale sockets) and re-
	// runs the preflight before refusing. Set true to skip cleanup
	// and surface the original PreflightError immediately. Surfaced
	// via the `--no-auto-cleanup` CLI flag.
	NoAutoCleanup bool

	// Rootful selects which podman daemon the machine forwards its
	// API socket to:
	//   false (default) → /run/user/<UID>/podman/podman.sock — rootless;
	//                     OK for ordinary docker-compatible workloads.
	//   true            → /run/podman/podman.sock — rootful; required for
	//                     k3s-agent / kubelet-in-container that needs to
	//                     mkdir into /sys/fs/cgroup. The rootless variant
	//                     refuses with "permission denied" on cgroup
	//                     writes even under --privileged.
	// Surfaced via the `--rootful` CLI flag on `machine init`.
	Rootful bool

	// sizing is the host-resource snapshot RecommendMachineSizing used
	// to pick CPUs/Memory/Disk. Populated by DefaultMachineConfig only;
	// CLI overrides leave it at the zero value (DetectionOK=false) and
	// logs surface that as "cli-override".
	sizing SizingSource
}

// DefaultMachineConfig returns host-aware defaults sized as a fraction
// of available CPU / memory / free disk. See RecommendMachineSizing for
// the formulas. CLI flags (--cpus, --memory, --disk-size) bind on top
// of these defaults and continue to override.
func DefaultMachineConfig() MachineConfig {
	cpus, mem, disk, src := RecommendMachineSizing(DefaultProbe{}, machineDataDir())
	return MachineConfig{
		Name:   DefaultMachineName,
		CPUs:   cpus,
		Memory: mem,
		Disk:   disk,
		sizing: src,
	}
}

// EnsureMachine ensures a podman machine is initialized and running.
// Uses podman's Go libraries directly — no external binary needed.
func EnsureMachine(ctx context.Context, cfg MachineConfig) error {
	// Ensure vfkit helper is available (macOS VM hypervisor).
	ensureHelperBinariesOnPath()
	if err := ensurePlatformMachinePrereqs(ctx); err != nil {
		return err
	}

	// Get the platform's VM provider (AppleHV on macOS, QEMU on Linux, HyperV on Windows).
	mp, err := ociMachine.GetProvider()
	if err != nil {
		return fmt.Errorf("get machine provider: %w", err)
	}

	// Check if machine already exists and is running.
	mc, exists := findMachine(cfg.Name, mp)
	if exists {
		state, err := mp.State(mc, false)
		if err == nil && state == ociMachine.Running {
			slog.Info("container: machine already running", "name", cfg.Name)
			return nil
		}

		// Machine exists but not running — start it.
		slog.Info("container: starting machine", "name", cfg.Name)
		updateConn := true
		if err := ociMachine.Start(mc, mp, ociMachine.StartOptions{}, &updateConn); err != nil {
			return fmt.Errorf("machine start: %w", err)
		}
	} else {
		// Machine doesn't exist — init and start. Same preflight as
		// the explicit InitMachine path so accidental auto-provisions
		// (e.g., `ycode podman ps` against a wiped config) don't
		// silently allocate beyond what the host can serve. Honours
		// cfg.NoAutoCleanup for the cleanup-retry behavior.
		if err := PreflightAndCleanup(DefaultProbe{}, cfg, machineDataDir(), PreflightOptions{}, cfg.NoAutoCleanup); err != nil {
			return err
		}

		slog.Info("container: initializing machine (first-time setup, downloads ~800MB VM image)",
			append([]any{"name", cfg.Name, "cpus", cfg.CPUs, "memory_mb", cfg.Memory, "disk_gb", cfg.Disk},
				sizingLogAttrs(cfg.sizing)...)...)

		// See InitMachine for the rationale on Username — gvproxy
		// requires a non-empty value or it refuses to start.
		user := "core"
		if c, err := config.Default(); err == nil && c.Machine.User != "" {
			user = c.Machine.User
		}
		initOpts := ociMachine.InitOptions{
			Name:      cfg.Name,
			CPUS:      uint64(cfg.CPUs),
			Memory:    uint64(cfg.Memory),
			DiskSize:  uint64(cfg.Disk),
			IsDefault: true,
			Image:     resolveMachineImage(),
			Username:  user,
			Volumes:   resolveMachineVolumes(),
			Rootful:   cfg.Rootful,
		}

		if err := ociMachine.Init(initOpts, mp); err != nil {
			return fmt.Errorf("machine init: %w", err)
		}

		// Re-find the machine config after init.
		mc, exists = findMachine(cfg.Name, mp)
		if !exists {
			return fmt.Errorf("machine init succeeded but config not found")
		}

		slog.Info("container: starting machine", "name", cfg.Name)
		updateConn := true
		if err := ociMachine.Start(mc, mp, ociMachine.StartOptions{}, &updateConn); err != nil {
			return fmt.Errorf("machine start: %w", err)
		}
	}

	// Wait for socket to become available.
	for i := 0; i < 15; i++ {
		if socketPath := defaultSocketPath(); socketPath != "" {
			if err := waitForSocket(socketPath, 2*time.Second); err == nil {
				slog.Info("container: machine ready", "name", cfg.Name, "socket", socketPath)
				// One-shot: enable cpuset delegation in the VM's
				// systemd-user cgroup tree so containers can run
				// k3s (which hard-requires cpuset). Best-effort;
				// failure logs but doesn't gate machine readiness.
				if err := enableCpusetDelegation(cfg.Name); err != nil {
					slog.Warn("container: cpuset delegation setup failed (k3s in container won't start)", "err", err)
				}
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("machine started but socket not available after 30s")
}

// enableCpusetDelegation SSHes into the running podman machine and
// adds `cpuset` to each level of the systemd-user cgroup hierarchy's
// subtree_control. Fedora's systemd-user instance doesn't delegate
// cpuset by default (historical: cpuset was v1-only and required
// CAP_SYS_NICE); modern systemd CAN delegate it but only when asked.
//
// Without this, `cat /sys/fs/cgroup/cgroup.controllers` from inside
// a container shows `cpu io memory pids` — no cpuset — and any tool
// that requires it (k3s, kubelet, runc with cpu pinning) fails at
// startup with "failed to find cpuset cgroup (v2)".
//
// Idempotent: writing `+cpuset` to a cgroup that already has it is a
// no-op (linux kernel accepts the syscall and returns success).
// Runs once per machine-start; on a freshly-rebooted machine the
// hierarchy resets so we re-apply.
func enableCpusetDelegation(machineName string) error {
	mp, err := ociMachine.GetProvider()
	if err != nil {
		return fmt.Errorf("get provider: %w", err)
	}
	mc, exists := findMachine(machineName, mp)
	if !exists {
		return fmt.Errorf("machine %q not found", machineName)
	}
	identity := mc.SSH.IdentityPath
	port := mc.SSH.Port
	user := mc.SSH.RemoteUsername
	if identity == "" || port == 0 || user == "" {
		return fmt.Errorf("ssh details missing on machine %q", machineName)
	}
	cmd := exec.Command("ssh",
		"-i", identity,
		"-p", fmt.Sprintf("%d", port),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		fmt.Sprintf("%s@localhost", user),
		// One-shot script: walk the user-slice hierarchy, write
		// +cpuset to subtree_control at each level. sudo because
		// the writes need root.
		`for p in /user.slice /user.slice/user-501.slice /user.slice/user-501.slice/user@501.service /user.slice/user-501.slice/user@501.service/user.slice; do echo +cpuset | sudo tee /sys/fs/cgroup${p}/cgroup.subtree_control >/dev/null 2>&1 || true; done`,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	slog.Info("container: cpuset delegation enabled on machine", "name", machineName)
	return nil
}

// StopMachine stops the podman machine using Go libraries.
func StopMachine(ctx context.Context, name string) error {
	mp, err := ociMachine.GetProvider()
	if err != nil {
		return err
	}

	mc, exists := findMachine(name, mp)
	if !exists {
		return fmt.Errorf("machine %q not found", name)
	}

	return ociMachine.Stop(mc, mp, false)
}

// InitMachine creates and registers a new VM without starting it. Use
// StartMachine afterward (or EnsureMachine which does both).
func InitMachine(ctx context.Context, cfg MachineConfig) error {
	if err := ensurePlatformMachinePrereqs(ctx); err != nil {
		return err
	}

	// Preflight: refuse before we touch any state if the host doesn't
	// have enough memory or disk to run the VM as configured. Without
	// this, vfkit would happily allocate a 50 GB sparse image on a
	// 30 GB partition or a 4 GB VM on a swapping host, then OOM-kill
	// the user's first heavy build with no warning. Soft-fails on
	// unsupported platforms.
	//
	// PreflightAndCleanup attempts an offline cleanup (kill orphaned
	// vfkit/gvproxy zombies, remove stale sockets) on initial refusal
	// and retries the preflight. Disable with cfg.NoAutoCleanup.
	if err := PreflightAndCleanup(DefaultProbe{}, cfg, machineDataDir(), PreflightOptions{}, cfg.NoAutoCleanup); err != nil {
		return err
	}

	ensureHelperBinariesOnPath()

	mp, err := ociMachine.GetProvider()
	if err != nil {
		return fmt.Errorf("get machine provider: %w", err)
	}
	if _, exists := findMachine(cfg.Name, mp); exists {
		return fmt.Errorf("machine %q already exists", cfg.Name)
	}
	// Username must be set: upstream uses it as `mc.SSH.RemoteUsername`
	// AND as one of the four `-forward-*` flags passed to gvproxy. If any
	// of those four are empty, gvproxy refuses to start with "must all be
	// specified together, the same number of times, or not at all", which
	// surfaces here as "machine start: unable to connect to gvproxy
	// socket" after the wait-for-socket backoff times out. The Fedora
	// CoreOS machine image ships with a "core" account, matching
	// containers.conf's documented default.
	user := "core"
	if c, err := config.Default(); err == nil && c.Machine.User != "" {
		user = c.Machine.User
	}
	opts := ociMachine.InitOptions{
		Name:      cfg.Name,
		CPUS:      uint64(cfg.CPUs),
		Memory:    uint64(cfg.Memory),
		DiskSize:  uint64(cfg.Disk),
		IsDefault: true,
		Image:     resolveMachineImage(),
		Username:  user,
		Volumes:   resolveMachineVolumes(),
	}
	slog.Info("container: initializing machine (downloads ~800MB VM image on first run)",
		append([]any{"name", cfg.Name, "cpus", cfg.CPUs, "memory_mb", cfg.Memory, "disk_gb", cfg.Disk},
			sizingLogAttrs(cfg.sizing)...)...)
	if err := ociMachine.Init(opts, mp); err != nil {
		return fmt.Errorf("machine init: %w", err)
	}
	return nil
}

// StartMachine starts an existing machine and registers the user-facing
// podman connection. Returns nil immediately if the machine is already
// running.
func StartMachine(ctx context.Context, name string) error {
	ensureHelperBinariesOnPath()

	// Honor YCODE_LOG_LEVEL=debug for the upstream libpod logrus chain.
	// Without this the gvproxy commandline + vfkit/krunkit launch trace
	// stays hidden, which makes diagnosing "machine start: unable to
	// connect to gvproxy socket" effectively impossible.
	if strings.EqualFold(os.Getenv("YCODE_LOG_LEVEL"), "debug") {
		logrus.SetLevel(logrus.DebugLevel)
	}

	mp, err := ociMachine.GetProvider()
	if err != nil {
		return fmt.Errorf("get machine provider: %w", err)
	}
	mc, exists := findMachine(name, mp)
	if !exists {
		return fmt.Errorf("machine %q not found", name)
	}
	if state, err := mp.State(mc, false); err == nil && state == ociMachine.Running {
		return nil
	}
	updateConn := true
	if err := ociMachine.Start(mc, mp, ociMachine.StartOptions{}, &updateConn); err != nil {
		return fmt.Errorf("machine start: %w", err)
	}
	if socketPath := defaultSocketPath(); socketPath != "" {
		if err := waitForSocket(socketPath, 30*time.Second); err != nil {
			return fmt.Errorf("machine start: API socket not ready: %w", err)
		}
	}
	return nil
}

// RemoveMachine deletes a machine's config, image, and ignition files.
// Force=true skips the running-state check (the machine is stopped if
// needed). SaveImage/SaveIgnition retain those artifacts for reuse.
func RemoveMachine(ctx context.Context, name string, opts ociMachine.RemoveOptions) error {
	mp, err := ociMachine.GetProvider()
	if err != nil {
		return fmt.Errorf("get machine provider: %w", err)
	}
	mc, exists := findMachine(name, mp)
	if !exists {
		return fmt.Errorf("machine %q not found", name)
	}
	return ociMachine.Remove(mc, mp, opts)
}

// ResetMachines wipes ALL podman machine state across every provider on
// the current platform. Use this to recover from corruption where a
// machine config exists on disk but the system connection registry
// disagrees.
func ResetMachines(ctx context.Context) error {
	mp, err := ociMachine.GetProvider()
	if err != nil {
		return fmt.Errorf("get machine provider: %w", err)
	}
	return ociMachine.Reset([]ociMachine.VMProvider{mp}, ociMachine.ResetOptions{Force: true})
}

// ListMachines returns one entry per VM known to the current platform's
// provider — running or not.
func ListMachines(ctx context.Context) ([]*ociMachine.ListResponse, error) {
	mp, err := ociMachine.GetProvider()
	if err != nil {
		return nil, fmt.Errorf("get machine provider: %w", err)
	}
	return ociMachine.List([]ociMachine.VMProvider{mp}, ociMachine.ListOptions{})
}

// --- internal helpers ---

// ensureHelperBinariesOnPath extracts every embedded helper binary
// (vfkit, gvproxy) to the cache dir, prepends that dir to PATH (for
// helpers searched via PATH like vfkit), and exposes it through the
// CONTAINERS_HELPER_BINARY_DIR env var (for helpers searched only via
// upstream's helper_binaries_dir like gvproxy — see
// `c.Engine.HelperBinariesDir.Get()` in
// go.podman.io/common/pkg/config.FindHelperBinary).
//
// Doing both means users get a working podman machine with no manual
// containers.conf edits.
// isolateMachineDirs points podman's MACHINE registry (and config) at
// bashy-owned XDG dirs so bashy's VM lives entirely apart from any podman a
// user installed independently (brew, curl|sh). Without this, bashy's machine
// and the user's both live under ~/.local/share/containers/podman/machine and
// collide on podman's one-active-VM-per-provider guard (checkExclusiveActiveVM)
// — and it would look like bashy has to stop/replace the user's VM (a hostile,
// adoption-killing prompt). With separate registries, bashy provisions and runs
// its own VM invisibly; the user's `podman machine ls` never even shows it, and
// nothing of theirs is touched.
//
// podman's DataDirPrefix()/ConfDirPrefix() resolve via homedir.GetDataHome()
// /GetConfigHome(), which honour XDG_DATA_HOME / XDG_CONFIG_HOME. We set them
// unconditionally for bashy's own machine operations — this runs inside the
// dedicated `bashy podman`/`bashy dks` invocation, so overriding XDG here does
// not leak into the user's other tools. Idempotent.
func isolateMachineDirs() {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return
	}
	// A bashy-owned marker suffix so re-entrancy is a no-op and so the paths
	// are obviously ours in `ls`.
	os.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share", "bashy"))
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config", "bashy"))
}

func ensureHelperBinariesOnPath() {
	isolateMachineDirs()
	cacheDir := defaultBinCacheDir()

	vfkitOK := false
	if vfkitEmbed.Available() {
		if path, err := vfkitEmbed.EnsureVfkit(cacheDir); err != nil {
			slog.Warn("container: embedded vfkit extraction failed", "error", err)
		} else {
			slog.Info("container: using embedded vfkit", "path", path)
			vfkitOK = true
		}
	}
	if gvproxyEmbed.Available() {
		if path, err := gvproxyEmbed.EnsureGvproxy(cacheDir); err != nil {
			slog.Warn("container: embedded gvproxy extraction failed", "error", err)
		} else {
			slog.Info("container: using embedded gvproxy", "path", path)
		}
	}
	if err := ensurePlatformHelperBinaries(cacheDir); err != nil {
		slog.Warn("container: platform helper provisioning failed", "error", err)
	}

	// PATH-search (vfkit) — `FindHelperBinary(_, true)`.
	currentPath := os.Getenv("PATH")
	if !strings.Contains(currentPath, cacheDir) {
		os.Setenv("PATH", cacheDir+string(os.PathListSeparator)+currentPath)
	}
	// helper_binaries_dir-search (gvproxy) — `FindHelperBinary(_, false)`.
	// Upstream prepends CONTAINERS_HELPER_BINARY_DIR to the search list.
	if os.Getenv("CONTAINERS_HELPER_BINARY_DIR") == "" {
		os.Setenv("CONTAINERS_HELPER_BINARY_DIR", cacheDir)
	}

	// Provider selection: upstream's macOS default is libkrun, which
	// needs krunkit + libkrun.dylib. We only embed vfkit, so without an
	// override the start path fails with `exec: "krunkit": executable
	// file not found`. Force applehv when (a) vfkit was extracted
	// successfully and (b) the user hasn't already overridden — either
	// via containers.conf or CONTAINERS_MACHINE_PROVIDER directly.
	if vfkitOK && os.Getenv("CONTAINERS_MACHINE_PROVIDER") == "" {
		if cfg, err := config.Default(); err == nil && cfg.Machine.Provider != "" {
			// User pinned a provider in containers.conf — respect it.
		} else {
			os.Setenv("CONTAINERS_MACHINE_PROVIDER", "applehv")
		}
	}
}

// sizingLogAttrs builds the trailing key/value attrs for the init slog
// line so users see why a particular CPUs/Memory/Disk was chosen. When
// the source is zero-valued (e.g. CLI flag overrides built MachineConfig
// without going through DefaultMachineConfig), reports "cli-override".
func sizingLogAttrs(src SizingSource) []any {
	switch {
	case src.DetectionOK:
		return []any{
			"sizing", "host-aware",
			"host_cpus", src.HostCPUs,
			"host_total_mem_mb", src.HostTotalMemMB,
			"host_free_disk_gb", src.HostFreeDiskGB,
		}
	case src.HostCPUs == 0 && src.HostTotalMemMB == 0 && src.HostFreeDiskGB == 0:
		return []any{"sizing", "cli-override"}
	default:
		return []any{"sizing", "fallback-default", "host_cpus", src.HostCPUs}
	}
}

// machineDataDir returns the path Statfs should run against for the
// preflight disk check. We don't have a single canonical "machine data
// dir" — vfkit's disk image lives under
// ~/.local/share/containers/podman/machine/<provider>/ — so we point
// at the user's home directory; on every supported OS the home
// partition is the one Statfs reports free bytes for that path. Falls
// back to /tmp on the unusual case where home isn't resolvable.
func machineDataDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	return os.TempDir()
}

// findMachine looks up a machine config by name from the provider.
func findMachine(name string, mp ociMachine.VMProvider) (*ociMachine.MachineConfig, bool) {
	isolateMachineDirs()
	dirs, err := ociMachine.GetMachineDirs(mp.VMType())
	if err != nil {
		return nil, false
	}
	mc, err := ociMachine.LoadMachineByName(name, dirs)
	if err != nil {
		return nil, false
	}
	return mc, true
}
