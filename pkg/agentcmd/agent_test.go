package agentcmd

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/bus"
)

func clearIdentityEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"BASHY_PRINCIPAL", "BASHY_AGENT_ID", "BASHY_AGENT", "WEAVE_AGENT",
		"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_SANDBOX", "CODEX_THREAD_ID",
		"GEMINI_CLI", "CURSOR_AGENT", "CURSOR_TRACE_ID", "GOOSE_TERMINAL",
		"OPENCODE_CLIENT", "CLINE_ACTIVE", "AGENT", "AI_AGENT",
	} {
		t.Setenv(key, "")
	}
}

func TestWhoAmIUsesTheBoardIdentity(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/meridian-alias")
	bus.FleetResolveName = func(name string) string {
		if name == "meridian-alias" {
			return "meridian"
		}
		return ""
	}
	t.Cleanup(func() { bus.FleetResolveName = nil })

	got, err := WhoAmI()
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != "meridian" || got.Source != "launcher principal" {
		t.Fatalf("WhoAmI() = %+v, want board identity meridian", got)
	}
}

func TestWhoAmIUsesLauncherAgentID(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("BASHY_AGENT_ID", "meridian")

	got, err := WhoAmI()
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != "meridian" {
		t.Fatalf("WhoAmI() = %+v, want launcher agent ID", got)
	}
}

func TestWhoAmIRefusesToolAndLoginFallbacks(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("USER", "qiangli")
	t.Setenv("CLAUDECODE", "1")
	if _, err := WhoAmI(); err == nil || !strings.Contains(err.Error(), "tool \"claude\"") {
		t.Fatalf("tool-only WhoAmI error = %v, want an identity refusal", err)
	}

	clearIdentityEnv(t)
	if _, err := WhoAmI(); err == nil || !strings.Contains(err.Error(), "no launcher-stamped") {
		t.Fatalf("unattributed WhoAmI error = %v, want an identity refusal", err)
	}
}
