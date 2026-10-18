package fleet

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

// runCmd drives one of the noun trees against a scratch local ring and returns
// what it wrote to stdout.
func runCmd(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// The store entry IS the asset blob the control plane serves. `show --yaml`
// must reproduce it byte-for-byte, or the two ends of `sync` have drifted and
// a round-trip through bashy would silently rewrite an org's definition.
func TestShowYAMLRoundTripsTheStoredBlob(t *testing.T) {
	root := t.TempDir()
	cat := New(WithRoot(root))
	if err := cat.SaveTool(Tool{
		Name: "orgtool", Kind: ToolKindCLI, Display: "Org Tool",
		CLI: ToolCLI{Binary: "orgtool", Launch: ToolLaunch{Exec: "orgtool --model {model} {prompt}"}},
	}); err != nil {
		t.Fatal(err)
	}

	onDisk, err := os.ReadFile(filepath.Join(root, "tools", "orgtool.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := runCmd(t, NewToolsCmd(WithRoot(root)), "show", "orgtool", "--yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got != string(onDisk) {
		t.Fatalf("show --yaml != the stored blob\n--- disk ---\n%s\n--- show ---\n%s", onDisk, got)
	}
}

// --yaml is the explicit spelling of show's default for tools and models.
func TestShowYAMLIsTheDefaultForToolsAndModels(t *testing.T) {
	root := t.TempDir()
	cat := New(WithRoot(root))
	if err := cat.SaveModel(Model{Name: "m1", Kind: "subscription", Provider: "anthropic", UpstreamID: "up1"}); err != nil {
		t.Fatal(err)
	}
	bare, err := runCmd(t, NewModelsCmd(WithRoot(root)), "show", "m1")
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := runCmd(t, NewModelsCmd(WithRoot(root)), "show", "m1", "--yaml")
	if err != nil {
		t.Fatal(err)
	}
	if bare != explicit {
		t.Fatalf("--yaml changed the default output:\n%q\nvs\n%q", bare, explicit)
	}
}

// An agent's asset blob is the AgentFile ENVELOPE, not a bare agent — that is
// the shape the store holds and the control plane serves, so re-importing the
// emitted bytes has to work.
func TestAgentsShowYAMLEmitsTheEnvelope(t *testing.T) {
	fleettest.Ring(t)
	root := t.TempDir()
	cat := New(WithRoot(root))
	if err := cat.SaveAgent(Agent{Name: "007", Tool: "claude", Model: "fable"}); err != nil {
		t.Fatal(err)
	}
	got, err := runCmd(t, NewAgentsCmd(WithRoot(root)), "show", "007", "--yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(got), "agents:") {
		t.Fatalf("not an AgentFile envelope:\n%s", got)
	}
	// The emitted bytes must parse back to the same binding.
	f, err := ParseAgentFile("007", []byte(got), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Agents) != 1 || f.Agents[0].MatrixKey() != "claude:fable5" {
		t.Fatalf("round-trip lost the binding: %+v", f.Agents)
	}
}

func TestAgentsShowReportsEffectiveBinaryFallback(t *testing.T) {
	root := t.TempDir()
	cat := New(WithRoot(root))
	if err := cat.SaveTool(Tool{
		Name: "fallback-tool", Kind: ToolKindCLI,
		CLI: ToolCLI{Launch: ToolLaunch{Exec: "fallback-tool --model {model} {prompt}"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(Model{Name: "m1", UpstreamID: "provider-m1"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(Agent{Name: "fallback-agent", Tool: "fallback-tool", Model: "m1"}); err != nil {
		t.Fatal(err)
	}

	got, err := runCmd(t, NewAgentsCmd(WithRoot(root)), "show", "fallback-agent")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "tool:    fallback-tool (fallback-tool)") {
		t.Fatalf("agent summary omitted the effective binary fallback:\n%s", got)
	}
}

// The human agent summary prints the EFFECTIVE reader prefs for the
// selected Agent+Tool+band: budget_bytes plus the other declared keys, with
// default values shown explicitly. The raw BASHY_PREFS JSON never prints.
func TestAgentShowReaderPrefs(t *testing.T) {
	mkRing := func(t *testing.T, agentPrefs ReaderPrefs, modelBand int) string {
		t.Helper()
		root := t.TempDir()
		cat := New(WithRoot(root))
		if err := cat.SaveTool(Tool{
			Name: "show-tool", Kind: ToolKindCLI,
			CLI: ToolCLI{Launch: ToolLaunch{Exec: "show-tool {prompt}"}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := cat.SaveModel(Model{Name: "show-model", UpstreamID: "provider-show-model", Band: modelBand}); err != nil {
			t.Fatal(err)
		}
		if err := cat.SaveAgent(Agent{Name: "show-agent", Tool: "show-tool", Model: "show-model", Prefs: agentPrefs}); err != nil {
			t.Fatal(err)
		}
		return root
	}
	show := func(t *testing.T, root string) string {
		t.Helper()
		got, err := runCmd(t, NewAgentsCmd(WithRoot(root)), "show", "show-agent")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	t.Run("defaults are explicit", func(t *testing.T) {
		got := show(t, mkRing(t, ReaderPrefs{}, 0))
		for _, want := range []string{
			fmt.Sprintf("budget_bytes=%d", DefaultBudgetBytes),
			"view=" + ViewText,
			"help=" + HelpClassic,
			"dialect=bash",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("agent summary omitted default %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, EnvBashyPrefs) {
			t.Fatalf("agent summary leaked the prefs env name:\n%s", got)
		}
	})

	t.Run("agent prefs win", func(t *testing.T) {
		got := show(t, mkRing(t, ReaderPrefs{BudgetBytes: 12345, View: ViewJSON}, 0))
		for _, want := range []string{"budget_bytes=12345", "view=" + ViewJSON} {
			if !strings.Contains(got, want) {
				t.Fatalf("agent summary omitted effective pref %q:\n%s", want, got)
			}
		}
	})

	t.Run("env resolves without leaking raw JSON", func(t *testing.T) {
		root := mkRing(t, ReaderPrefs{}, 0)
		raw := `{"budget_bytes":77777,"view":"json"}`
		t.Setenv(EnvBashyPrefs, raw)
		got := show(t, root)
		for _, want := range []string{"budget_bytes=77777", "view=json"} {
			if !strings.Contains(got, want) {
				t.Fatalf("agent summary omitted env pref %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, raw) {
			t.Fatalf("agent summary leaked raw %s:\n%s", EnvBashyPrefs, got)
		}
	})

	t.Run("band help default", func(t *testing.T) {
		got := show(t, mkRing(t, ReaderPrefs{}, 4))
		if !strings.Contains(got, "help="+HelpSkill) {
			t.Fatalf("agent summary omitted band help default %q:\n%s", HelpSkill, got)
		}
	})
}

// Asking for two formats is a caller mistake; letting one silently win would
// hide it.
func TestShowRejectsBothFormats(t *testing.T) {
	root := t.TempDir()
	cat := New(WithRoot(root))
	if err := cat.SaveAgent(Agent{Name: "007", Tool: "claude", Model: "fable"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		noun string
		cmd  *cobra.Command
		name string
	}{
		{"tools", NewToolsCmd(WithRoot(root)), "codex"},
		{"models", NewModelsCmd(WithRoot(root)), "fable"},
		{"agents", NewAgentsCmd(WithRoot(root)), "007"},
	} {
		if _, err := runCmd(t, tc.cmd, "show", tc.name, "--json", "--yaml"); err == nil {
			t.Errorf("%s show accepted --json --yaml together", tc.noun)
		}
	}
}
