package chat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// denyUnsafeLaunch pins the host to the SAFE posture: no explicit opt-in and no
// container. Without this a test host that happens to run inside a container (CI)
// would resolve the unsafe argv and mask the default we are asserting.
func denyUnsafeLaunch(t *testing.T) {
	t.Helper()
	t.Setenv(UnsafeLaunchEnv, "")
	prev := containerized
	containerized = func() bool { return false }
	t.Cleanup(func() { containerized = prev })
}

var unsafeFlagList = []string{
	"--dangerously-skip-permissions",
	"--dangerously-bypass-approvals-and-sandbox",
	"--yolo",
	"--yes-always",
	"--auto",
}

// The seeded fallback profiles (the last-resort contract for an unregistered
// tool) carry NO approval-gate kill-switch in their default Args — the
// kill-switches live in UnsafeArgs and are applied only when a launch is
// permitted. This is the safe-by-default guarantee at the data level.
func TestSeededProfilesAreSafeByDefault(t *testing.T) {
	for name, p := range seededProfiles {
		for _, a := range p.Args {
			for _, bad := range unsafeFlagList {
				if a == bad {
					t.Errorf("seeded profile %q default Args carry unsafe flag %q: %v", name, bad, p.Args)
				}
			}
		}
	}
}

// On an uncontained host with no opt-in, NO default launch is performed with an
// agent's safety system stripped: a registered agent whose template carries a
// kill-switch is REFUSED, and a seeded/unregistered agent renders a clean argv.
// Either way the resolved-and-permitted argv never contains a kill-switch.
func TestDefaultLaunchNeverCarriesUnsafeFlags(t *testing.T) {
	pinCatalog(t)
	denyUnsafeLaunch(t)

	for name := range seededProfiles {
		l, err := resolveLaunch(name, Options{})
		if err != nil {
			continue // refused before launch — the gate did its job
		}
		for _, a := range l.Args {
			for _, bad := range unsafeFlagList {
				if a == bad {
					t.Errorf("default launch of %q carries unsafe flag %q: %v", name, bad, l.Args)
				}
			}
		}
	}
}

// The same agent, once unsafe launches are permitted, gets its kill-switch back
// — proving the flag is gated, not deleted.
func TestPermittedLaunchRestoresUnsafeFlags(t *testing.T) {
	pinCatalog(t)
	permitUnsafeLaunch(t)

	_, args, _ := argv(t, "claude", Options{})
	found := false
	for _, a := range args {
		if a == "--dangerously-skip-permissions" {
			found = true
		}
	}
	if !found {
		t.Fatalf("permitted claude launch should carry its kill-switch: %v", args)
	}
}

// The credential firewall is wired into the agent-spawn environment: a seeded
// vault secret present in the launcher's own env is NOT handed to the child.
func TestAgentChildEnvScrubsVaultSecret(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("BASHY_ALLOW_AGENT_SECRETS", "")
	t.Setenv("BASHY_FORCE_AGENT_SHELL", "0") // avoid shim/symlink side effects
	mapPath := filepath.Join(cfg, "bashy", "secrets.map")
	if err := os.MkdirAll(filepath.Dir(mapPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mapPath, []byte("ANTHROPIC_API_KEY=@host-anthropic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-live-secret")

	env := agentChildEnv(context.Background())
	for _, kv := range env {
		if len(kv) >= len("ANTHROPIC_API_KEY=") && kv[:len("ANTHROPIC_API_KEY=")] == "ANTHROPIC_API_KEY=" {
			t.Fatalf("vault secret handed to spawned agent: %q", kv)
		}
	}
}

// PreserveEnv was added after Launch became public. Manual/legacy constructors
// with a nil or empty slice must retain the catalog-derived single-key behavior
// without reopening unrelated operator credentials.
func TestAgentChildEnvLegacyLaunchFallsBackToCatalogCredential(t *testing.T) {
	pinCatalog(t)
	t.Setenv("BASHY_ALLOW_AGENT_SECRETS", "0")
	t.Setenv("BASHY_FORCE_AGENT_SHELL", "0")
	t.Setenv("DEEPSEEK_API_KEY", "selected-model-credential")
	t.Setenv("OPENAI_API_KEY", "unrelated-operator-credential")

	for _, preserve := range [][]string{nil, {}} {
		ctx := withLaunch(context.Background(), Launch{
			Tool: "ycode", ToolName: "ycode", ModelName: "deepseek-v4-pro", PreserveEnv: preserve,
		})
		env := agentChildEnv(ctx)
		if !childEnvHasName(env, "DEEPSEEK_API_KEY") {
			t.Errorf("legacy launch lost catalog credential: names=%v", childEnvNames(env))
		}
		if childEnvHasName(env, "OPENAI_API_KEY") {
			t.Errorf("legacy fallback reopened unrelated credential: names=%v", childEnvNames(env))
		}
	}
}

// Coach uses Session/steer_exec while chat uses the one-shot exec template.
// Those are deliberately different argv attached differently, but they must
// never become different credential environments again.
func TestCoachAndChatChildProviderEnvironmentMatch(t *testing.T) {
	pinCatalog(t)
	t.Setenv("BASHY_ALLOW_AGENT_SECRETS", "0")
	t.Setenv("BASHY_FORCE_AGENT_SHELL", "0")
	t.Setenv("ZAI_API_KEY", "selected-model-credential")
	t.Setenv("OPENAI_API_KEY", "unrelated-operator-credential")
	t.Setenv("ANTHROPIC_API_KEY", "")

	chatLaunch, err := resolveLaunch("ycode-glm-5.2", Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	coachLaunch, err := resolveLaunch("ycode-glm-5.2", Options{ReadOnly: true, Steer: true})
	if err != nil {
		t.Fatal(err)
	}

	chatCmd := agentCommand(withLaunch(context.Background(), chatLaunch),
		chatLaunch.Tool, append(chatLaunch.Args, "task"), ".")
	coachCmd := agentCommand(withLaunch(context.Background(), coachLaunch),
		coachLaunch.Tool, coachLaunch.Args, ".")

	names := []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "ZAI_API_KEY"}
	chatProviders := selectedEnv(chatCmd.Env, names)
	coachProviders := selectedEnv(coachCmd.Env, names)
	if strings.Join(chatProviders, "\x00") != strings.Join(coachProviders, "\x00") {
		t.Fatalf("provider environment diverged:\nchat: %v\ncoach: %v", chatProviders, coachProviders)
	}
	if !contains(chatProviders, "ZAI_API_KEY=selected-model-credential") {
		t.Errorf("shared child environment missing selected provider credential: %v", chatProviders)
	}
	for _, unrelated := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		if childEnvHasName(chatProviders, unrelated) {
			t.Errorf("shared child environment leaked unrelated %s: %v", unrelated, chatProviders)
		}
	}
}

func selectedEnv(env, names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		prefix := name + "="
		for _, kv := range env {
			if strings.HasPrefix(kv, prefix) {
				out = append(out, kv)
				break
			}
		}
	}
	return out
}

func childEnvHasName(env []string, name string) bool {
	prefix := name + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

func childEnvNames(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out = append(out, kv[:i])
		}
	}
	return out
}
