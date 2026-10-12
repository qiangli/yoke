package notrack

import (
	"reflect"
	"strings"
	"testing"
)

func TestEnvContainsRequiredCategories(t *testing.T) {
	env := Env([]string{"A=1", "DOTNET_NOLOGO=0", "npm_config_update_notifier=true"})
	// At least the categories named in sprint acceptance.
	cases := []struct{ key, want string }{
		{"NPM_CONFIG_UPDATE_NOTIFIER", "false"},
		{"NPM_CONFIG_FUND", "false"},
		{"PIP_DISABLE_PIP_VERSION_CHECK", "1"},
		{"GOTELEMETRY", "off"},
		{"DOTNET_CLI_TELEMETRY_OPTOUT", "1"},
		{"DOTNET_NOLOGO", "1"},
		{"POWERSHELL_TELEMETRY_OPTOUT", "1"},
		{"POWERSHELL_UPDATECHECK", "Off"},
		{"DO_NOT_TRACK", "1"},
		{"GH_NO_UPDATE_NOTIFIER", "1"},
		{"CHECKPOINT_DISABLE", "1"},
	}
	for _, c := range cases {
		if !Has(env, c.key, c.want) {
			t.Fatalf("Env missing %s=%s, got %q", c.key, c.want, env)
		}
	}
	// The lowercase spelling is replaced, not duplicated.
	if n := strings.Count(strings.ToLower(strings.Join(env, "\n")), "npm_config_update_notifier="); n != 1 {
		t.Fatalf("npm_config_update_notifier appears %d times: %q", n, env)
	}
	// Preserves unrelated vars.
	if !Has(env, "A", "1") {
		t.Fatalf("Env dropped unrelated var: %q", env)
	}
	// Idempotent: applying twice yields the same env.
	if again := Env(env); !reflect.DeepEqual(again, env) {
		t.Fatalf("Env not idempotent:\n%q\n%q", env, again)
	}
}

func TestEnvCaseInsensitiveOverride(t *testing.T) {
	env := Env([]string{"gotelemetry=on", "Gh_No_Update_Notifier=0"})
	if !Has(env, "GOTELEMETRY", "off") {
		t.Fatalf("GOTELEMETRY not overridden case-insensitively: %q", env)
	}
	if !Has(env, "GH_NO_UPDATE_NOTIFIER", "1") {
		t.Fatalf("GH_NO_UPDATE_NOTIFIER not overridden: %q", env)
	}
}

func TestVarsHaveNoCaseCollisions(t *testing.T) {
	seen := map[string]string{}
	for k := range Vars {
		u := strings.ToUpper(k)
		if prev, ok := seen[u]; ok {
			t.Fatalf("Vars keys %q and %q collide case-insensitively", prev, k)
		}
		seen[u] = k
	}
}
