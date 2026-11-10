package secrets

import (
	"io"
	"strings"
	"testing"
)

// allowRevealForTest simulates the interactive owner terminal for one test:
// it lifts the terminal probe (a `go test` process has no pty) and blanks the
// agent markers (the test harness itself may be running as an agent session,
// whose environment the test process inherits). Both are in-process seams —
// never an environment variable a spawned agent could forge — and t.Setenv
// restores the real values at cleanup.
func allowRevealForTest(t *testing.T) {
	t.Helper()
	for _, name := range []string{"BASHY_PRINCIPAL", "BASHY_AGENT_ID", "BASHY_AGENT_BINDING"} {
		t.Setenv(name, "")
	}
	prev := revealInteractive
	revealInteractive = func(io.Reader) bool { return true }
	t.Cleanup(func() { revealInteractive = prev })
}

func TestAgentCaller(t *testing.T) {
	for _, tc := range []struct {
		name     string
		environ  []string
		expected bool
	}{
		{"empty", nil, false},
		{"plain process", []string{"PATH=/bin", "HOME=/tmp"}, false},
		{"agent principal", []string{"BASHY_PRINCIPAL=dhnt:agent/esme"}, true},
		{"person principal is not an agent", []string{"BASHY_PRINCIPAL=dhnt:person/esme"}, false},
		{"agent id", []string{"BASHY_AGENT_ID=esme"}, true},
		{"blank agent id", []string{"BASHY_AGENT_ID="}, false},
		{"agent binding", []string{"BASHY_AGENT_BINDING=claude:fable"}, true},
		{"unrelated prefix", []string{"BASHY_PRINCIPALX=dhnt:agent/esme"}, false},
	} {
		if got := AgentCaller(tc.environ); got != tc.expected {
			t.Errorf("%s: AgentCaller = %v, want %v", tc.name, got, tc.expected)
		}
	}
}

func TestCheckReveal(t *testing.T) {
	human := []string{"PATH=/bin"}
	agent := []string{"PATH=/bin", "BASHY_PRINCIPAL=dhnt:agent/esme"}

	if err := CheckReveal(human, true); err != nil {
		t.Fatalf("interactive human reveal refused: %v", err)
	}
	err := CheckReveal(agent, true)
	if err == nil || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("agent reveal at a tty: err = %v, want agent refusal", err)
	}
	err = CheckReveal(human, false)
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("non-tty human reveal: err = %v, want interactive-terminal refusal", err)
	}
	if want := ErrRevealRefused.Error(); !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("refusal %q does not carry %q", err.Error(), want)
	}
}

// The gate runs BEFORE the vault is touched: a refused caller learns nothing,
// not even whether the name exists (no network request is made).
func TestShowRefusesAgentAndNonTTYBeforeVault(t *testing.T) {
	configureSecretsTestEnv(t)
	// Neutralize whatever session markers the surrounding harness left in
	// this process's environment so each refusal reason is tested on its own.
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("BASHY_AGENT_ID", "")
	t.Setenv("BASHY_AGENT_BINDING", "")
	fv := newFakeVault(t)
	fv.data["deploy-token"] = "sycamore-lantern-42"

	// Non-tty caller (the test process has no terminal on stdin).
	_, _, err := executeSecrets(t, fv.cfg(), nil, "show", "deploy-token")
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("non-tty show: err = %v, want interactive-terminal refusal", err)
	}
	if len(fv.paths) != 0 {
		t.Fatalf("refused show still reached the vault: %v", fv.paths)
	}

	// Agent caller: refused even with the interactive probe lifted.
	allowRevealForTest(t)
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/esme")
	_, _, err = executeSecrets(t, fv.cfg(), nil, "show", "deploy-token")
	if err == nil || !strings.Contains(err.Error(), "agent callers") {
		t.Fatalf("agent show: err = %v, want agent refusal", err)
	}
	if len(fv.paths) != 0 {
		t.Fatalf("refused agent show still reached the vault: %v", fv.paths)
	}
}

func TestEnvRefusesNonTTY(t *testing.T) {
	configureSecretsTestEnv(t)
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("BASHY_AGENT_ID", "")
	t.Setenv("BASHY_AGENT_BINDING", "")
	fv := newFakeVault(t)
	_, _, err := executeSecrets(t, fv.cfg(), nil, "env")
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("non-tty env: err = %v, want interactive-terminal refusal", err)
	}
	if len(fv.paths) != 0 {
		t.Fatalf("refused env still reached the vault: %v", fv.paths)
	}
}

// Handle use needs no reveal gate: secret exec runs non-interactively by
// design (TestSecretExecChildEnvironmentOnly exercises the whole verb without
// a tty), so the gate above must never grow onto it.
