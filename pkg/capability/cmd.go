package capability

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// referenceMD is the full `bashy capability` guide, embedded so it ships with the
// binary and is available wherever bashy runs. Surfaced by `bashy capability reference`.
//
//go:embed reference.md
var referenceMD string

// NewCapabilityCmd returns the `bashy capability` command tree — the living
// agent×capability matrix behind capability-routed delegation.
func NewCapabilityCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "capability",
		Short: "living agent (tool:model) × capability matrix for routing",
		Long: "The routing table behind capability-routed delegation: which agent\n" +
			"(tool:model) is best for each capability, seeded from research priors and\n" +
			"refined by observed outcomes on this host. Run `bashy capability reference`\n" +
			"for the full guide.",
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.AddCommand(newBestCmd(), newShowCmd(), newMatrixCmd(), newRecordCmd(), newSeedCmd(), newReferenceCmd())
	return cmd
}

func newReferenceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reference",
		Short: "print the full bashy capability guide (embedded in the binary)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprint(cmd.OutOrStdout(), referenceMD)
			return nil
		},
	}
}

func newBestCmd() *cobra.Command {
	var all bool
	var by string
	cmd := &cobra.Command{
		Use:   "best <capability>",
		Short: "rank agents for a capability (routable only, unless --all)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, ok := ParseCapability(args[0])
			if !ok {
				return fmt.Errorf("unknown capability %q (try: %s)", args[0], joinCaps())
			}
			key := SortKey(by)
			if key != ByQuality && key != ByValue && key != ByCost {
				return fmt.Errorf("--by must be quality | value | cost")
			}
			m, err := Load()
			if err != nil {
				return err
			}
			ranked := m.Best(c, !all, key)
			if len(ranked) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "no agents scored for %s\n", c)
				return nil
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "best for %s (by %s):\n", c, key)
			for i, r := range ranked {
				mark := "✓"
				if !r.Operable {
					mark = "✗"
				}
				fmt.Fprintf(w, "  %d. %-26s q=%.2f rel=%.2f cost=%d val=%.2f %s %s [%s]\n",
					i+1, r.Agent, r.Cell.Quality, r.Reliability, r.Cell.CostMicro, r.Value, mark, r.Reason, r.Cell.Source)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include non-routable agents")
	cmd.Flags().StringVar(&by, "by", "quality", "ranking key: quality | value | cost")
	return cmd
}

func newShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <agent>",
		Short: "show one agent's capability row (agent = tool:model)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := Load()
			if err != nil {
				return err
			}
			row, ok := m.Agents[args[0]]
			if !ok {
				return fmt.Errorf("no such agent %q (see `bashy capability matrix`)", args[0])
			}
			w := cmd.OutOrStdout()
			ok2, reason := Operable(ToolOf(args[0]))
			fmt.Fprintf(w, "%s  (operable=%v: %s)\n", args[0], ok2, reason)
			for _, c := range AllCaps() {
				if cell, ok := row[c]; ok {
					fmt.Fprintf(w, "  %-18s q=%.2f [%s n=%d]\n", c, cell.Quality, cell.Source, cell.Samples)
				}
			}
			return nil
		},
	}
}

func newMatrixCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "matrix",
		Short: "print the full agent × capability matrix",
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := Load()
			if err != nil {
				return err
			}
			agents := make([]string, 0, len(m.Agents))
			for a := range m.Agents {
				agents = append(agents, a)
			}
			sort.Strings(agents)
			w := cmd.OutOrStdout()
			caps := AllCaps()
			// header
			fmt.Fprintf(w, "%-26s", "agent \\ capability")
			for _, c := range caps {
				fmt.Fprintf(w, " %-4s", abbrev(c))
			}
			fmt.Fprintln(w)
			for _, a := range agents {
				fmt.Fprintf(w, "%-26s", a)
				for _, c := range caps {
					if cell, ok := m.Agents[a][c]; ok {
						fmt.Fprintf(w, " %-4.2f", cell.Quality)
					} else {
						fmt.Fprintf(w, " %-4s", "-")
					}
				}
				fmt.Fprintln(w)
			}
			fmt.Fprintf(w, "\ncolumns: %s\n", legend(caps))
			return nil
		},
	}
}

func newRecordCmd() *cobra.Command {
	var agent, capStr, outcome string
	var latency, cost int64
	cmd := &cobra.Command{
		Use:   "record --agent tool:model --capability C --outcome pass|fail",
		Short: "fold an observed outcome into the matrix (self-updating)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if agent == "" || capStr == "" {
				return fmt.Errorf("--agent and --capability are required")
			}
			c, ok := ParseCapability(capStr)
			if !ok {
				return fmt.Errorf("unknown capability %q", capStr)
			}
			pass := outcome == "pass" || outcome == "ok" || outcome == "true"
			if outcome != "" && !pass && outcome != "fail" && outcome != "false" {
				return fmt.Errorf("--outcome must be pass|fail")
			}
			if err := Record(agent, c, pass, latency, cost, NowRFC()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "recorded %s / %s = %s\n", agent, c, outcome)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&agent, "agent", "", "tool:model")
	f.StringVar(&capStr, "capability", "", "capability key")
	f.StringVar(&outcome, "outcome", "pass", "pass|fail")
	f.Int64Var(&latency, "latency", 0, "observed latency ms")
	f.Int64Var(&cost, "cost", 0, "observed cost (micro-units)")
	return cmd
}

func newSeedCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "seed",
		Short: "(re)write the research-prior matrix",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !force {
				if _, err := Load(); err == nil {
					fmt.Fprintln(cmd.OutOrStdout(), "matrix exists; use --force to overwrite priors (host-measured cells are lost)")
					return nil
				}
			}
			m := seedPriors()
			if err := m.save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "seeded %d agents at %s\n", len(m.Agents), matrixPath())
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing matrix")
	return cmd
}

func joinCaps() string {
	parts := make([]string, 0, len(AllCaps()))
	for _, c := range AllCaps() {
		parts = append(parts, string(c))
	}
	return strings.Join(parts, ", ")
}

// abbrev gives a stable 4-char column tag for the matrix header.
func abbrev(c Capability) string {
	m := map[Capability]string{
		CapOperability: "oper", CapShell: "shel", CapToolUse: "tool", CapIsolation: "isol",
		CapCoding: "code", CapBugFixing: "bug", CapCodeReview: "revw", CapTestGen: "test",
		CapDeepResearch: "rsch", CapWebSearch: "web", CapBrowserUse: "brws", CapDataAnalysis: "data",
		CapPlanning: "plan", CapDecisionSupport: "decn", CapOrchestration: "orch",
	}
	if s, ok := m[c]; ok {
		return s
	}
	if len(c) >= 4 {
		return string(c)[:4]
	}
	return string(c)
}

func legend(caps []Capability) string {
	parts := make([]string, 0, len(caps))
	for _, c := range caps {
		parts = append(parts, abbrev(c)+"="+string(c))
	}
	return strings.Join(parts, " · ")
}
