package fleet

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The zcode seed writes its provider config in a setup snippet. SetupScript
// shell-quotes every substituted value, so the snippet must use the tokens as
// shell WORDS — never inside a heredoc or a double-quoted string, where the
// quotes would land literally in the path or the JSON.
func TestZcodeSetupWritesValidProviderConfig(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	tool, ok := New(WithRoot(t.TempDir()), WithoutCloudOverlay()).Tool("zcode")
	if !ok {
		t.Fatal("zcode seed missing")
	}
	state := filepath.Join(t.TempDir(), "zc state")
	script := tool.SetupScript(LaunchVars{Model: "glm-5.3", BaseURL: "https://api.example.test/v4", StateDir: state})
	cmd := exec.Command(sh, "-c", script)
	cmd.Env = append(os.Environ(), "ZCODE_API_KEY=k-123")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("setup failed: %v\n%s\nscript:\n%s", err, out, script)
	}
	raw, err := os.ReadFile(filepath.Join(state, "provider_config.json"))
	if err != nil {
		t.Fatalf("provider config not written where the launch env points: %v\nscript:\n%s", err, script)
	}
	var cfg struct {
		Providers []struct {
			BaseURL string `json:"baseUrl"`
			Access  struct {
				APIKey string `json:"apiKey"`
			} `json:"access"`
			Models []string `json:"personalModelIds"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("provider config is not JSON: %v\n%s", err, raw)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("providers = %+v", cfg.Providers)
	}
	p := cfg.Providers[0]
	if p.BaseURL != "https://api.example.test/v4" || p.Access.APIKey != "k-123" || len(p.Models) != 1 || p.Models[0] != "glm-5.3" {
		t.Fatalf("provider config = %s", raw)
	}
}

// The steerable launch opens zcode's interactive TUI on its own default
// model: no -p/{prompt} one-shot and no {model} (zcode has no --model flag;
// the setup's provider config in {state_dir} supplies the model).
func TestZcodeSeedSteerExec(t *testing.T) {
	tool, ok := New(WithRoot(t.TempDir()), WithoutCloudOverlay()).Tool("zcode")
	if !ok {
		t.Fatal("zcode seed missing")
	}
	if got := tool.CLI.Launch.SteerExec; got != "zcode --mode yolo" {
		t.Errorf("steer_exec = %q, want zcode --mode yolo", got)
	}
	argv, ok := tool.SteerArgvPrefix("glm-5.3")
	if !ok {
		t.Fatal("zcode is not steerable")
	}
	if got := strings.Join(argv, " "); got != "--mode yolo" {
		t.Errorf("steer argv = %q, want --mode yolo (bound model must not be injected)", got)
	}
	if tool.SteerTakesPrompt() {
		t.Error("zcode steer must not take a prompt")
	}
}
