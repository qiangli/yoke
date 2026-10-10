package skills

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/fleet"
)

func mcpTools() []fleet.Tool {
	return []fleet.Tool{
		{Name: "claw", Integration: fleet.ToolIntegration{
			Skills: fleet.IntegrationSkills{User: []string{"~/.claw/skills"}},
			MCP: &fleet.IntegrationMCP{
				CLI: []string{"claw", "mcp", "add", "-s", "user", "bashy", "--", "bashy", "mcp", "serve"},
			},
		}},
		{Name: "quiet", Integration: fleet.ToolIntegration{
			Skills: fleet.IntegrationSkills{User: []string{"~/.quiet/skills"}},
			MCP: &fleet.IntegrationMCP{
				Config: "~/.quiet/servers.toml",
				Format: "toml",
				Key:    "mcp_servers",
			},
		}},
		{Name: "nomcp", Integration: fleet.ToolIntegration{
			Skills: fleet.IntegrationSkills{User: []string{"~/.nomcp/skills"}},
		}},
	}
}

func swapMCPSeams(t *testing.T, tools func() []fleet.Tool) *[][]string {
	t.Helper()
	var ran [][]string
	origTools, origRun := fleetTools, runMCPRegistration
	fleetTools = tools
	runMCPRegistration = func(argv []string, stdout, stderr io.Writer) error {
		ran = append(ran, argv)
		return nil
	}
	t.Cleanup(func() { fleetTools, runMCPRegistration = origTools, origRun })
	return &ran
}

// --mcp registers the bashy MCP server by running the tool's OWN declared
// command, verbatim — never by rewriting the tool's config file.
func TestExportMCPRunsToolsOwnCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ran := swapMCPSeams(t, mcpTools)

	cfg, _, _ := exportFixture(t)
	var out, errb bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	if err := runExport(cmd, cfg, "guide", "", []string{"claw"}, false, false, false, true, false); err != nil {
		t.Fatalf("export: %v\n%s", err, errb.String())
	}
	if len(*ran) != 1 || !slices.Equal((*ran)[0], []string{"claw", "mcp", "add", "-s", "user", "bashy", "--", "bashy", "mcp", "serve"}) {
		t.Fatalf("registration ran %v", *ran)
	}
	// The skill export itself still happened into the tool's declared roots.
	if _, err := os.Stat(filepath.Join(home, ".claw", "skills", "guide", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}

// --dry-run prints the argv instead of running it.
func TestExportMCPDryRunPrintsCommandWithoutRunningIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ran := swapMCPSeams(t, mcpTools)

	cfg, _, _ := exportFixture(t)
	var out, errb bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	if err := runExport(cmd, cfg, "guide", "", []string{"claw"}, false, false, false, true, true); err != nil {
		t.Fatalf("export: %v\n%s", err, errb.String())
	}
	if len(*ran) != 0 {
		t.Fatalf("dry run executed %v", *ran)
	}
	if !strings.Contains(out.String(), "claw mcp add -s user bashy -- bashy mcp serve") {
		t.Fatalf("dry-run output = %q", out.String())
	}
}

// A tool with no registration command gets the exact entry to add, the config
// path and key printed — its config file is never touched.
func TestExportMCPWithoutCLIPrintsEntryInsteadOfEditing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ran := swapMCPSeams(t, mcpTools)

	cfg, _, _ := exportFixture(t)
	var out, errb bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	if err := runExport(cmd, cfg, "guide", "", []string{"quiet"}, false, false, false, true, false); err != nil {
		t.Fatalf("export: %v\n%s", err, errb.String())
	}
	if len(*ran) != 0 {
		t.Fatalf("printed path still ran %v", *ran)
	}
	s := out.String()
	for _, want := range []string{
		filepath.Join(home, ".quiet", "servers.toml"),
		"mcp_servers",
		"bashy",
		`command = "bashy"`,
		`args = ["mcp", "serve"]`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	// Never rewritten: the config file is still absent after the export.
	if _, err := os.Stat(filepath.Join(home, ".quiet", "servers.toml")); !os.IsNotExist(err) {
		t.Fatalf("quiet config was written: %v", err)
	}
}

// --mcp needs a named tool, and a tool with no MCP declaration is an error,
// never a silent no-op.
func TestExportMCPValidation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ran := swapMCPSeams(t, mcpTools)

	cfg, _, _ := exportFixture(t)
	newCmd := func() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
		var out, errb bytes.Buffer
		c := &cobra.Command{}
		c.SetOut(&out)
		c.SetErr(&errb)
		return c, &out, &errb
	}
	c, _, _ := newCmd()
	if err := runExport(c, cfg, "guide", "", nil, false, false, false, true, false); err == nil || !strings.Contains(err.Error(), "--tool") {
		t.Fatalf("--mcp without --tool: err = %v", err)
	}
	c, _, _ = newCmd()
	if err := runExport(c, cfg, "guide", "", []string{"nomcp"}, false, false, false, true, false); err == nil || !strings.Contains(err.Error(), "no MCP") {
		t.Fatalf("tool without MCP block: err = %v", err)
	}
	if len(*ran) != 0 {
		t.Fatalf("validation errors ran %v", *ran)
	}
}

// The flags reach the cobra surface: `skill export <name> --tool T --mcp
// --dry-run` prints the tool's own registration argv.
func TestCLIExportMCPDryRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	swapMCPSeams(t, mcpTools)

	embedded := fstest.MapFS{
		"guide/SKILL.md": {Data: []byte("---\nname: guide\ndescription: the guide\n---\nBODY\n")},
	}
	f := &cobraRunner{t: t, opts: []Option{
		WithSource(EmbedSource(embedded, RingEmbedded)),
		WithConfigDir(t.TempDir()),
	}}
	stdout, _, err := f.run("export", "guide", "--tool", "claw", "--mcp", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "claw mcp add -s user bashy -- bashy mcp serve") {
		t.Fatalf("stdout = %q", stdout)
	}
}
