package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	ociMachine "github.com/qiangli/yoke/pkg/oci/machine"

	podman_embed "github.com/qiangli/yoke/external/podman/engine/podman_embed"
)

// NewPodmanCmd is the `bashy podman` front-door: a thin pass-through to Podman,
// preferring the embedded binary, then a cached or managed Podman.
// CONTAINER_HOST auto-points at the ISOLATED `bashy`
// machine's socket when present so images + containers land in bashy's own
// store. bashy does NOT reimplement podman's verbs; every upstream flag works.
// $BASHY_PODMAN_SYSTEM=1 forces a podman on $PATH.
func NewPodmanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "podman [ARGS...]",
		Aliases: []string{"docker"},
		Short:   "Container management — pass-through to embedded or managed Podman",
		Long: `Thin pass-through to Podman. Every upstream verb/flag/output works — bashy
does not reimplement them.

Resolution order:
  1. embedded Podman, when this bashy build includes it
  2. cached Podman engine, or a managed Podman remote client

CONTAINER_HOST is auto-set to bashy's own isolated machine socket when present,
so containers and images land in bashy's store, distinct from any host or ycode
podman.

  machine   Manage bashy's embedded vfkit VM (isolated; name "` + DefaultMachineName + `")

$BASHY_PODMAN_SYSTEM=1 forces a podman on $PATH.`,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return ExecManaged(cmd.Context(), args)
		},
	}
	cmd.AddCommand(newPodmanMachineCmd())
	return cmd
}

// systemPodman reports whether the caller opted out of the embedded binary back
// to a host podman on $PATH ($BASHY_PODMAN_SYSTEM truthy).
func systemPodman() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BASHY_PODMAN_SYSTEM"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// ExecManaged resolves the podman binary and execs it with args, pointed at
// bashy's isolated machine socket via CONTAINER_HOST when one exists. The
// child's exit code propagates via os.Exit so CI scripts see failures.
func ExecManaged(ctx context.Context, args []string) error {
	if shouldEnsureMachine(args) {
		if err := EnsureMachine(ctx, DefaultMachineConfig()); err != nil {
			return err
		}
	}
	bin, err := resolvePodmanBinary()
	if err != nil {
		return err
	}
	cmd := buildManagedExec(ctx, bin, DefaultSocketPath(), args)
	if runErr := cmd.Run(); runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("podman: %w", runErr)
	}
	return nil
}

func shouldEnsureMachine(args []string) bool {
	if len(args) == 0 {
		return true
	}
	for _, arg := range args {
		switch arg {
		case "--":
			return true
		case "-h", "--help", "--version", "help", "machine", "version", "system":
			return false
		}
		if !strings.HasPrefix(arg, "-") {
			return true
		}
	}
	return true
}

// buildManagedExec builds the exec.Cmd shipping args to the resolved podman,
// with CONTAINER_HOST set to the isolated machine socket. Split out for testing.
func buildManagedExec(ctx context.Context, bin, socket string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	env := os.Environ()
	if socket != "" {
		env = append(env, "CONTAINER_HOST=unix://"+socket)
	}
	if dir := filepath.Dir(bin); dir != "." && dir != "" {
		env = prependPath(env, dir)
	}
	if runtime.GOOS == "linux" {
		if helperDir := filepath.Clean(filepath.Join(filepath.Dir(bin), "..", "lib", "podman")); isDir(helperDir) {
			if os.Getenv("CONTAINERS_CONF_OVERRIDE") == "" {
				if conf, err := writeHelperConf(filepath.Dir(bin), helperDir); err == nil {
					env = append(env, "CONTAINERS_CONF_OVERRIDE="+conf)
				}
			}
		}
	}
	cmd.Env = env
	return cmd
}

// writeHelperConf writes (idempotently) a containers.conf override that points
// conmon, the helper dir and the OCI runtimes at the files shipped in the same
// engine tarball; CONTAINERS_HELPER_BINARY_DIR alone does not fix conmon lookup.
func writeHelperConf(binDir, helperDir string) (string, error) {
	var b strings.Builder
	b.WriteString("[engine]\n")
	if conmon := filepath.Join(helperDir, "conmon"); isFile(conmon) {
		fmt.Fprintf(&b, "conmon_path = [%q]\n", conmon)
	}
	fmt.Fprintf(&b, "helper_binaries_dir = [%q]\n", helperDir)
	var rt []string
	for _, name := range []string{"crun", "runc"} {
		var found string
		for _, d := range []string{binDir, helperDir} {
			if p := filepath.Join(d, name); isFile(p) {
				found = p
				break
			}
		}
		if found != "" {
			rt = append(rt, fmt.Sprintf("%s = [%q]\n", name, found))
		}
	}
	if len(rt) > 0 {
		b.WriteString("\n[engine.runtimes]\n")
		for _, l := range rt {
			b.WriteString(l)
		}
	}
	dir := filepath.Join(filepath.Dir(defaultBinCacheDir()), "podman-conf")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(b.String()))
	path := filepath.Join(dir, "containers-"+hex.EncodeToString(sum[:6])+".conf")
	if old, err := os.ReadFile(path); err == nil && string(old) == b.String() {
		return path, nil
	}
	tmp, err := os.CreateTemp(dir, ".conf-*")
	if err != nil {
		return "", err
	}
	_, werr := tmp.WriteString(b.String())
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return "", werr
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return path, nil
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// safePathPodman refuses a PATH entry that returns to this bashy process.
// Shell wrappers are common in local installations, so inspect their small
// text bodies as well as symlinks to the current executable.
func safePathPodman() (string, error) {
	bin, err := exec.LookPath("podman")
	if err != nil {
		return "", err
	}
	self, err := os.Executable()
	if err == nil {
		selfInfo, selfErr := os.Stat(self)
		binInfo, binErr := os.Stat(bin)
		if selfErr == nil && binErr == nil && os.SameFile(selfInfo, binInfo) {
			return "", fmt.Errorf("podman on PATH resolves to bashy")
		}
	}
	f, err := os.Open(bin)
	if err == nil {
		defer f.Close()
		buf := make([]byte, 65536)
		n, _ := f.Read(buf)
		if n > 0 && buf[0] == '#' && strings.Contains(string(buf[:n]), "bashy") && strings.Contains(string(buf[:n]), "oci") {
			return "", fmt.Errorf("podman on PATH invokes bashy oci")
		}
	}
	return bin, nil
}

func prependPath(env []string, dir string) []string {
	key := "PATH"
	sep := string(os.PathListSeparator)
	for i, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if ok && strings.EqualFold(k, key) {
			cp := append([]string(nil), env...)
			cp[i] = k + "=" + dir + sep + v
			return cp
		}
	}
	return append(env, key+"="+dir)
}

// resolvePodmanBinary picks the podman binary: $BASHY_PODMAN_SYSTEM → $PATH;
// else embedded; else cached engine or managed download; else a clear error.
func resolvePodmanBinary() (string, error) {
	if systemPodman() {
		bin, err := safePathPodman()
		if err != nil {
			return "", fmt.Errorf("podman not found on PATH ($BASHY_PODMAN_SYSTEM is set; unset it to use the embedded binary): %w", err)
		}
		return bin, nil
	}
	if podman_embed.Available() {
		bin, err := podman_embed.EnsurePodman(defaultBinCacheDir())
		if err != nil {
			return "", fmt.Errorf("extract embedded podman: %w", err)
		}
		return bin, nil
	}
	var managedErr error
	if bin, err := ensureManagedPodman(context.Background()); err == nil {
		return bin, nil
	} else {
		managedErr = err
	}
	return "", fmt.Errorf("podman unavailable: no embedded binary, no cached engine, and managed provisioning for %s failed: %w", managedPodmanPlatform(), managedErr)
}

// newPodmanMachineCmd wires the isolated `bashy` machine lifecycle.
func newPodmanMachineCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "machine",
		Short: "Manage bashy's embedded (isolated) podman VM",
		Long: `Manage the Linux VM that hosts containers on macOS / Windows — bashy's own
machine ("` + DefaultMachineName + `"), kept distinct from any host or ycode machine.

Most users never run these directly — the machine auto-provisions on first
container use. Use them to recover from corrupted state or tune the VM.`,
	}
	cmd.AddCommand(
		newMachineInitCmd(),
		newMachineStartCmd(),
		newMachineStopCmd(),
		newMachineListCmd(),
		newMachineRmCmd(),
		newMachineResetCmd(),
	)
	return cmd
}

func newMachineInitCmd() *cobra.Command {
	cfg := DefaultMachineConfig()
	cmd := &cobra.Command{
		Use:   "init [NAME]",
		Short: "Create (and register) a new VM",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				cfg.Name = args[0]
			}
			if err := InitMachine(cmd.Context(), cfg); err != nil {
				return err
			}
			fmt.Printf("Machine %q initialized. Start it with: bashy podman machine start %s\n", cfg.Name, cfg.Name)
			return nil
		},
	}
	cmd.Flags().IntVar(&cfg.CPUs, "cpus", cfg.CPUs, "Number of vCPUs")
	cmd.Flags().IntVar(&cfg.Memory, "memory", cfg.Memory, "Memory in MB")
	cmd.Flags().IntVar(&cfg.Disk, "disk-size", cfg.Disk, "Disk size in GB")
	cmd.Flags().BoolVar(&cfg.NoAutoCleanup, "no-auto-cleanup", false,
		"Skip auto-cleanup of orphaned vfkit/gvproxy processes on preflight refusal")
	cmd.Flags().BoolVar(&cfg.Rootful, "rootful", false,
		"Forward the machine's API socket to the VM's rootful podman daemon (required for k3s-agent / kubelet-in-container).")
	return cmd
}

func newMachineStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start [NAME]",
		Short: "Start a stopped VM",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := DefaultMachineName
			if len(args) == 1 {
				name = args[0]
			}
			if err := StartMachine(cmd.Context(), name); err != nil {
				return err
			}
			fmt.Printf("Machine %q started\n", name)
			return nil
		},
	}
}

func newMachineStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop [NAME]",
		Short: "Stop a running VM",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := DefaultMachineName
			if len(args) == 1 {
				name = args[0]
			}
			if err := StopMachine(cmd.Context(), name); err != nil {
				return err
			}
			fmt.Printf("Machine %q stopped\n", name)
			return nil
		},
	}
}

func newMachineListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List managed VMs",
		RunE: func(cmd *cobra.Command, args []string) error {
			machines, err := ListMachines(cmd.Context())
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tVMTYPE\tCPUS\tMEMORY\tDISK\tRUNNING")
			for _, m := range machines {
				fmt.Fprintf(w, "%s\t%s\t%d\t%d MiB\t%d GiB\t%t\n",
					m.Name, m.VMType, m.CPUs, uint64(m.Memory), uint64(m.DiskSize), m.Running)
			}
			w.Flush()
			return nil
		},
	}
}

func newMachineRmCmd() *cobra.Command {
	var (
		force        bool
		saveImage    bool
		saveIgnition bool
	)
	cmd := &cobra.Command{
		Use:   "rm [NAME]",
		Short: "Remove a VM",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := DefaultMachineName
			if len(args) == 1 {
				name = args[0]
			}
			opts := ociMachine.RemoveOptions{Force: force, SaveImage: saveImage, SaveIgnition: saveIgnition}
			if err := RemoveMachine(cmd.Context(), name, opts); err != nil {
				return err
			}
			fmt.Printf("Machine %q removed\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Stop and remove a running machine")
	cmd.Flags().BoolVar(&saveImage, "save-image", false, "Keep the downloaded VM disk image")
	cmd.Flags().BoolVar(&saveIgnition, "save-ignition", false, "Keep the ignition file")
	return cmd
}

func newMachineResetCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Wipe ALL machines and their state (recovery escape hatch)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return fmt.Errorf("refusing to reset without --yes (this is destructive)")
			}
			if err := ResetMachines(cmd.Context()); err != nil {
				return err
			}
			fmt.Println("All machines reset.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Confirm the reset (required)")
	return cmd
}
