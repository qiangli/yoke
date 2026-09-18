package agentctl

import (
	"strings"
	"testing"
)

// THE REGRESSION. This is agy's real output, verbatim. The binding
// `agy:gemini3.1` sat in the fleet listed, banded and routable, and failed every
// single time it was asked to speak — while the ledger recorded it as "medium
// reliability, needs interactive sign-in first". It was never signing in. It was
// never launching.
func TestClassifyCatchesTheDeadModelBinding(t *testing.T) {
	raw := `Error: invalid --model "gemini-3.1": model gemini-3.1 is not recognized ` +
		`as a known model or custom model in settings
Available models:
  Gemini 3.5 Flash (Medium)
  Gemini 3.1 Pro (High)`

	st, note := Classify(raw, false)
	if st != ProbeBadModel {
		t.Fatalf("Classify = %q (%s), want %q — a dead model binding must be named as one", st, note, ProbeBadModel)
	}
	if st.OK() {
		t.Error("a dead binding must not report OK")
	}
}

// A model rejection is checked BEFORE a flag rejection, because a tool that
// rejects a model usually prints a usage block too. Reporting that as a stale
// contract sends the operator to edit an argv template that is perfectly fine.
func TestModelErrorWinsOverTheUsageBlockItPrints(t *testing.T) {
	raw := "Error: invalid --model \"x\"\nUsage: agy [options]"
	if st, _ := Classify(raw, false); st != ProbeBadModel {
		t.Errorf("Classify = %q, want %q — the usage block is a symptom, not the cause", st, ProbeBadModel)
	}
}

// THE SECOND ONE, found by the probe itself the first time it ran — and reported
// OK, because the first version of Classify passed anything that produced output
// and matched no known failure signature.
//
// A verifier that passes on the ABSENCE of a known failure is not a verifier; it
// is a list of the failures somebody already thought of. Only PROBE_OK passes.
func TestClassifyRefusesToPassWithoutPositiveEvidence(t *testing.T) {
	// aider:deepseek-v4, verbatim. Every run of this agent dies here.
	raw := `litellm.BadRequestError: DeepseekException - {"error":{"message":"The supported ` +
		`API model names are deepseek-v4-pro or deepseek-v4-flash, but you passed deepseek-v4."}}`
	if st, note := Classify(raw, false); st.OK() {
		t.Fatalf("Classify = %q (%s) — a dead binding must never pass just because its error was unfamiliar", st, note)
	}
	if st, _ := Classify(raw, false); st != ProbeBadModel {
		t.Errorf("Classify = %q, want %q so the operator knows to re-peg the model", st, ProbeBadModel)
	}

	// And the general case: output nobody has a signature for still FAILS.
	st, note := Classify("some novel error nobody has ever seen", false)
	if st.OK() {
		t.Errorf("unrecognised output must fail, not pass: %q (%s)", st, note)
	}
}

func TestClassifyVerdicts(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		timedOut bool
		want     ProbeStatus
	}{
		{"answered", "PROBE_OK", false, ProbeOK},
		{"stale flag", "error: unexpected argument '--workspace'", false, ProbeStaleContract},
		{"needs login", "You are currently not signed in.", false, ProbeNeedsAuth},
		{"trust prompt", "Do you trust the contents of this folder?", false, ProbeNeedsAuth},
		// Parked at a prompt: ran to the deadline having said nothing. This is the
		// shape of a tool waiting for a human who is not there.
		{"stalled silent", "", true, ProbeNeedsAuth},
		{"timed out talking", "thinking…" + string(make([]byte, 60)), true, ProbeFailed},
		{"said nothing", "", false, ProbeFailed},
		// Said SOMETHING, but not the word. Fails: a pass must be earned by
		// evidence, not by the absence of a signature we happened to list.
		{"answered without the token", "Sure! The answer is OK.", false, ProbeFailed},
		{"answered with the token", "here you go: PROBE_OK", false, ProbeOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, note := Classify(c.raw, c.timedOut); got != c.want {
				t.Errorf("Classify = %q (%s), want %q", got, note, c.want)
			}
		})
	}
}

// A HEADLESS turn gets a pty only if it has a prompt to clear mid-run. Being
// steerable is NOT a reason: a one-shot (`codex exec`, `agy -p`) runs the prompt
// and exits, so there is no session to interrupt — and the pty would only merge
// the tool's banner into the captured answer.
func TestNeedsTerminal(t *testing.T) {
	if (Profile{Steerable: true, SteerExec: "codex"}).NeedsTerminal() {
		t.Error("a steerable tool does NOT need a terminal for a headless one-shot — " +
			"its steering lives in a different launch, and a pty here only adds banner noise")
	}
	if !(Profile{Clear: "say:1"}).NeedsTerminal() {
		t.Error("a tool with a trust prompt that can appear mid-run needs a terminal")
	}
	if (Profile{}).NeedsTerminal() {
		t.Error("a tool that neither prompts nor listens must get a pipe")
	}
}

// CanSteer needs BOTH: the measured capability, and a launch that delivers it.
// supports_say alone is a claim about the tool; without SteerExec there is no
// argv that opens a session to steer.
func TestCanSteer(t *testing.T) {
	if !(Profile{Steerable: true, SteerExec: "codex --model x"}).CanSteer() {
		t.Error("measured-steerable + an interactive launch = steerable")
	}
	if (Profile{Steerable: true}).CanSteer() {
		t.Error("steerable with no interactive launch is a capability with no way to reach it")
	}
	if (Profile{SteerExec: "x"}).CanSteer() {
		t.Error("an interactive launch on a tool that does not listen is not steering")
	}
}

// A rejected credential is not a mystery failure. Both kimi agents were reporting
// "failed" when what they actually needed was a Moonshot API key — and "failed"
// sends an operator hunting through a registry that is perfectly fine.
func TestClassifyNamesARejectedCredential(t *testing.T) {
	raw := "litellm.AuthenticationError: AuthenticationError: MoonshotException - The provided API key is invalid"
	st, note := Classify(raw, false)
	if st != ProbeNeedsAuth {
		t.Fatalf("Classify = %q (%s), want %q so the operator is sent to `bashy secret`", st, note, ProbeNeedsAuth)
	}
	if st.OK() {
		t.Error("an agent that cannot authenticate is not usable")
	}
}

// The excerpt must not be the banner. Agent CLIs open with a box-drawing rule, so
// "the first line" is reliably `────────` — which tells nobody anything.
func TestFirstLinePrefersTheErrorOverTheBanner(t *testing.T) {
	raw := "────────────────────\nAider v0.86.2\nMain model: x\n\nlitellm.BadRequestError: boom\n"
	if got := firstLine(raw); !strings.Contains(got, "BadRequestError") {
		t.Errorf("firstLine = %q, want the error, not the banner", got)
	}
}
