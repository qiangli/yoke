package commscli

// CLI-reachability audit for the S80 comms surface.
//
// WHY THIS EXISTS. S80 merged six changes; one of them (the inbox/notify front
// doors) passed every package test and still turned out to be unreachable from
// the shipped CLI — the constructors existed, nothing mounted them. A test that
// calls cmd.Execute() in-process cannot see that class of failure, because it
// IS the mount. So this audit drives every S80 verb the way an agent does:
// argv into a BUILT BINARY, a composed environment, an exit code, and the
// stdout/stderr split — with every store pointed at a temp dir.
//
// The binary is this test binary re-exec'd (COMMSCLI_TOOL=1): TestMain then
// runs a cobra root that mounts exactly the exported, host-agnostic
// constructors a host like bashy mounts — NewWhoisCmd, NewAgentCmd,
// NewMessageBoardCmd, NewBusCmd, NewInboxCmd, NewNotifyCmd, NewWeaveCmd —
// and wires the one injection point board.go documents as load-bearing
// (bus.FleetNames). That proves everything on this repo's side of the mount:
// the constructor exists, parses argv, resolves its stores from the
// environment, behaves, and exits with the right code. The one thing it
// cannot prove — that the bashy host actually calls the constructor — is
// covered by scripts/comms-cli-audit.sh (run it against a freshly built
// bashy) and pinned by atlas_test.go.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/agentcmd"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/principal"
	"github.com/qiangli/yoke/pkg/weave"
)

func TestMain(m *testing.M) {
	// Stub-tool mode: `weave fleet --probe` executes roster binaries; the
	// stubs planted on the audit PATH are copies of this test binary, told
	// apart by their basename. Both exit 0 — only their OUTPUT differs,
	// which is the property the probe test exists to prove.
	switch strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") {
	case "stub-ok":
		fmt.Println(fleet.SmokeToken)
		os.Exit(0)
	case "stub-mute":
		os.Exit(0) // success exit, no output: an unusable tool that lies with its status
	}
	if os.Getenv("COMMSCLI_TOOL") == "1" {
		runAuditTool()
	}
	os.Exit(m.Run())
}

// runAuditTool is the audit binary: the exported S80 front doors mounted on
// one root, driven by real argv, exiting with a real code. It mirrors the
// host contract, not a test harness: errors print to stderr and exit
// non-zero; weave's structured exits are honored per pkg/weave/export.go.
func runAuditTool() {
	// The ONE host injection the board documents as load-bearing
	// ("WIRING THIS IS LOAD-BEARING", pkg/bus/board.go): the fleet roster.
	// Without it a catalog agent is unresolvable and every send to one fails.
	bus.FleetNames = func() []string {
		agents, _ := fleet.New().Agents()
		names := make([]string, 0, len(agents))
		for _, a := range agents {
			names = append(names, a.Name)
		}
		return names
	}

	root := &cobra.Command{Use: "commscli-audit", SilenceUsage: true, SilenceErrors: true}
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(
		principal.NewWhoisCmd(),
		agentcmd.NewAgentCmd(),
		bus.NewMessageBoardCmd(),
		bus.NewBusCmd(),
		bus.NewInboxCmd(),
		bus.NewNotifyCmd(),
		weave.NewWeaveCmd(),
	)
	err := root.Execute()
	if err == nil {
		os.Exit(0)
	}
	if weave.IsStructuredExit(err) {
		os.Exit(weave.ExitCode(err)) // the subverb already reported itself
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
