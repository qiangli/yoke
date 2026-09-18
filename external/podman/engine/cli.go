package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	ociMachine "github.com/qiangli/yoke/pkg/oci/machine"

	podman_embed "github.com/qiangli/yoke/external/podman/engine/podman_embed"
)

// NewPodmanCmd is the `bashy podman` front-door: a thin pass-through to Podman,
// preferring the embedded binary, then a managed binmgr-downloaded remote
// client, then a host podman. CONTAINER_HOST auto-points at the ISOLATED `bashy`
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
  2. managed Podman remote client, downloaded and sha-verified by binmgr
  3. podman on PATH

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
	cmd.Env = env
	return cmd
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
// else embedded; else managed binmgr download/cache; else $PATH fallback; else a
// clear error.
func resolvePodmanBinary() (string, error) {
	if systemPodman() {
		bin, err := exec.LookPath("podman")
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
	if bin, err := exec.LookPath("podman"); err == nil {
		return bin, nil
	}
	return "", fmt.Errorf("no embedded podman in this build, managed podman unavailable for %s (%v), and no podman on PATH — rebuild with the embed_podman tag (the embed blob), or install upstream podman", managedPodmanPlatform(), managedErr)
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
