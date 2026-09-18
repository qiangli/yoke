package agentcmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/principal"
	"github.com/spf13/cobra"
)

const schemaVersion = "bashy-agent-v1"

type WhoAmIResult struct {
	SchemaVersion string `json:"schema_version"`
	Agent         string `json:"agent"`
	Source        string `json:"source"`
}

func NewAgentCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "agent identity and local agent helpers"}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.AddCommand(NewWhoamiCmd())
	return cmd
}

// NewWhoamiCmd is the `whoami` subcommand: the launcher-stamped agent identity.
// It is exported so the embedding shell can mount it under the canonical
// `agent` noun beside the fleet registry (`bashy agent whoami`) instead of a
// second top-level verb one letter away from it.
func NewWhoamiCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "print the active agent identity",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := WhoAmI()
			if err != nil {
				return err
			}
			if asJSON || os.Getenv("BASHY_AGENTIC") != "" {
				b, _ := json.Marshal(res)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), res.Agent)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// WhoAmI resolves the named agent this process is acting as. A harness is a
// tool, not an agent: without a launcher-stamped agent principal, returning
// the tool (or the OS login) would make an attribution claim the board rejects.
func WhoAmI() (WhoAmIResult, error) {
	self, ok := principal.NewResolver(fleet.New(), principal.DefaultEnv()).Self()
	if !ok || self.Kind != principal.KindAgent {
		if ok {
			return WhoAmIResult{}, fmt.Errorf("agent identity unavailable: running under tool %q, which is not an agent identity", self.Name)
		}
		return WhoAmIResult{}, fmt.Errorf("agent identity unavailable: no launcher-stamped agent identity")
	}

	// BoardIdentity's explicit path is exactly what `bashy mb --as <name>`
	// accepts, including catalog aliases. It intentionally does not inspect the
	// login fallback, because Self above already established an agent identity.
	name, err := bus.BoardIdentity(self.Name)
	if err != nil || strings.TrimSpace(name) == "" {
		if err != nil {
			return WhoAmIResult{}, fmt.Errorf("agent identity unavailable: %w", err)
		}
		return WhoAmIResult{}, fmt.Errorf("agent identity unavailable: board could not resolve the launcher-stamped identity")
	}
	return WhoAmIResult{
		SchemaVersion: schemaVersion,
		Agent:         name,
		Source:        "launcher principal",
	}, nil
}
