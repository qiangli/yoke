package agentlaunch

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/secrets"
)

// A custom OpenAI-compatible CLI bound to a custom endpoint model: the launch
// carries the model's endpoint and id in the tool's declared env, and the
// model's credential under the protocol's key name whatever the model's ref.
func TestCustomToolReceivesBoundModelEndpointAndKey(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveTool(fleet.Tool{
		Name: "qwenish", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Binary: "qwenish", Launch: fleet.ToolLaunch{
			Exec:   "qwenish --yolo -m {model} {prompt}",
			KeyEnv: []string{"OPENAI_API_KEY"},
			Env: []string{
				"OPENAI_BASE_URL={base_url}",
				"PROVIDER_HOST={base_url_origin}",
				"PROVIDER_PATH={base_url_path}/chat/completions",
				"QWENISH_MODE=auto",
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{
		Name: "e2e-glm", Kind: "api", Provider: "openai-compat",
		UpstreamID: "glm-5.3", BaseURL: "https://api.example.test/api/coding/paas/v4/",
		APIKeyRef: "zai",
	}); err != nil {
		t.Fatal(err)
	}

	l, err := ResolveWithCatalog("qwenish:e2e-glm", Options{AllowUnsafe: true, DryRun: true}, testCatalog(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"OPENAI_BASE_URL=https://api.example.test/api/coding/paas/v4/",
		"PROVIDER_HOST=https://api.example.test",
		"PROVIDER_PATH=/api/coding/paas/v4/chat/completions",
		"QWENISH_MODE=auto",
	} {
		if !slices.Contains(l.Env, want) {
			t.Errorf("launch env missing %q: %q", want, l.Env)
		}
	}
	if got, want := l.CredentialEnvAliases["OPENAI_API_KEY"], secrets.CredentialEnvNames("zai"); !slices.Equal(got, want) {
		t.Fatalf("OPENAI_API_KEY sources = %q, want %q (aliases %v)", got, want, l.CredentialEnvAliases)
	}
	if slices.ContainsFunc(l.Env, func(kv string) bool { return strings.Contains(kv, "{") }) {
		t.Fatalf("unrendered placeholder in env: %q", l.Env)
	}
}

// A tool that reads its model from the environment (no {model} in argv) can
// still be bound: the binding selects the model through the env, and the
// model's credential is granted. Before, bashy refused the binding outright
// ("a label, not a selection") and such a tool never received a key.
func TestToolSelectingModelFromEnvCanBeBound(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveTool(fleet.Tool{
		Name: "handsy", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Binary: "handsy", Launch: fleet.ToolLaunch{
			Exec:   "handsy --headless -t {prompt}",
			KeyEnv: []string{"LLM_API_KEY"},
			Env:    []string{"LLM_MODEL={model}", "LLM_BASE_URL={base_url}"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{
		Name: "local-qwen", Kind: "local", Provider: "openai-compat",
		UpstreamID: "qwen3.5:9b", BaseURL: "http://127.0.0.1:11434/v1",
		APIKeyRef: "ollama", ToolIDs: map[string]string{"handsy": "openai/qwen3.5:9b"},
	}); err != nil {
		t.Fatal(err)
	}
	tool, _ := cat.Tool("handsy")
	if !tool.TakesModel() {
		t.Fatal("a {model} in the launch env must count as model selection")
	}
	l, err := ResolveWithCatalog("handsy:local-qwen", Options{AllowUnsafe: true, DryRun: true}, testCatalog(root))
	if err != nil {
		t.Fatalf("binding refused: %v", err)
	}
	if !slices.Contains(l.Env, "LLM_MODEL=openai/qwen3.5:9b") || !slices.Contains(l.Env, "LLM_BASE_URL=http://127.0.0.1:11434/v1") {
		t.Fatalf("launch env = %q", l.Env)
	}
	if _, ok := l.CredentialEnvAliases["LLM_API_KEY"]; !ok {
		t.Fatalf("credential not projected: %v", l.CredentialEnvAliases)
	}
	if slices.Contains(l.Args, "openai/qwen3.5:9b") {
		t.Fatalf("model leaked into argv: %q", l.Args)
	}
}

// A placeholder with no value for this launch drops the pair instead of
// setting it empty, so the tool's own default survives an unbound launch.
func TestLaunchEnvDropsPairsWithoutValue(t *testing.T) {
	tool := fleet.Tool{CLI: fleet.ToolCLI{Launch: fleet.ToolLaunch{Env: []string{
		"A={model}", "B={base_url}", "C=fixed", "D={base_url_origin}",
	}}}}
	got := tool.LaunchEnv(fleet.LaunchVars{})
	if !slices.Equal(got, []string{"C=fixed"}) {
		t.Fatalf("LaunchEnv(unbound) = %q", got)
	}
	got = tool.LaunchEnv(fleet.LaunchVars{Model: "m1", BaseURL: "not a url"})
	if !slices.Equal(got, []string{"A=m1", "B=not a url", "C=fixed"}) {
		t.Fatalf("LaunchEnv(bad url) = %q", got)
	}
}

// A tool whose provider config must be written before it starts (crush,
// hermes, forge, zcode) declares a setup snippet. The launch runs it through
// bashy and then execs the tool, so every consumer gets it; a {model} in the
// setup alone makes the tool model-selecting.
func TestCustomToolSetupWrapsLaunch(t *testing.T) {
	orig := SetupShell
	SetupShell = func() (string, error) { return "/opt/bashy", nil }
	t.Cleanup(func() { SetupShell = orig })

	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveTool(fleet.Tool{
		Name: "forgeish", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Binary: "forgeish", Launch: fleet.ToolLaunch{
			Exec:   "forgeish -p {prompt}",
			KeyEnv: []string{"OPENAI_API_KEY"},
			Setup:  "forgeish config set model openai_compatible {model} </dev/null",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{
		Name: "odd", Kind: "api", Provider: "openai-compat",
		UpstreamID: "it's-odd", BaseURL: "https://api.example.test/v1", APIKeyRef: "zai",
	}); err != nil {
		t.Fatal(err)
	}
	l, err := ResolveWithCatalog("forgeish:odd", Options{AllowUnsafe: true, DryRun: true}, testCatalog(root))
	if err != nil {
		t.Fatalf("binding refused: %v", err)
	}
	if l.Tool != "/opt/bashy" {
		t.Fatalf("tool = %q, want the setup shell", l.Tool)
	}
	want := []string{"--bashsharp", "-c", "forgeish config set model openai_compatible 'it'\\''s-odd' </dev/null\nexec \"$@\"", "forgeish-setup", "forgeish", "-p"}
	if !slices.Equal(l.Args, want) {
		t.Fatalf("args = %q\nwant  %q", l.Args, want)
	}
	if !l.TakesPrompt {
		t.Fatal("the prompt must still be appended (it becomes the last \"$@\" argument)")
	}
}

// {state_dir} gives a setup a bashy-owned directory private to the binding,
// so the CLI is pointed at bashy's copy of its config and the user's own
// configuration of the same CLI is never rewritten.
func TestCustomToolStateDirIsPrivatePerBinding(t *testing.T) {
	orig := SetupShell
	SetupShell = func() (string, error) { return "/opt/bashy", nil }
	t.Cleanup(func() { SetupShell = orig })
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)

	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveTool(fleet.Tool{
		Name: "hermesish", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Binary: "hermesish", Launch: fleet.ToolLaunch{
			Exec:  "hermesish -m {model} -z {prompt}",
			Env:   []string{"HERMESISH_HOME={state_dir}"},
			Setup: "mkdir -p {state_dir} && printf 'base_url: %s\\n' {base_url} > {state_dir}/config.yaml",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{
		Name: "glm-x", Kind: "api", Provider: "openai-compat", UpstreamID: "glm-5.3",
		BaseURL: "https://api.example.test/v1", APIKeyRef: "zai",
	}); err != nil {
		t.Fatal(err)
	}
	l, err := ResolveWithCatalog("hermesish:glm-x", Options{AllowUnsafe: true}, testCatalog(root))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "tool-state", "hermesish", "glm-x")
	if !slices.Contains(l.Env, "HERMESISH_HOME="+dir) {
		t.Fatalf("env = %q, want HERMESISH_HOME=%s", l.Env, dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("state dir %s: %v %v", dir, fi, err)
	}
	if !strings.Contains(l.Args[2], "'"+dir+"'/config.yaml") {
		t.Fatalf("setup = %q", l.Args[1])
	}
}

// A CLI whose model flag takes provider/model (crush -m bashy/{model},
// openclaw --model bashy/{model}) gets the bound model inside the token.
func TestModelPlaceholderInsideToken(t *testing.T) {
	tool := fleet.Tool{CLI: fleet.ToolCLI{Binary: "crushish", Launch: fleet.ToolLaunch{
		Exec: "crushish run -q -m bashy/{model} {prompt}",
	}}}
	got := tool.Argv("glm-5.3", "hi")
	if want := []string{"crushish", "run", "-q", "-m", "bashy/glm-5.3", "hi"}; !slices.Equal(got, want) {
		t.Fatalf("bound argv = %q, want %q", got, want)
	}
	got = tool.Argv("", "hi")
	if want := []string{"crushish", "run", "-q", "hi"}; !slices.Equal(got, want) {
		t.Fatalf("unbound argv = %q, want %q", got, want)
	}
}

// F11: ACP has no model selection, so a bound model used to refuse ACP. A
// tool that receives its model through the launch env (or setup) carries the
// binding outside the protocol, so ACP honours it; an argv-only tool is still
// refused rather than silently running its own default model.
func TestACPCarriesBindingThroughLaunchEnv(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	for _, tl := range []fleet.Tool{
		{Name: "envacp", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: "envacp", Launch: fleet.ToolLaunch{
			Exec: "envacp --headless -t {prompt}", ACPExec: "envacp acp",
			KeyEnv: []string{"LLM_API_KEY"}, Env: []string{"LLM_MODEL={model}"},
		}}},
		{Name: "argvacp", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: "argvacp", Launch: fleet.ToolLaunch{
			Exec: "argvacp -m {model} {prompt}", ACPExec: "argvacp --acp",
		}}},
	} {
		if err := cat.SaveTool(tl); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.SaveModel(fleet.Model{Name: "m1", Kind: "api", Provider: "openai-compat",
		UpstreamID: "glm-5.3", BaseURL: "https://api.example.test/v1", APIKeyRef: "zai"}); err != nil {
		t.Fatal(err)
	}
	l, err := ResolveWithCatalog("envacp:m1", Options{ACP: true, AllowUnsafe: true, DryRun: true}, testCatalog(root))
	if err != nil {
		t.Fatalf("env-bound ACP refused: %v", err)
	}
	if !slices.Equal(l.Args, []string{"acp"}) || !slices.Contains(l.Env, "LLM_MODEL=glm-5.3") {
		t.Fatalf("acp launch = args %q env %q", l.Args, l.Env)
	}
	if _, err := ResolveWithCatalog("argvacp:m1", Options{ACP: true, AllowUnsafe: true, DryRun: true}, testCatalog(root)); err == nil {
		t.Fatal("argv-only tool must still refuse a bound model over ACP")
	}
}

// F16: a tool that needs a minimum context window refuses a model declaring
// less, with a message naming both numbers; an undeclared length is not
// checked (absence of data is not a refusal).
func TestMinContextRefusesSmallerModel(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveTool(fleet.Tool{Name: "bigctx", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: "bigctx", Launch: fleet.ToolLaunch{
		Exec: "bigctx -m {model} -z {prompt}", MinContext: 64000,
	}}}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []fleet.Model{
		{Name: "small", Kind: "local", Provider: "openai-compat", UpstreamID: "qwen3:8b", ContextLength: 40960},
		{Name: "large", Kind: "api", Provider: "openai-compat", UpstreamID: "glm-5.3", ContextLength: 131072},
		{Name: "unknown", Kind: "local", Provider: "openai-compat", UpstreamID: "x"},
	} {
		if err := cat.SaveModel(m); err != nil {
			t.Fatal(err)
		}
	}
	_, err := ResolveWithCatalog("bigctx:small", Options{AllowUnsafe: true, DryRun: true}, testCatalog(root))
	if err == nil || !strings.Contains(err.Error(), "64000") || !strings.Contains(err.Error(), "40960") {
		t.Fatalf("small model: err = %v", err)
	}
	for _, ok := range []string{"bigctx:large", "bigctx:unknown"} {
		if _, err := ResolveWithCatalog(ok, Options{AllowUnsafe: true, DryRun: true}, testCatalog(root)); err != nil {
			t.Fatalf("%s refused: %v", ok, err)
		}
	}
}
