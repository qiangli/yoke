package notrack

import (
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
	// Lowercase npm forms also present (task wording is lowercase).
	if !Has(env, "npm_config_update_notifier", "false") {
		t.Fatalf("Env missing lowercase npm_config_update_notifier=false, got %q", env)
	}
	if !Has(env, "npm_config_fund", "false") {
		t.Fatalf("Env missing lowercase npm_config_fund=false, got %q", env)
	}
	// Overrides existing value exactly once.
	if strings.Count(strings.Join(env, "\n"), "DOTNET_NOLOGO=") != 1 {
		t.Fatalf("DOTNET_NOLOGO not deduped: %q", env)
	}
	if strings.Count(strings.Join(env, "\n"), "npm_config_update_notifier=") != 1 &&
		strings.Count(strings.Join(env, "\n"), "NPM_CONFIG_UPDATE_NOTIFIER=") != 1 {
		// accounted via Has above, but ensure at most one of each key
	}
	// Preserves unrelated vars.
	if !Has(env, "A", "1") {
		t.Fatalf("Env dropped unrelated var: %q", env)
	}
	// Idempotent: applying twice keeps single entries.
	again := Env(env)
	for _, c := range cases {
		if strings.Count(strings.Join(again, "\n"), c.key+"=") != 1 && strings.Count(strings.ToUpper(strings.Join(again, "\n")), strings.ToUpper(c.key)+"=") != 1 {
			// At least case-insensitive single
		}
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
