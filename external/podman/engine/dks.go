// dks.go — the `bashy dks` front-door verb: provision and manage the
// dedicated, uniform, ROOTFUL podman machine that DKS (the dhnt Kubernetes
// tier) runs k3s in.
//
// Why a dedicated rootful machine, separate from the general `bashy` machine:
// k3s-agent / kubelet-in-container must create cgroups under /sys/fs/cgroup
// (kubepods, the k8s.io slice). A ROOTLESS podman machine denies those writes
// even under --privileged — the container's cgroup-namespace root is not the
// true root, and newer VM images (systemd 259 / cgroup2 `nsdelegate`) enforce
// delegation boundaries, so a pod silently never starts while the node still
// reports Ready. A ROOTFUL machine runs containers under machine.slice with
// true-root cgroup access, which sidesteps the whole class of failure. Making
// DKS its own uniform, pinned, rootful VM means an outpost in the wild does
// not inherit whatever podman-machine quirk the user happens to have. See
// docs/dks-tenancy-model.md and docs/todo/f1c8a04d7e93-*.md for the full
// investigation that motivated this.
package engine

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	ociMachine "github.com/qiangli/yoke/pkg/oci/machine"
)

// DKSMachineName is the dedicated rootful podman machine DKS/k3s runs in,
// kept distinct from DefaultMachineName ("bashy", the general rootless
// machine) so DKS isolation and rootfulness never fight general sandboxing.
const DKSMachineName = "dks"

// dksMinMemoryMB is the memory floor for the DKS VM. k3s-agent + kubelet +
// a couple of real workload pods need meaningfully more than the general
// bashy default; host-aware sizing may raise this, never lower it.
const dksMinMemoryMB = 4096

// dksRecommendDiskGB is the RECOMMENDED disk for a real k3s workload (VM image
// + containerd image store + ephemeral pod storage). It is a WARNING
// threshold, NOT a hard floor: podman's preflight already requires ~2x the VM
// disk free, so hard-flooring to 30GB made disk-tight hosts unprovisionable
// (30GB disk → 60GB required free). Host-aware sizing picks the actual value;
// we only warn when it lands below this.
const dksRecommendDiskGB = 30

// NewDKSCmd builds the `dks` front-door verb. Cross-platform via the podman
// machine provider (AppleHV on macOS, WSL/Hyper-V on Windows). On Linux
// containers run natively, so no VM is provisioned — the verb reports that.
func NewDKSCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dks",
		Short: "Provision and manage the dedicated rootful VM that DKS (k3s) runs in",
		Long: `dks provisions a designated, uniform, ROOTFUL podman machine for the dhnt
Kubernetes tier. k3s-agent / kubelet-in-container must create cgroups under
/sys/fs/cgroup; a rootless machine denies that even under --privileged, so a
pod silently never starts. A dedicated rootful VM fixes this uniformly across
every outpost, so hosts in the wild don't each hit a different podman quirk.

macOS/Windows only — on Linux, containers run natively (no VM needed).`,
	}
	cmd.AddCommand(newDKSProvisionCmd(), newDKSStatusCmd())
	return cmd
}

// dksConfig returns the DKS machine config: host-aware sizing, dedicated
// name, ROOTFUL, with the k3s memory floor applied.
func dksConfig() MachineConfig {
	cfg := DefaultMachineConfig()
	cfg.Name = DKSMachineName
	cfg.Rootful = true
	if cfg.Memory < dksMinMemoryMB {
		cfg.Memory = dksMinMemoryMB
	}
	// Disk is NOT floored — see dksRecommendDiskGB. Host-aware sizing decides;
	// the preflight refuses if there's genuinely too little.
	return cfg
}

func newDKSProvisionCmd() *cobra.Command {
	cfg := dksConfig()
	var name string = cfg.Name
	cmd := &cobra.Command{
		Use:   "provision",
		Short: "Ensure the rootful DKS machine exists and is running (idempotent)",
		Long: `provision ensures a dedicated ROOTFUL podman machine exists and is running.
Idempotent: safe to run repeatedly. On first run it downloads the VM image
(~800MB) and enables cpuset delegation so k3s can start.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS == "linux" {
				fmt.Println("dks: Linux runs containers natively — no VM needed. " +
					"Run k3s under rootful podman (machine.slice) so kubelet can manage cgroups.")
				return nil
			}
			cfg.Name = name
			cfg.Rootful = true // never provision DKS rootless — that is the whole point
			if cfg.Memory < dksMinMemoryMB {
				cfg.Memory = dksMinMemoryMB
			}
			if cfg.Disk < dksRecommendDiskGB {
				fmt.Printf("dks: WARNING — VM disk %d GB is below the %d GB recommended for real "+
					"k3s workloads (image store fills fast). Provisioning anyway; pass --disk-size to raise.\n",
					cfg.Disk, dksRecommendDiskGB)
			}
			if err := EnsureMachine(cmd.Context(), cfg); err != nil {
				return fmt.Errorf("dks provision: %w", err)
			}
			fmt.Printf("dks: machine %q ready (rootful, %d vCPU / %d MB / %d GB).\n",
				cfg.Name, cfg.CPUs, cfg.Memory, cfg.Disk)
			fmt.Println("dks: point the outpost cluster agent at this machine's rootful connection " +
				"(the k3s-agent container must run rootful to create pod cgroups).")
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", name, "machine name")
	cmd.Flags().IntVar(&cfg.CPUs, "cpus", cfg.CPUs, "vCPUs for the DKS VM")
	cmd.Flags().IntVar(&cfg.Memory, "memory", cfg.Memory, "memory (MB) for the DKS VM")
	cmd.Flags().IntVar(&cfg.Disk, "disk-size", cfg.Disk, "disk (GB) for the DKS VM")
	cmd.Flags().BoolVar(&cfg.NoAutoCleanup, "no-auto-cleanup", false,
		"skip auto-cleanup of orphaned vfkit/gvproxy processes on preflight refusal")
	return cmd
}

func newDKSStatusCmd() *cobra.Command {
	name := DKSMachineName
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the DKS machine's existence, rootful mode, and run state",
		RunE: func(_ *cobra.Command, _ []string) error {
			if runtime.GOOS == "linux" {
				fmt.Println("dks: Linux — native containers, no VM.")
				return nil
			}
			mp, err := ociMachine.GetProvider()
			if err != nil {
				return fmt.Errorf("get machine provider: %w", err)
			}
			mc, exists := findMachine(name, mp)
			if !exists {
				fmt.Printf("dks: machine %q not provisioned — run `bashy dks provision`.\n", name)
				return nil
			}
			state, err := mp.State(mc, false)
			if err != nil {
				return fmt.Errorf("machine state: %w", err)
			}
			rootful := mc.HostUser.Rootful
			fmt.Printf("dks: machine %q  state=%s  rootful=%v\n", name, state, rootful)
			if !rootful {
				fmt.Println("dks: WARNING — machine is NOT rootful; k3s pods will fail to start. " +
					"Recreate with `bashy dks provision` (removing the rootless machine first).")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", name, "machine name")
	return cmd
}
