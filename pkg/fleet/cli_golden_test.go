package fleet

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The registry CLI's observable output, pinned byte for byte before the
// resource-kind core refactor (Sprint 406 story e018942b): the refactor must
// leave every list/show/schema rendering of tool, model, agent, command and
// app unchanged unless a golden file is updated on purpose, in the same
// change, with the reason in its message.
//
//	go test ./pkg/fleet -run TestRegistryCLIGolden -update-golden
var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/golden/cli/*.golden from the current CLI output")

// goldenEnv isolates the catalog: no seeded roster, no shared rings, a
// private fleet store and BASHY_HOME, so only the fixtures below (plus the
// embedded tool launch contracts, which `--custom` views exclude) are visible.
func goldenEnv(t *testing.T) (root string, opts []Option) {
	t.Helper()
	root = t.TempDir()
	t.Setenv("BASHY_FLEET_SEEDS", "off")
	t.Setenv("BASHY_FLEET_DIR", root)
	t.Setenv("BASHY_HOME", filepath.Join(root, "home"))
	for _, k := range []string{
		"BASHY_TOOLS_PATH", "BASHY_MODELS_PATH", "BASHY_AGENTS_PATH", "BASHY_COMMANDS_PATH",
		"BASHY_TOOLS_DIR", "BASHY_MODELS_DIR", "BASHY_AGENTS_DIR", "BASHY_COMMANDS_DIR",
		"BASHY_PEOPLE_DIR", "BASHY_HOSTS_DIR", "BASHY_APPS_DIR",
	} {
		t.Setenv(k, "")
	}
	return root, []Option{WithRoot(root)}
}

// goldenFixtures registers one entry per noun through the real add verbs.
func goldenFixtures(t *testing.T, root string, opts []Option) {
	t.Helper()
	tool := filepath.Join(root, "gt-tool.yaml")
	if err := os.WriteFile(tool, []byte(`name: gt-tool
kind: cli
display: Golden Tool
cli:
  binary: gt-tool
  launch:
    exec: gt-tool --yolo -m {model} {prompt}
    prompt_position: last
    key_env: [OPENAI_API_KEY]
    env: ["OPENAI_BASE_URL={base_url}"]
quirks: fixture
integration:
  detect: ~/.gt-tool
  skills:
    user: [~/.agents/skills]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		cmd  func() *cobra.Command
		args []string
	}{
		{func() *cobra.Command { return NewToolsCmd(opts...) }, []string{"add", tool}},
		{func() *cobra.Command { return NewModelsCmd(opts...) }, []string{"add", "gt-model",
			"--provider", "openai-compat", "--kind", "api", "--upstream", "gt-upstream-1",
			"--base-url", "https://api.example.test/v1", "--api-key-ref", "gtkey",
			"--band", "3", "--band-source", "declared", "--family", "gt", "--version", "1", "--alias", "gt-alias"}},
		{func() *cobra.Command { return NewAgentsCmd(opts...) }, []string{"add", "gt-agent",
			"--tool", "gt-tool", "--model", "gt-model", "--nick", "Goldie", "--description", "golden fixture agent"}},
		{func() *cobra.Command { return NewCommandsCmd(opts...) }, []string{"add", "gt-cmd",
			"--set", "exec.0=gt-external", "--set", "synopsis=golden fixture command", "--set", "effects.0=read"}},
		{func() *cobra.Command { return appRoot(opts) }, []string{"add", "gt-app", "--port", "18080"}},
	}
	for _, s := range steps {
		if out, err := runCmd(t, s.cmd(), s.args...); err != nil {
			t.Fatalf("fixture %v: %v\n%s", s.args, err, out)
		}
	}
}

// appRoot mounts the app verbs under a root, as the console does.
func appRoot(opts []Option) *cobra.Command {
	root := &cobra.Command{Use: "app", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(NewAppCmds(opts...)...)
	return root
}

func TestRegistryCLIGolden(t *testing.T) {
	root, opts := goldenEnv(t)
	goldenFixtures(t, root, opts)

	nouns := map[string]func() *cobra.Command{
		"tool":    func() *cobra.Command { return NewToolsCmd(opts...) },
		"model":   func() *cobra.Command { return NewModelsCmd(opts...) },
		"agent":   func() *cobra.Command { return NewAgentsCmd(opts...) },
		"command": func() *cobra.Command { return NewCommandsCmd(opts...) },
		"app":     func() *cobra.Command { return appRoot(opts) },
	}
	cases := []struct {
		noun, name string
		args       []string
	}{
		{"tool", "list-custom", []string{"list", "--custom"}},
		{"tool", "list-custom-json", []string{"list", "--custom", "--json"}},
		{"tool", "show", []string{"show", "gt-tool"}},
		{"tool", "show-json", []string{"show", "gt-tool", "--json"}},
		{"tool", "schema-json", []string{"schema", "--json"}},
		{"model", "list-custom", []string{"list", "--custom"}},
		{"model", "list-custom-json", []string{"list", "--custom", "--json"}},
		{"model", "show", []string{"show", "gt-model"}},
		{"model", "show-json", []string{"show", "gt-model", "--json"}},
		{"model", "schema-json", []string{"schema", "--json"}},
		{"agent", "list-custom", []string{"list", "--custom"}},
		{"agent", "list-custom-json", []string{"list", "--custom", "--json"}},
		{"agent", "show", []string{"show", "gt-agent"}},
		{"agent", "show-json", []string{"show", "gt-agent", "--json"}},
		{"agent", "schema-json", []string{"schema", "--json"}},
		{"command", "list", []string{"list"}},
		{"command", "list-json", []string{"list", "--json"}},
		{"command", "show-yaml", []string{"show", "gt-cmd", "--yaml"}},
		{"command", "show-json", []string{"show", "gt-cmd", "--json"}},
		{"command", "schema-json", []string{"schema", "--json"}},
		{"app", "show", []string{"show", "gt-app"}},
		{"app", "show-json", []string{"show", "gt-app", "--json"}},
		{"app", "schema-json", []string{"schema", "--json"}},
	}
	for _, tc := range cases {
		t.Run(tc.noun+"/"+tc.name, func(t *testing.T) {
			out, err := runCmd(t, nouns[tc.noun](), tc.args...)
			if err != nil {
				t.Fatalf("%v: %v\n%s", tc.args, err, out)
			}
			checkGolden(t, filepath.Join("testdata", "golden", "cli", tc.noun+"_"+tc.name+".golden"), normalizeGolden(out, root))
		})
	}
}

// normalizeGolden removes what differs between runs and hosts: the temp
// store root and the user's home directory.
func normalizeGolden(out, root string) string {
	out = strings.ReplaceAll(out, root, "<ROOT>")
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = strings.ReplaceAll(out, home, "<HOME>")
	}
	return out
}

func checkGolden(t *testing.T, path, got string) {
	t.Helper()
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (run with -update-golden): %v", path, err)
	}
	if string(want) != got {
		t.Errorf("%s changed. If intended, rerun with -update-golden and explain why in the commit.\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}
