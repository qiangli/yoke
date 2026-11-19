package fleet

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The dsh seed (graduated from bashy examples/fleet in Sprint 671) launches
// headless and over ACP, takes its model through the launch, and writes
// $DSH_HOME/cordis.patch.yml in its state dir: the llm-pi-ai route for an
// OpenAI-compatible gateway, the deepseek-official route without one.
func TestDshSeedDeclarationAndSetup(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	tool, ok := New(WithRoot(t.TempDir()), WithoutCloudOverlay()).Tool("dsh")
	if !ok {
		t.Fatal("dsh seed missing")
	}
	if tool.IsRetired() {
		t.Fatal("dsh seed is retired")
	}
	if got := tool.CLI.Launch.Exec; got != "dsh --profile headless {prompt}" {
		t.Errorf("exec = %q", got)
	}
	if got := tool.CLI.Launch.ACPExec; got != "dsh --profile acp" {
		t.Errorf("acp_exec = %q", got)
	}
	if !tool.ModelViaLaunch() || !tool.UsesStateDir() {
		t.Errorf("ModelViaLaunch=%v UsesStateDir=%v, want both", tool.ModelViaLaunch(), tool.UsesStateDir())
	}

	run := func(vars LaunchVars) string {
		t.Helper()
		script := tool.SetupScript(vars)
		if out, err := exec.Command(sh, "-e", "-c", script).CombinedOutput(); err != nil {
			t.Fatalf("setup failed: %v\n%s\nscript:\n%s", err, out, script)
		}
		raw, err := os.ReadFile(filepath.Join(vars.StateDir, "cordis.patch.yml"))
		if err != nil {
			t.Fatalf("cordis.patch.yml not written: %v", err)
		}
		var parsed []map[string]any
		if err := yaml.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("cordis.patch.yml is not YAML: %v\n%s", err, raw)
		}
		return string(raw)
	}

	gw := run(LaunchVars{StateDir: filepath.Join(t.TempDir(), "dsh state"), BaseURL: "http://127.0.0.1:11434/v1", Model: "qwen3.5:9b"})
	for _, want := range []string{"llm-pi-ai", "http://127.0.0.1:11434/v1", "qwen3.5:9b"} {
		if !strings.Contains(gw, want) {
			t.Errorf("gateway patch missing %q:\n%s", want, gw)
		}
	}
	native := run(LaunchVars{StateDir: t.TempDir(), Model: "deepseek-v4-pro"})
	for _, want := range []string{"deepseek-official", "deepseek-v4-pro"} {
		if !strings.Contains(native, want) {
			t.Errorf("native patch missing %q:\n%s", want, native)
		}
	}
}
