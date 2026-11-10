package secrets

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/creack/pty/v2"
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
	for _, handle := range []string{"secret exec NAME -- CMD", "secret exec --stdin NAME -- CMD"} {
		if !strings.Contains(err.Error(), handle) {
			t.Fatalf("agent refusal %q does not name the handle form %q", err.Error(), handle)
		}
	}
	err = CheckReveal(human, false)
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("non-tty human reveal: err = %v, want interactive-terminal refusal", err)
	}
	for _, handle := range []string{"secret exec NAME -- CMD", "secret exec --stdin NAME -- CMD"} {
		if !strings.Contains(err.Error(), handle) {
			t.Fatalf("non-tty refusal %q does not name the handle form %q", err.Error(), handle)
		}
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

// TestSecretEnvEvalChild is the child half of the command-substitution proof
// below: it runs `secret env` with its own stdin (a pty wired by the parent)
// as the terminal probe, stdout captured by the parent.
func TestSecretEnvEvalChild(t *testing.T) {
	if os.Getenv("BASHY_SECRET_ENV_EVAL_CHILD") == "" {
		return
	}
	configureSecretsTestEnv(t)
	fv := newFakeVault(t)
	fv.data["host-a-eval"] = "synthetic-eval-value"
	writeDefaultTemplate(t, "EVAL_KEY=@host-a-eval\n")
	out, _, err := executeSecrets(t, fv.cfg(), nil, "env", "--no-cache")
	if err != nil {
		fmt.Fprintln(os.Stderr, "child secret env:", err)
		os.Exit(9)
	}
	fmt.Print(out)
	os.Exit(0)
}

// TestEnvEvalSurvivesCommandSubstitution proves `eval "$(bashy secret env)"`
// keeps working for a human at an interactive shell. Command substitution
// captures STDOUT on a pipe — what stays interactive is STDIN, the user's
// terminal — and the gate probes stdin, not stdout, so the exports print.
// The child is exactly that shape: pty on stdin, pipe on stdout. A gate
// keyed on stdout-is-a-tty would refuse it (red); this pins that it does not.
func TestEnvEvalSurvivesCommandSubstitution(t *testing.T) {
	master, tty, err := pty.Open()
	if err != nil {
		t.Skipf("pty.Open: %v", err)
	}
	defer master.Close()
	defer tty.Close()

	child := exec.Command(os.Args[0], "-test.run=^TestSecretEnvEvalChild$")
	child.Stdin = tty // the user's terminal: interactive from the child's view
	var out, errb bytes.Buffer
	child.Stdout = &out // the command-substitution pipe
	child.Stderr = &errb
	// The child must look like the owner's shell, not an agent session,
	// regardless of the markers this test harness itself carries.
	env := []string{"BASHY_SECRET_ENV_EVAL_CHILD=1"}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "BASHY_SECRET_ENV_EVAL_CHILD", "BASHY_PRINCIPAL", "BASHY_AGENT_ID", "BASHY_AGENT_BINDING":
			continue
		}
		env = append(env, entry)
	}
	child.Env = env
	if err := child.Run(); err != nil {
		t.Fatalf("eval child: %v (stderr: %q)", err, errb.String())
	}
	const want = "export EVAL_KEY='synthetic-eval-value'\n"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("eval-shape output = %q, want %q in it (values must print while stdin is the terminal)", out.String(), want)
	}
}
