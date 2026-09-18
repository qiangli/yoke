package fleet

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestFleetListHelpDefinesEveryOutputField(t *testing.T) {
	cases := []struct {
		name string
		root func(...Option) *cobra.Command
		want []string
	}{
		{
			name: "tools", root: NewToolsCmd,
			want: []string{"NAME", "KIND", "BINARY", "MODEL-SELECT", "RING", "selects_model",
				"declared binary, otherwise NAME", "hidden CLI definitions", "not where an executable", "tools verify NAME"},
		},
		{
			name: "models", root: NewModelsCmd,
			want: []string{"NAME", "BAND", "KIND", "PROVIDER", "TARGET", "ALIASES", "RING",
				"band_source", "not billing mode", "tool-specific id"},
		},
		{
			name: "agents", root: NewAgentsCmd,
			want: []string{"NAME", "NICK", "BAND", "TOOL", "MODEL", "RELIAB", "RESOLVES", "RING",
				"structural only", "not a live check", "agents verify NAME --live", "canonical tool:model"},
		},
	}

	for _, tc := range cases {
		for _, args := range [][]string{{"--help"}, {"list", "--help"}} {
			t.Run(tc.name+"/"+strings.Join(args, "-"), func(t *testing.T) {
				out, err := runCmd(t, tc.root(WithRoot(t.TempDir())), args...)
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range tc.want {
					if !strings.Contains(out, want) {
						t.Errorf("help does not define %q:\n%s", want, out)
					}
				}
			})
		}
	}
}

func TestToolsTableCallsBooleanModelFieldModelSelect(t *testing.T) {
	out, err := runCmd(t, NewToolsCmd(WithRoot(t.TempDir())))
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(out, "\n")
	if !strings.Contains(first, "MODEL-SELECT") || strings.Contains(first, "\tMODEL\t") {
		t.Fatalf("ambiguous tools heading: %q", first)
	}
}

func TestToolsListReportsEffectiveBinaryFallback(t *testing.T) {
	root := t.TempDir()
	cat := New(WithRoot(root))
	if err := cat.SaveTool(Tool{
		Name: "fallback-tool",
		Kind: ToolKindCLI,
		CLI:  ToolCLI{Launch: ToolLaunch{Exec: "fallback-tool {prompt}"}},
	}); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, NewToolsCmd(WithRoot(root)), "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []toolRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Name == "fallback-tool" {
			if row.Binary != "fallback-tool" {
				t.Fatalf("binary = %q, want the effective name fallback", row.Binary)
			}
			return
		}
	}
	t.Fatalf("fallback-tool missing from output: %s", out)
}

func TestModelsJSONMakesLegacyDeclaredBandSourceExplicit(t *testing.T) {
	root := t.TempDir()
	cat := New(WithRoot(root))
	if err := cat.SaveModel(Model{Name: "prior-model", Band: 2}); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, NewModelsCmd(WithRoot(root)), "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []modelRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Name == "prior-model" {
			if row.BandSource != BandDeclared {
				t.Fatalf("band_source = %q, want %q", row.BandSource, BandDeclared)
			}
			return
		}
	}
	t.Fatalf("prior-model missing from output: %s", out)
}

func TestRootAndListFlagHelpCanBeSpecializedIndependently(t *testing.T) {
	cmd := NewAgentsCmd(WithRoot(t.TempDir()))
	rootAll := cmd.Flags().Lookup("all")
	list, _, err := cmd.Find([]string{"list"})
	if err != nil {
		t.Fatal(err)
	}
	listAll := list.Flags().Lookup("all")
	if rootAll == nil || listAll == nil {
		t.Fatal("agents root or list is missing --all")
	}
	rootAll.Usage = "root-specific meaning"
	if listAll.Usage == rootAll.Usage {
		t.Fatal("root and list share flag metadata; wrapper help mutations leak into list help")
	}
}

func TestAgentsListJSONRefusesAnAmbiguousIdentity(t *testing.T) {
	root := t.TempDir()
	cat := New(WithRoot(root))
	if err := cat.SaveAgent(Agent{Name: "alpha", Aliases: []string{"shared"}, Tool: "claude", Model: "opus5"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(Agent{Name: "beta", Aliases: []string{"shared"}, Tool: "codex", Model: "gpt-5.5"}); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, NewAgentsCmd(WithRoot(root)), "list", "--json")
	if err == nil {
		t.Fatalf("ambiguous JSON roster succeeded: %s", out)
	}
	if strings.HasPrefix(strings.TrimSpace(out), "[") {
		t.Fatalf("ambiguous JSON roster emitted selectable rows: %s", out)
	}
}
