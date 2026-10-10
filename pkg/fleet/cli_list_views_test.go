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

// viewFixture builds a catalog with one entry per ring for every noun, on top
// of the real baseline. Names are ring-explicit so text and JSON agree by eye.
func viewFixture(t *testing.T, noun string) (root string, opts []Option) {
	t.Helper()
	root = t.TempDir()
	body := func(name string) string {
		switch noun {
		case "tool":
			return "name: " + name + "\nkind: cli\ncli:\n  binary: " + name + "\n  launch:\n    exec: " + name + " {prompt}\n"
		case "model":
			return "name: " + name + "\nkind: local\nsource: cloud\n"
		default:
			return "agents:\n  - name: " + name + "\n    tool: codex\n    model: gpt6-sol\n"
		}
	}
	dir := map[string]string{"tool": dirTools, "model": dirModels, "agent": dirAgents}[noun]
	opts = []Option{WithRoot(root), WithoutCloudOverlay(),
		WithSource(dir, assetring.FileFS(fstest.MapFS{
			"shared-view.yaml": {Data: []byte(body("shared-view"))},
		}, assetring.RingShared, ext)),
		WithSource(dir, assetring.FileFS(fstest.MapFS{
			"cloud-view.yaml": {Data: []byte(body("cloud-view"))},
		}, assetring.RingCloud, ext))}
	save := map[string]func(*Catalog) error{
		"tool": func(c *Catalog) error {
			return c.SaveTool(Tool{Name: "local-view", Kind: ToolKindCLI})
		},
		"model": func(c *Catalog) error {
			return c.SaveModel(Model{Name: "local-view", Kind: ModelKindLocal, Source: ModelSourceCloud})
		},
		"agent": func(c *Catalog) error {
			return c.SaveAgent(Agent{Name: "local-view", Tool: "codex", Model: "gpt6-sol"})
		},
	}[noun]
	if err := save(New(opts...)); err != nil {
		t.Fatal(err)
	}
	return root, opts
}

func listNames(t *testing.T, root func(...Option) *cobra.Command, opts []Option, args ...string) (names []string, stderr string) {
	t.Helper()
	cmd := root(opts...)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(append([]string{"list", "--json"}, args...))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v: %v; stderr: %s", args, err, errOut.String())
	}
	rows := decodeListItems[struct{ Name string }](t, out.Bytes())
	for _, r := range rows {
		names = append(names, r.Name)
	}
	return names, errOut.String()
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// The default view is the selected entry per name across ALL rings: a local
// entry is shown, not hidden behind a hint.
func TestListDefaultShowsEveryRing(t *testing.T) {
	for noun, root := range map[string]func(...Option) *cobra.Command{
		"tool": NewToolsCmd, "model": NewModelsCmd, "agent": NewAgentsCmd,
	} {
		t.Run(noun, func(t *testing.T) {
			_, opts := viewFixture(t, noun)
			names, stderr := listNames(t, root, opts)
			for _, want := range []string{"shared-view", "cloud-view", "local-view"} {
				if !hasName(names, want) {
					t.Errorf("default view missing %q: %v", want, names)
				}
			}
			if strings.Contains(stderr, "hidden") {
				t.Errorf("default view printed a hidden hint: %q", stderr)
			}
			// Text agrees with JSON.
			cmd := root(opts...)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"list"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"shared-view", "cloud-view", "local-view"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("text view missing %q:\n%s", want, out.String())
				}
			}
		})
	}
}

// --builtin is the embedded ring, spelled as a view.
func TestListBuiltinMatchesRingEmbedded(t *testing.T) {
	for noun, root := range map[string]func(...Option) *cobra.Command{
		"tool": NewToolsCmd, "model": NewModelsCmd, "agent": NewAgentsCmd,
	} {
		t.Run(noun, func(t *testing.T) {
			_, opts := viewFixture(t, noun)
			builtin, _ := listNames(t, root, opts, "--builtin")
			embedded, _ := listNames(t, root, opts, "--ring", "embedded")
			if strings.Join(builtin, ",") != strings.Join(embedded, ",") {
				t.Errorf("--builtin = %v, --ring embedded = %v", builtin, embedded)
			}
			if hasName(builtin, "local-view") || hasName(builtin, "shared-view") || hasName(builtin, "cloud-view") {
				t.Errorf("--builtin leaked a non-embedded entry: %v", builtin)
			}
		})
	}
}

// The views are alternatives: any pair is refused.
func TestListViewsAreMutuallyExclusive(t *testing.T) {
	for _, root := range []func(...Option) *cobra.Command{NewToolsCmd, NewModelsCmd, NewAgentsCmd} {
		for _, args := range [][]string{
			{"list", "--builtin", "--custom"},
			{"list", "--builtin", "--all"},
			{"list", "--builtin", "--ring", "local"},
			{"list", "--builtin", "--active"},
			{"list", "--builtin", "--retired"},
			{"list", "--active", "--custom"},
			{"list", "--active", "--all"},
			{"list", "--active", "--ring", "local"},
			{"list", "--active", "--retired"},
			{"list", "--retired", "--custom"},
		} {
			if _, err := runCmd(t, root(WithRoot(t.TempDir())), args...); err == nil {
				t.Errorf("%v accepted", args)
			}
		}
	}
}

// --active lists what is usable on this host now: a tool whose binary
// resolves, a model whose credential is present, an agent whose halves are.
func TestListActiveShowsOnlyUsableEntries(t *testing.T) {
	binDir := t.TempDir()
	present := filepath.Join(binDir, "present-tool")
	if err := os.WriteFile(present, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("VIEWACTIVE_API_KEY", "test-credential")

	root := t.TempDir()
	opts := []Option{WithRoot(root), WithoutCloudOverlay(), WithBaselineFS(fstest.MapFS{})}
	cat := New(opts...)
	if err := cat.SaveTool(Tool{Name: "present-tool", Kind: ToolKindCLI, CLI: ToolCLI{Binary: "present-tool"}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveTool(Tool{Name: "absent-tool", Kind: ToolKindCLI, CLI: ToolCLI{Binary: "absent-tool"}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(Model{Name: "keyed-model", Kind: ModelKindAPI, APIKeyRef: "viewactive"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(Model{Name: "unkeyed-model", Kind: ModelKindAPI, APIKeyRef: "viewactive-missing"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(Model{Name: "local-model", Kind: ModelKindLocal}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(Agent{Name: "live-agent", Tool: "present-tool", Model: "keyed-model"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(Agent{Name: "dead-tool-agent", Tool: "absent-tool", Model: "keyed-model"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(Agent{Name: "dead-model-agent", Tool: "present-tool", Model: "unkeyed-model"}); err != nil {
		t.Fatal(err)
	}

	tools, _ := listNames(t, NewToolsCmd, opts, "--active")
	if !hasName(tools, "present-tool") || hasName(tools, "absent-tool") {
		t.Errorf("--active tools = %v", tools)
	}
	models, _ := listNames(t, NewModelsCmd, opts, "--active")
	if !hasName(models, "keyed-model") || !hasName(models, "local-model") || hasName(models, "unkeyed-model") {
		t.Errorf("--active models = %v", models)
	}
	agents, _ := listNames(t, NewAgentsCmd, opts, "--active")
	if !hasName(agents, "live-agent") || hasName(agents, "dead-tool-agent") || hasName(agents, "dead-model-agent") {
		t.Errorf("--active agents = %v", agents)
	}
	// The same rows in text.
	out, err := runCmd(t, NewAgentsCmd(opts...), "list", "--active")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "live-agent") || strings.Contains(out, "dead-tool-agent") || strings.Contains(out, "dead-model-agent") {
		t.Errorf("--active text disagrees with JSON:\n%s", out)
	}
}

// A hidden tool stays resolvable but leaves default and --active lists, and
// agents bound to it leave with it.
func TestHiddenToolHidesBoundAgentsToo(t *testing.T) {
	// loud-tool resolves on PATH so the --active leg can tell hidden apart
	// from merely uninstalled.
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "loud-tool"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	root := t.TempDir()
	opts := []Option{WithRoot(root), WithoutCloudOverlay(), WithBaselineFS(fstest.MapFS{})}
	cat := New(opts...)
	if err := cat.SaveTool(Tool{Name: "quiet-tool", Kind: ToolKindCLI, Hidden: true}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveTool(Tool{Name: "loud-tool", Kind: ToolKindCLI, CLI: ToolCLI{Binary: "loud-tool"}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(Model{Name: "m", Kind: ModelKindLocal}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(Agent{Name: "quiet-agent", Tool: "quiet-tool", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(Agent{Name: "loud-agent", Tool: "loud-tool", Model: "m"}); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{nil, {"--active"}} {
		tools, _ := listNames(t, NewToolsCmd, opts, args...)
		if hasName(tools, "quiet-tool") || !hasName(tools, "loud-tool") {
			t.Errorf("%v tools = %v", args, tools)
		}
		agents, _ := listNames(t, NewAgentsCmd, opts, args...)
		if hasName(agents, "quiet-agent") || !hasName(agents, "loud-agent") {
			t.Errorf("%v agents = %v", args, agents)
		}
	}
	// Explicit views still show both.
	for _, args := range [][]string{{"--all"}, {"--custom"}, {"--ring", "local"}} {
		tools, _ := listNames(t, NewToolsCmd, opts, args...)
		if !hasName(tools, "quiet-tool") {
			t.Errorf("%v must show the hidden tool: %v", args, tools)
		}
		agents, _ := listNames(t, NewAgentsCmd, opts, args...)
		if !hasName(agents, "quiet-agent") {
			t.Errorf("%v must show the agent on the hidden tool: %v", args, agents)
		}
	}
	// Still resolvable and verifiable by explicit name.
	if _, ok := New(opts...).Tool("quiet-tool"); !ok {
		t.Error("a hidden tool must stay resolvable")
	}
}

// The cloud pull applies the same hidden filter: a hidden tool never lands
// in the ring, alongside the function kits sync already skips.
func TestSyncToolsSkipsHidden(t *testing.T) {
	client, _ := fakePlane(t, map[string]string{
		"/api/v1/tools": `{"tools":[
		  {"name":"loud","content":"name: loud\nkind: cli\n"},
		  {"name":"quiet","content":"name: quiet\nkind: cli\nhidden: true\n"}]}`,
	})
	root := t.TempDir()
	res, err := client.Sync(CloudCacheRoot(root), dirTools)
	if err != nil {
		t.Fatal(err)
	}
	if res.Fetched != 1 || res.Skipped != 1 {
		t.Fatalf("fetched=%d skipped=%d — the hidden tool must be skipped and reported", res.Fetched, res.Skipped)
	}
	c := New(WithRoot(root))
	if _, ok := c.Tool("quiet"); ok {
		t.Fatal("a hidden tool leaked into the tool ring")
	}
	if _, ok := c.Tool("loud"); !ok {
		t.Fatal("the visible tool must sync")
	}
}
