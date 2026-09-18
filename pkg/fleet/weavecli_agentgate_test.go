// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// These tests live beside the marker table because IsAgentDriven only sees
// the table when this package is linked (weavecli.DetectTool is set at init
// above); in the bare coreutils build the predicate is BASHY_AGENTIC alone.

package fleet

import (
	"testing"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// humanShell scrubs every agent signal, so a test can assert what a HUMAN sees.
//
// The marker list is QUERIED from the registry, never hardcoded — it is data (`bashy
// tools add` extends it), so a literal list here would rot into a test that passes only
// because it forgot to clear the marker that mattered. That is not hypothetical: these
// tests first failed precisely because the suite itself runs inside an agent session.
func humanShell(t *testing.T) {
	t.Helper()
	t.Setenv("BASHY_AGENTIC", "")
	for _, env := range MarkerEnvs() {
		t.Setenv(env, "")
	}
}

// THE BUG: two shipped features were silently dark in the most common agentic setup.
//
// BASHY_AGENTIC is set in exactly ONE place — when bashy launches an agentic worker
// itself. A human who types `claude` in a terminal with bashy as its shell (which is the
// entire point of `bashy install-agent`) sets nothing. So the advisor and the nudges,
// which gated on weavecli.IsAgent(), never fired for the users they were built for.
//
// The tell was that the same wrong gate would have neutered the coordination guard: a
// guard that no-ops in exactly the sessions that collide.
func TestAnAgentDrivingTheShellIsDetected(t *testing.T) {
	humanShell(t)
	t.Setenv("CLAUDECODE", "1") // a human-launched Claude session: no BASHY_AGENTIC

	if !weavecli.IsAgentDriven() {
		t.Fatal(`weavecli.IsAgentDriven() = false in a plain agent session (CLAUDECODE set, BASHY_AGENTIC unset).

This is THE bug: the advisor and the nudges gate on this, so both were dark in the single
most common agentic configuration there is -- an agent CLI running with bashy as its
shell.`)
	}
}

// bashy ORCHESTRATING a run counts too — a weave worker has no harness marker of its own.
func TestAnOrchestratedWorkerIsAgentDriven(t *testing.T) {
	humanShell(t)
	t.Setenv("BASHY_AGENTIC", "1")
	if !weavecli.IsAgentDriven() {
		t.Fatal("a bashy-orchestrated worker is not agent-driven")
	}
	if !weavecli.IsAgent() {
		t.Fatal("weavecli.IsAgent() = false under BASHY_AGENTIC=1")
	}
}

// THE HALF THAT MUST NOT BREAK — and the reason this is two predicates and not one wider
// one. The three questions have different BLAST RADII:
//
//	a HINT   (advisor line on stderr when a command fails)  additive — fire it freely
//	a FORMAT (stdout as JSON instead of a human table)      a contract — do not sniff it
//	a WORLD  (`go` shimmed to a self-downloaded toolchain)  destructive — never guess
//
// Widening weavecli.IsAgent() to mean "an agent is driving" would have flipped ~30 subverbs'
// stdout from tables to JSON in every Claude session, AND shimmed `go` to a toolchain
// bashy downloads for itself — shadowing the compiler the developer actually installed.
// That trades one silent bug for two louder ones.
func TestAgentDrivenDoesNotImplyAgentMode(t *testing.T) {
	humanShell(t)
	t.Setenv("CLAUDECODE", "1")

	if !weavecli.IsAgentDriven() {
		t.Fatal("precondition: the agent should be detected")
	}
	if weavecli.IsAgent() {
		t.Fatal(`weavecli.IsAgent() = true in a human's Claude session.

weavecli.IsAgent decides OUTPUT FORMAT (~30 subverbs) and the TOOLCHAIN SHIMS. If it is true here:

  - every ` + "`weave list`" + ` and ` + "`weave status`" + ` in the transcript becomes a JSON blob
    instead of a table, changing what a human reads; and
  - bashy shadows the developer's own ` + "`go`" + ` with a DIFFERENT, self-downloaded
    toolchain, in a session they never asked to have taken over.

Agent MODE is asked for. It is not sniffed.`)
	}
	// And the format contract is intact: no explicit request, no forced JSON.
	if got := weavecli.ResolveOutputMode(false, false, false); got != weavecli.OutputAuto {
		t.Fatalf("weavecli.ResolveOutputMode = %v in a driven-but-not-agent-mode session, want weavecli.OutputAuto — the output contract must not flip on ambient detection", got)
	}
}

// No agent anywhere: a human at a terminal gets a plain shell, unchanged.
func TestAPlainHumanShellIsNeither(t *testing.T) {
	humanShell(t)
	if weavecli.IsAgentDriven() {
		t.Fatal("a plain human shell was treated as agent-driven — advisory hints would appear for no reason")
	}
	if weavecli.IsAgent() {
		t.Fatal("a plain human shell reported agent mode")
	}
}

// BASHY_AGENTIC=0 is an explicit opt-out of agent MODE, and it must work.
func TestExplicitOptOutIsHonoured(t *testing.T) {
	humanShell(t)
	t.Setenv("BASHY_AGENTIC", "0")
	if weavecli.IsAgent() {
		t.Fatal("BASHY_AGENTIC=0 still reported agent mode — the opt-out does not work")
	}
}
