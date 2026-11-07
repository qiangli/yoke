package agentlaunch

import (
	"slices"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/secrets"
)

// S584 B1: AWS Bedrock as an OPT-IN provider, data only. These tests resolve
// the seeded bedrock agents against the embedded baseline (no test ring, no
// operator overlay) and pin the argv, env and credential names each binding
// renders — and pin the NATIVE agents byte-identical, because the rule is
// that a native default launch renders exactly what it rendered before the
// bedrock seeds existed.
func bedrockBaselineCatalog(t *testing.T) CatalogFunc {
	t.Helper()
	// The shipped seeds are the whole catalog under test: no shared ring, no
	// operator overlay, seeds on, fenced local store.
	for _, k := range []string{"BASHY_TOOLS_PATH", "BASHY_MODELS_PATH", "BASHY_AGENTS_PATH", "BASHY_FLEET_SEEDS"} {
		t.Setenv(k, "")
	}
	t.Setenv("BASHY_FLEET_DIR", t.TempDir())
	t.Setenv("BASHY_HOME", t.TempDir())
	return NewCatalog
}

func TestBedrockAgentsResolve(t *testing.T) {
	newCatalog := bedrockBaselineCatalog(t)
	bearer := secrets.CredentialEnvNames("aws-bedrock")

	// claude: the model's env carries the two Bedrock switches, and the
	// credential projects under the name Claude Code reads for Bedrock.
	l, err := ResolveWithCatalog("claude-bedrock-opus4.8", Options{AllowUnsafe: true, DryRun: true}, newCatalog)
	if err != nil {
		t.Fatalf("claude-bedrock-opus4.8: %v", err)
	}
	if want := []string{"--dangerously-skip-permissions", "--model", "us.anthropic.claude-opus-4-8", "-p"}; !slices.Equal(l.Args, want) {
		t.Errorf("claude-bedrock argv = %q, want %q", l.Args, want)
	}
	for _, want := range []string{"CLAUDE_CODE_USE_BEDROCK=1", "AWS_REGION=us-east-1"} {
		if !slices.Contains(l.Env, want) {
			t.Errorf("claude-bedrock env missing %q: %q", want, l.Env)
		}
	}
	if got := l.CredentialEnvAliases["AWS_BEARER_TOKEN_BEDROCK"]; !slices.Equal(got, bearer) {
		t.Errorf("claude-bedrock AWS_BEARER_TOKEN_BEDROCK sources = %q, want %q", got, bearer)
	}

	// codex: id and credential only; the model_provider config gap is
	// documented on the model (models/bedrock-gpt6-sol.yaml), so nothing here
	// may pretend to render it. (--sandbox workspace-write renders as the
	// operator's default bypass — FinalizeArgs — exactly as the native codex
	// launch does.)
	l, err = ResolveWithCatalog("codex-bedrock-gpt6-sol", Options{AllowUnsafe: true, DryRun: true}, newCatalog)
	if err != nil {
		t.Fatalf("codex-bedrock-gpt6-sol: %v", err)
	}
	if want := []string{"exec", "--skip-git-repo-check", "--model", "openai.gpt-6-sol", "--dangerously-bypass-approvals-and-sandbox"}; !slices.Equal(l.Args, want) {
		t.Errorf("codex-bedrock argv = %q, want %q", l.Args, want)
	}
	if got := l.CredentialEnvAliases["AWS_BEARER_TOKEN_BEDROCK"]; !slices.Equal(got, bearer) {
		t.Errorf("codex-bedrock AWS_BEARER_TOKEN_BEDROCK sources = %q, want %q", got, bearer)
	}
	if slices.ContainsFunc(l.Args, func(a string) bool { return strings.Contains(a, "model_provider") }) {
		t.Errorf("codex-bedrock argv must not half-render the config gap: %q", l.Args)
	}

	// opencode: its own provider-prefixed id spelling.
	l, err = ResolveWithCatalog("opencode-bedrock-glm-5", Options{AllowUnsafe: true, DryRun: true}, newCatalog)
	if err != nil {
		t.Fatalf("opencode-bedrock-glm-5: %v", err)
	}
	if l.Model != "amazon-bedrock/zai.glm-5" {
		t.Errorf("opencode-bedrock model = %q, want amazon-bedrock/zai.glm-5", l.Model)
	}
	if want := []string{"run", "--auto", "--model", "amazon-bedrock/zai.glm-5"}; !slices.Equal(l.Args, want) {
		t.Errorf("opencode-bedrock argv = %q, want %q", l.Args, want)
	}
	if got := l.CredentialEnvAliases["AWS_BEARER_TOKEN_BEDROCK"]; !slices.Equal(got, bearer) {
		t.Errorf("opencode-bedrock AWS_BEARER_TOKEN_BEDROCK sources = %q, want %q", got, bearer)
	}

	// zcode: no credential_env — its ZCODE_API_KEY key_env projects the same
	// ref, and its existing setup writes provider_config.json from the
	// binding's {base_url}/{model}.
	l, err = ResolveWithCatalog("zcode-bedrock-glm-5", Options{AllowUnsafe: true, DryRun: true}, newCatalog)
	if err != nil {
		t.Fatalf("zcode-bedrock-glm-5: %v", err)
	}
	if l.Model != "zai.glm-5" {
		t.Errorf("zcode-bedrock model = %q, want zai.glm-5", l.Model)
	}
	if got := l.CredentialEnvAliases["ZCODE_API_KEY"]; !slices.Equal(got, bearer) {
		t.Errorf("zcode-bedrock ZCODE_API_KEY sources = %q, want %q", got, bearer)
	}
	if l.SetupWrapped == "" {
		t.Fatal("zcode-bedrock must run the zcode setup (provider_config.json)")
	}
	script := l.Args[2]
	for _, want := range []string{"https://bedrock-runtime.us-east-1.amazonaws.com/openai/v1", "zai.glm-5"} {
		if !strings.Contains(script, want) {
			t.Errorf("zcode-bedrock setup missing %q:\n%s", want, script)
		}
	}
}

// The rule the sprint rides on: a tool's NATIVE default launch renders
// byte-identical before and after the Bedrock seeds. Each row pins the exact
// argv, env and credential projection of a native agent as it rendered before
// S584 B1 (templates, managed env, the launcher's codex bypass default and
// credential spelling included).
func TestNativeAgentsRenderUnchangedBesideBedrockSeeds(t *testing.T) {
	newCatalog := bedrockBaselineCatalog(t)
	for name, want := range map[string]struct {
		args []string
		env  []string
		cred map[string][]string
	}{
		"claude-opus5.5": {
			args: []string{"--dangerously-skip-permissions", "--model", "claude-opus-5-5", "-p"},
			env: []string{
				"DISABLE_AUTOUPDATER=1",
				"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
				"DISABLE_TELEMETRY=1",
				"DO_NOT_TRACK=1",
				"DISABLE_ERROR_REPORTING=1",
				"DISABLE_GROWTHBOOK=1",
				"DISABLE_BUG_COMMAND=1",
				"DISABLE_FEEDBACK_COMMAND=1",
				"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY=1",
			},
		},
		"codex-gpt6-sol": {
			args: []string{"exec", "--skip-git-repo-check", "--model", "gpt-6-sol", "--dangerously-bypass-approvals-and-sandbox"},
			env:  nil,
		},
		"opencode-glm-5.3": {
			args: []string{"run", "--auto", "--model", "zai-coding-plan/glm-5.3"},
			env: []string{
				"OPENCODE_DISABLE_AUTOUPDATE=1",
				"OPENCODE_DISABLE_SHARE=1",
				"DO_NOT_TRACK=1",
			},
			cred: map[string][]string{"ZHIPU_API_KEY": secrets.CredentialEnvNames("zai")},
		},
		"zcode-glm-5.3": {
			// argv tail only (after the setup wrapper's shell prefix): the
			// native template, kill-switch included. The env is the tool's
			// own {state_dir} pairs — names pinned, the temp dir is not.
			args: []string{"--mode", "yolo", "-p"},
			env: []string{
				"ZCODE_DATA_BASE_DIR=",
				"ZCODE_PERSONAL_PROVIDER_CONFIG_FILE=",
			},
			cred: map[string][]string{"ZCODE_API_KEY": secrets.CredentialEnvNames("zai")},
		},
	} {
		l, err := ResolveWithCatalog(name, Options{AllowUnsafe: true, DryRun: true}, newCatalog)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		args := l.Args
		if l.SetupWrapped != "" { // unwrap: [--bashsharp, -c, script, name-setup, tool, <tail...>]
			args = args[5:]
		}
		if !slices.Equal(args, want.args) {
			t.Errorf("%s argv = %q, want %q", name, args, want.args)
		}
		switch name {
		case "zcode-glm-5.3": // pairs carry the run's temp state_dir: pin names, then count
			for _, prefix := range want.env {
				if !slices.ContainsFunc(l.Env, func(kv string) bool { return strings.HasPrefix(kv, prefix) }) {
					t.Errorf("%s env missing %q*: %q", name, prefix, l.Env)
				}
			}
			if len(l.Env) != len(want.env) {
				t.Errorf("%s env = %q, want exactly the %d native pairs", name, l.Env, len(want.env))
			}
		default:
			if !slices.Equal(l.Env, want.env) {
				t.Errorf("%s env = %q, want exactly %q", name, l.Env, want.env)
			}
		}
		if len(want.cred) != len(l.CredentialEnvAliases) {
			t.Errorf("%s credential aliases = %v, want exactly %v", name, l.CredentialEnvAliases, want.cred)
			continue
		}
		for target, sources := range want.cred {
			if got := l.CredentialEnvAliases[target]; !slices.Equal(got, sources) {
				t.Errorf("%s %s sources = %q, want %q", name, target, got, sources)
			}
		}
	}
}

// Model.Env is model-scoped: a model that declares it appends its pairs only
// when IT is the bound one, and a tool with no launch env still receives them
// (the mechanism must not depend on the tool declaring env first).
func TestModelEnvAppliesOnlyWhenModelBound(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveTool(fleet.Tool{
		Name: "plainish", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Binary: "plainish", Launch: fleet.ToolLaunch{
			Exec: "plainish -m {model} {prompt}",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{
		Name: "switchenv", Kind: "api", Provider: "openai-compat",
		UpstreamID: "switchenv-1", BaseURL: "https://api.example.test/v1", APIKeyRef: "gtkey",
		Env: []string{"ROUTE_ON=1", "ROUTE_REGION=us-east-1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{
		Name: "plainmodel", Kind: "api", Provider: "openai-compat",
		UpstreamID: "plainmodel-1", BaseURL: "https://api.example.test/v1", APIKeyRef: "gtkey",
	}); err != nil {
		t.Fatal(err)
	}

	l, err := ResolveWithCatalog("plainish:switchenv", Options{AllowUnsafe: true, DryRun: true}, testCatalog(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ROUTE_ON=1", "ROUTE_REGION=us-east-1"} {
		if !slices.Contains(l.Env, want) {
			t.Errorf("bound switchenv env missing %q: %q", want, l.Env)
		}
	}

	// The same tool bound to a model with no env — or launched bare — sees
	// none of it: the native path stays byte-identical.
	l, err = ResolveWithCatalog("plainish:plainmodel", Options{AllowUnsafe: true, DryRun: true}, testCatalog(root))
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(l.Env, func(kv string) bool { return strings.HasPrefix(kv, "ROUTE_") }) {
		t.Errorf("plainmodel launch inherited route env: %q", l.Env)
	}
	l, err = ResolveWithCatalog("plainish", Options{AllowUnsafe: true, DryRun: true}, testCatalog(root))
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(l.Env, func(kv string) bool { return strings.HasPrefix(kv, "ROUTE_") }) {
		t.Errorf("unbound launch inherited route env: %q", l.Env)
	}
}
