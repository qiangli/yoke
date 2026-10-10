package fleet

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/assetring"
)

func TestListFilterKeepsSparseLocalOverrideAsLocalDefinition(t *testing.T) {
	root := t.TempDir()
	opts := []Option{WithRoot(root), WithoutCloudOverlay()}
	if _, err := runCmd(t, NewToolsCmd(opts...), "set", "codex", "--display", "Local display"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, dirTools, "codex.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "overlay: true") || strings.Contains(string(data), "launch:") {
		t.Fatalf("set materialized a full definition: %s", data)
	}
	for _, args := range [][]string{{"--custom", "--json"}, {"--ring", "local", "--json"}} {
		out, err := runCmd(t, NewToolsCmd(opts...), args...)
		if err != nil {
			t.Fatal(err)
		}
		rows := decodeListItems[toolRow](t, []byte(out))
		var found bool
		for _, row := range rows {
			if row.Name == "codex" {
				found = true
				if row.Ring != "local" || row.Binary != "codex" {
					t.Fatalf("%v: selected overlay = %+v", args, row)
				}
			}
		}
		if !found {
			t.Fatalf("%v: missing overridden tool", args)
		}
	}
	// The default view selects across ALL rings, so a sparse local override
	// of a seeded name shows up as the local selected definition.
	defaultOut, err := runCmd(t, NewToolsCmd(opts...), "--json")
	if err != nil {
		t.Fatal(err)
	}
	defaultRows := decodeListItems[toolRow](t, []byte(defaultOut))
	var found bool
	for _, row := range defaultRows {
		if row.Name == "codex" {
			found = true
			if row.Ring != "local" {
				t.Fatalf("default view selected = %+v, want the local override", row)
			}
		}
	}
	if !found {
		t.Fatal("default view hid the local selected definition")
	}
}

func TestFleetListRingViewsAgreeInTextAndJSON(t *testing.T) {
	for _, tc := range []struct {
		noun, dir, shared, cloud, local string
		root                            func(...Option) *cobra.Command
		add                             func(*Catalog) error
	}{
		{"tool", dirTools, "shared-list-tool", "cloud-list-tool", "local-list-tool", NewToolsCmd,
			func(c *Catalog) error { return c.SaveTool(Tool{Name: "local-list-tool", Kind: ToolKindCLI}) }},
		{"model", dirModels, "shared-list-model", "cloud-list-model", "local-list-model", NewModelsCmd,
			func(c *Catalog) error {
				return c.SaveModel(Model{Name: "local-list-model", Kind: ModelKindAPI, Source: ModelSourceCloud})
			}},
		{"agent", dirAgents, "shared-list-agent", "cloud-list-agent", "local-list-agent", NewAgentsCmd,
			func(c *Catalog) error {
				return c.SaveAgent(Agent{Name: "local-list-agent", Tool: "codex", Model: "gpt6-sol"})
			}},
	} {
		t.Run(tc.noun, func(t *testing.T) {
			root := t.TempDir()
			body := func(name string) string {
				switch tc.noun {
				case "tool":
					return "name: " + name + "\nkind: cli\n"
				case "model":
					return "name: " + name + "\nkind: api\nsource: cloud\n"
				default:
					return "agents:\n  - name: " + name + "\n    tool: codex\n    model: gpt6-sol\n"
				}
			}
			opts := []Option{WithRoot(root), WithoutCloudOverlay(),
				WithSource(tc.dir, assetring.FileFS(fstest.MapFS{tc.shared + ".yaml": {Data: []byte(body(tc.shared))}}, assetring.RingShared, ext)),
				WithSource(tc.dir, assetring.FileFS(fstest.MapFS{tc.cloud + ".yaml": {Data: []byte(body(tc.cloud))}}, assetring.RingCloud, ext))}
			if err := tc.add(New(opts...)); err != nil {
				t.Fatal(err)
			}
			for _, view := range []struct {
				args                         []string
				shared, cloud, local, hidden bool
			}{
				{nil, true, true, true, false},
				{[]string{"--custom"}, false, false, true, false},
				{[]string{"--all"}, true, true, true, false},
				{[]string{"--ring", "all"}, true, true, true, false},
				{[]string{"--ring", "embedded"}, false, false, false, false},
				{[]string{"--ring", "shared"}, true, false, false, false},
				{[]string{"--ring", "cloud"}, false, true, false, false},
				{[]string{"--ring", "local"}, false, false, true, false},
			} {
				for _, format := range []string{"text", "json"} {
					args := append([]string{"list"}, view.args...)
					if format == "json" {
						args = append(args, "--json")
					}
					cmd := tc.root(opts...)
					var out, errOut bytes.Buffer
					cmd.SetOut(&out)
					cmd.SetErr(&errOut)
					cmd.SetArgs(args)
					if err := cmd.Execute(); err != nil {
						t.Fatalf("%v: %v; stderr: %s", args, err, errOut.String())
					}
					found := map[string]string{}
					if format == "json" {
						rows := decodeListItems[struct{ Name, Ring, Source string }](t, out.Bytes())
						for _, row := range rows {
							found[row.Name] = row.Ring
							if tc.noun == "model" && (row.Name == tc.local || row.Name == tc.shared || row.Name == tc.cloud) && row.Source != ModelSourceCloud {
								t.Errorf("%v: model %s source = %q, want cloud inference path", args, row.Name, row.Source)
							}
						}
						if strings.Contains(errOut.String(), "custom entries hidden") {
							t.Errorf("%v: JSON printed a prose hint: %s", args, errOut.String())
						}
					} else {
						for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n")[1:] {
							fields := strings.Fields(line)
							if len(fields) > 1 {
								found[fields[0]] = fields[len(fields)-1]
							}
						}
						wantHint := "1 custom entries hidden — --custom to show"
						if (strings.TrimSpace(errOut.String()) == wantHint) != view.hidden {
							t.Errorf("%v: stderr = %q", args, errOut.String())
						}
					}
					for _, entry := range []struct {
						name, ring string
						want       bool
					}{
						{tc.shared, "shared", view.shared}, {tc.cloud, "cloud", view.cloud}, {tc.local, "local", view.local},
					} {
						got, ok := found[entry.name]
						if ok != entry.want || ok && got != entry.ring {
							t.Errorf("%v %s: %s ring = %q, present = %v; want %v/%s", args, format, entry.name, got, ok, entry.want, entry.ring)
						}
					}
				}
			}
		})
	}
}

func TestFleetListFilterHelpAndConflicts(t *testing.T) {
	for _, root := range []func(...Option) *cobra.Command{NewToolsCmd, NewModelsCmd, NewAgentsCmd} {
		for _, args := range [][]string{{"--help"}, {"list", "--help"}} {
			out, err := runCmd(t, root(WithRoot(t.TempDir())), args...)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"--custom", "--all", "--builtin", "--active", "--ring", "embedded", "shared", "cloud", "local"} {
				if !strings.Contains(out, want) {
					t.Errorf("%v help missing %q", args, want)
				}
			}
		}
		for _, args := range [][]string{{"list", "--ring", "bogus"}, {"list", "--all", "--custom"}, {"list", "--ring", "local", "--custom"}, {"list", "--builtin", "--active"}, {"list", "--active", "--custom"}} {
			if _, err := runCmd(t, root(WithRoot(t.TempDir())), args...); err == nil {
				t.Errorf("%v accepted", args)
			}
		}
	}
}
