package fleet_test

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/resources"
	"github.com/qiangli/yoke/pkg/skills"
)

func runExtensionCommand(t *testing.T, root *cobra.Command, input string, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root.SetIn(bytes.NewBufferString(input))
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute()
	if err != nil && errOut.Len() != 0 {
		return out.String() + errOut.String(), err
	}
	return out.String(), err
}

func TestSevenExtensionKindsYAMLAddIsIdentity(t *testing.T) {
	t.Setenv("BASHY_MINT_SPRINT", "")
	t.Setenv("BASHY_RESOURCES_DIR", t.TempDir())
	type kindCase struct {
		kind string
		name string
		root func() *cobra.Command
		seed func() error
		add  string
	}
	cases := []kindCase{}

	// Each fleet catalog uses one pinned root across seed, show and import.
	newFleetCase := func(kind, name string, seed func(*fleet.Catalog) error, root func(...fleet.Option) *cobra.Command) kindCase {
		dir := t.TempDir()
		opts := []fleet.Option{fleet.WithRoot(dir), fleet.WithoutCloudOverlay()}
		return kindCase{kind: kind, name: name, root: func() *cobra.Command { return root(opts...) }, seed: func() error { return seed(fleet.New(opts...)) }}
	}
	skillDir := t.TempDir()
	cases = []kindCase{
		newFleetCase("tool", "matrix-tool", func(c *fleet.Catalog) error {
			return c.SaveTool(fleet.Tool{Name: "matrix-tool", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: "/bin/true"}})
		}, fleet.NewToolsCmd),
		newFleetCase("model", "matrix-model", func(c *fleet.Catalog) error {
			return c.SaveModel(fleet.Model{Name: "matrix-model", Provider: "openai", Kind: fleet.ModelKindAPI, UpstreamID: "matrix-model"})
		}, fleet.NewModelsCmd),
		newFleetCase("agent", "matrix-agent", func(c *fleet.Catalog) error {
			if err := c.SaveTool(fleet.Tool{Name: "matrix-agent-tool", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: "/bin/true"}}); err != nil {
				return err
			}
			if err := c.SaveModel(fleet.Model{Name: "matrix-agent-model", Provider: "openai", Kind: fleet.ModelKindAPI, UpstreamID: "matrix-agent-model"}); err != nil {
				return err
			}
			return c.SaveAgent(fleet.Agent{Name: "matrix-agent", Tool: "matrix-agent-tool", Model: "matrix-agent-model", Permission: fleet.PermissionAuto})
		}, fleet.NewAgentsCmd),
		newFleetCase("command", "matrix-command", func(c *fleet.Catalog) error {
			return c.SaveCommand(fleet.Command{Name: "matrix-command", Kind: fleet.KindCommand, Exec: []string{"/bin/true"}, Effects: []string{"read"}})
		}, fleet.NewCommandsCmd),
		newFleetCase("service", "matrix-service", func(c *fleet.Catalog) error {
			return c.SaveService(fleet.Service{Name: "matrix-service", Kind: fleet.KindService, Exec: []string{"/bin/true"}, Enabled: false})
		}, fleet.NewServiceCmd),
		{kind: "resource", name: "matrix-resource", root: resources.NewCommand},
		{kind: "skill", name: "matrix-skill", root: func() *cobra.Command { return skills.NewSkillsCmd(skills.WithConfigDir(skillDir)) }},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			if tc.seed != nil {
				if err := tc.seed(); err != nil {
					t.Fatal(err)
				}
			} else {
				var seedArgs []string
				switch tc.kind {
				case "resource":
					seedArgs = []string{"add", tc.name, "--kind", "path", "/tmp/matrix-resource"}
				case "skill":
					seedArgs = []string{"add", tc.name, "--description", "round-trip fixture"}
				}
				if _, err := runExtensionCommand(t, tc.root(), "", seedArgs...); err != nil {
					t.Fatalf("seed %s: %v", tc.kind, err)
				}
			}
			before, err := runExtensionCommand(t, tc.root(), "", "show", tc.name, "--yaml")
			if err != nil {
				t.Fatalf("show --yaml: %v\n%s", err, before)
			}
			if _, err := runExtensionCommand(t, tc.root(), before, "add", "-f", "-"); err != nil {
				t.Fatalf("add -f -: %v", err)
			}
			after, err := runExtensionCommand(t, tc.root(), "", "show", tc.name, "--yaml")
			if err != nil {
				t.Fatalf("show after import: %v", err)
			}
			if before != after {
				t.Fatalf("show|add -f - changed %s record:\n%s\n---\n%s", tc.kind, before, after)
			}
			if _, err := runExtensionCommand(t, tc.root(), "", "list", "--json"); err != nil {
				t.Fatalf("list --json: %v", err)
			}
			setArgs := map[string][]string{
				"tool":     {"set", tc.name, "--display", "updated"},
				"model":    {"set", tc.name, "--display", "updated"},
				"agent":    {"set", tc.name, "--description", "updated"},
				"command":  {"set", tc.name, "--set", "synopsis=updated"},
				"service":  {"set", tc.name, "--set", "description=updated"},
				"resource": {"set", tc.name, "--title", "updated"},
				"skill":    {"set", tc.name, "--description", "updated"},
			}[tc.kind]
			if _, err := runExtensionCommand(t, tc.root(), "", setArgs...); err != nil {
				t.Fatalf("set: %v", err)
			}
			verifyOut, _ := runExtensionCommand(t, tc.root(), "", "verify", tc.name, "--json")
			if verifyOut == "" {
				t.Fatal("verify --json produced no report")
			}
			if _, err := runExtensionCommand(t, tc.root(), "", "rm", tc.name); err != nil {
				t.Fatalf("rm: %v", err)
			}
		})
	}
}
