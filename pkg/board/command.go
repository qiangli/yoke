package board

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// NewCommand constructs the read-only machine-global board projection used by
// the Sprint web panel and role dashboards. Passing nil uses the machine-global
// P1 sources; tests and P2 can inject a conductor-specific source set.
func NewCommand(sources []Source) *cobra.Command {
	var jsonOut, htmlOut bool
	var outPath string
	var expands []string
	cmd := &cobra.Command{Use: "board", Short: "Show the machine-global steward board across todo, sprint, and weave", Args: cobra.NoArgs, SilenceUsage: true, SilenceErrors: true, RunE: func(cmd *cobra.Command, _ []string) error {
		if jsonOut && htmlOut {
			return fmt.Errorf("--json and --html are mutually exclusive")
		}
		expand := map[string]bool{}
		valid := map[string]bool{"agents": true, "todo": true, "sprints": true, "runs": true, "workspaces": true, "salvage": true, "dag": true, "fleet": true, "resources": true, "utilization": true, "all": true}
		for _, part := range expands {
			for _, id := range strings.Split(part, ",") {
				if id = strings.TrimSpace(id); id != "" {
					if !valid[id] {
						return fmt.Errorf("unknown panel %q (want agents, todo, sprints, runs, workspaces, salvage, dag, fleet, resources, utilization, or all)", id)
					}
					expand[id] = true
				}
			}
		}
		// Steward scope is always the machine-global union, including completed
		// records. Panels never silently hide a subset of the underlying stores.
		opts := Options{All: true, Expand: expand}
		if sources == nil {
			sources = DefaultSources()
		}
		b, err := Collect(cmd.Context(), opts, sources, nil)
		if err != nil {
			return err
		}
		var renderer Renderer = TerminalRenderer{}
		if jsonOut {
			renderer = JSONRenderer{}
		} else if htmlOut {
			renderer = HTMLRenderer{}
		}
		payload, err := renderer.Render(b, opts)
		if err != nil {
			return err
		}
		if outPath != "" {
			return os.WriteFile(outPath, payload, 0o644)
		}
		_, err = cmd.OutOrStdout().Write(payload)
		return err
	}}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the bashy-board-v1 JSON envelope")
	cmd.Flags().BoolVar(&htmlOut, "html", false, "emit a self-contained responsive HTML dashboard")
	cmd.Flags().StringVar(&outPath, "out", "", "write rendered output to a file")
	cmd.Flags().StringSliceVar(&expands, "expand", nil, "expand panel(s): agents,todo,sprints,runs,workspaces,salvage,resources,utilization,all")
	return cmd
}

// NewDashboardCommand builds the role-scoped front door mounted as
// `bashy steward dashboard`. It is deliberately the same engine as `board`.
func NewDashboardCommand(sources []Source) *cobra.Command {
	cmd := NewCommand(sources)
	cmd.Use = "dashboard"
	cmd.Short = "Show the machine-global steward dashboard across todo, sprint, weave, and fleet"
	return cmd
}

// SkillRenderer prints the host's existing embedded role skill.
type SkillRenderer func(io.Writer) error

// newRoleCommand builds the backwards-compatible role namespace: bare `<role>`
// and `<role> skill` both render the existing operating skill, while `<role>
// dashboard` mounts this package's read-only board projection.
func newRoleCommand(role, short string, skill SkillRenderer, sources []Source) *cobra.Command {
	renderSkill := func(cmd *cobra.Command, _ []string) error {
		if skill == nil {
			return fmt.Errorf("%s skill renderer is not configured", role)
		}
		return skill(cmd.OutOrStdout())
	}
	root := &cobra.Command{
		Use: role, Short: short,
		Args: cobra.NoArgs, RunE: renderSkill, SilenceUsage: true, SilenceErrors: true,
	}
	// NO `completion` IN A ROLE NAMESPACE.
	//
	// cobra adds a shell-completion generator to whatever command is Execute()d,
	// and a role namespace is mounted as a subcommand tree of `bashy` — so the
	// generator it offers ("source <(steward completion bash)") is for a binary
	// called `steward` that does not exist. bashy owns its own completion, and an
	// agent reading `bashy steward --help` to learn what a steward can DO should
	// not have to work out that one of the listed verbs is scaffolding.
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(
		&cobra.Command{Use: "skill", Short: "Print the existing " + role + " operating skill", Args: cobra.NoArgs, RunE: renderSkill},
		NewDashboardCommand(sources),
	)
	return root
}

// NewStewardCommand mounts the steward role: skill + machine-global dashboard.
func NewStewardCommand(skill SkillRenderer, sources []Source) *cobra.Command {
	cmd := newRoleCommand("steward", "The steward role: its operating skill, host resources, and machine-global dashboard", skill, sources)
	cmd.Long = "The host-wide steward role: its operating skill and machine-global dashboard.\n\nResource health is a standing steward duty. Before fanout or large builds, and at long-run checkpoints, inspect disk, memory, CPU/load, utilization, and active containers with `bashy steward dashboard --expand resources,utilization` plus the host's native resource tools. Reclaim derived caches first; never prune shared container stores or stop live work blindly."
	return cmd
}

// NewConductorCommand mounts the conductor role: skill + dashboard. Sprint-scoping
// (one initiative) is a future refinement; today `conductor dashboard` shows the
// same board projection as the steward view.
func NewConductorCommand(skill SkillRenderer, sources []Source) *cobra.Command {
	return newRoleCommand("conductor", "The conductor role: its operating skill and dashboard", skill, sources)
}
