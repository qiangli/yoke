package agentctl

import (
	"regexp"
	"strings"
)

// A declared binding that does not run looks EXACTLY like one that does.
//
// `agents verify` is structural: it confirms an agent's tool and model both
// resolve in the catalog. It never asks the tool whether it will accept the
// model — and so `agy:gemini3.1` sat in the fleet for months, listed, banded,
// routable, and completely dead: agy rejects `--model gemini-3.1` outright. The
// ledger recorded that as "needs interactive sign-in first" and "medium
// reliability", which is what a broken flag looks like from far enough away.
//
// The only thing that catches this is launching the agent. Not the tool — the
// AGENT, tool and model together, because the model is the half that was wrong.
// A probe that runs `agy --help` proves nothing about `agy --model X`.
//
// So: ask it a question a working agent cannot fail, and read what comes back.

// ProbePrompt is the question. It is deliberately trivial — a probe measures
// whether the agent RUNS, not whether it is clever, and anything a model could
// plausibly get wrong would turn a launch check into a capability test.
const ProbePrompt = "Reply with exactly PROBE_OK and nothing else."

// ProbeStatus is why an agent can or cannot speak.
type ProbeStatus string

const (
	// ProbeOK — launched headless and answered.
	ProbeOK ProbeStatus = "ok"

	// ProbeBadModel — the tool rejected the MODEL. The binding is dead: the
	// agent is listed and routable and will fail every time it is asked to
	// speak. This is the one that hid.
	ProbeBadModel ProbeStatus = "bad-model"

	// ProbeStaleContract — the tool rejected a FLAG. The launch contract in the
	// registry no longer matches the installed CLI (codex renamed --workspace to
	// --sandbox and started exiting 2).
	ProbeStaleContract ProbeStatus = "stale-contract"

	// ProbeNeedsAuth — the tool is waiting for a human: a login, a trust prompt,
	// a device code. Not broken, but not usable unattended either.
	ProbeNeedsAuth ProbeStatus = "needs-auth"

	// ProbeFailed — it ran, and something else went wrong.
	ProbeFailed ProbeStatus = "failed"
)

// OK reports whether an agent in this state can be asked to do work.
func (s ProbeStatus) OK() bool { return s == ProbeOK }

// modelErrSignatures mean the tool rejected the model id. Kept separate from a
// flag error because the FIX is different — a stale contract needs the argv
// template edited, a dead model needs the registry's upstream id re-pegged — and
// an operator told only "it failed" has to go and find that out.
var modelErrSignatures = []string{
	"invalid --model", "unknown model", "model not found", "unsupported model",
	"is not recognized as a known model", "no such model", "model not available",
	"supported api model names", "but you passed", // deepseek via litellm
}

// contractErrSignatures mean the tool rejected a flag: the recorded launch
// contract has drifted from the installed CLI.
var contractErrSignatures = []string{
	"unexpected argument", "unknown option", "unknown flag", "invalid option",
	"unrecognized option", "unrecognized argument", "Usage:", "USAGE:",
}

// Classify turns a probe's output into a verdict.
//
// Pure, and separated from the launch so it can be tested without spawning a real
// agent — the classification is the part with judgement in it, and the part that
// is wrong when a fleet reports "medium reliability" for something that never ran.
//
// Order matters. A model rejection is checked BEFORE a flag rejection because
// tools print a usage block when they reject a model, and reporting that as a
// stale contract would send an operator to edit an argv template that is fine.
func Classify(raw string, timedOut bool) (ProbeStatus, string) {
	low := strings.ToLower(raw)

	for _, sig := range modelErrSignatures {
		if strings.Contains(low, sig) {
			return ProbeBadModel, "the tool rejected the model id — re-peg this model's `model:` in the registry"
		}
	}
	for _, sig := range contractErrSignatures {
		if strings.Contains(raw, sig) {
			return ProbeStaleContract, "the tool rejected a flag (" + strings.TrimSpace(sig) + ") — its launch contract has drifted"
		}
	}
	for _, sig := range authGateSignatures {
		if strings.Contains(low, sig) {
			return ProbeNeedsAuth, "waiting for a human: " + sig
		}
	}
	// THE ONLY WAY TO PASS. The agent said the word, so the whole chain — argv,
	// flags, model id, credentials, the model itself — worked.
	if strings.Contains(raw, "PROBE_OK") {
		return ProbeOK, "launched headless and answered"
	}

	// Everything below here FAILS, and that default is the point.
	//
	// The first version of this function passed anything that produced output and
	// matched no known failure signature. It duly reported `aider:deepseek-v4` as
	// ok — an agent whose every run dies with "The supported API model names are
	// deepseek-v4-pro or deepseek-v4-flash, but you passed deepseek-v4", a message
	// no signature list happened to contain. A verifier that passes on the ABSENCE
	// of a known failure is not a verifier; it is a list of the failures somebody
	// already thought of, and it is exactly the bug it exists to catch.
	//
	// So the signature lists above are for DIAGNOSIS — telling an operator whether
	// to re-peg a model, fix an argv, or go and log in. They are not what earns a
	// pass. Only the agent saying PROBE_OK earns a pass.
	if timedOut && len(strings.TrimSpace(raw)) < 40 {
		return ProbeNeedsAuth, "stalled with no output — most likely an interactive gate"
	}
	if timedOut {
		return ProbeFailed, "timed out without answering"
	}
	if strings.TrimSpace(raw) == "" {
		return ProbeFailed, "produced no output"
	}
	return ProbeFailed, "ran, but never said PROBE_OK — read the output: " + firstLine(raw)
}

// firstLine picks the most useful line to show an operator.
//
// NOT simply the first: agent CLIs open with a banner and a box-drawing rule, so
// "the first line" is reliably `────────────────` — which tells nobody anything.
// Prefer a line that looks like an error; fall back to the last thing said, since
// that is where a tool usually dies.
func firstLine(raw string) string {
	clean := func(s string) string {
		s = ansiRE.ReplaceAllString(s, "")
		return strings.TrimSpace(strings.Trim(s, "─—-═│ "))
	}
	var last string
	for _, ln := range strings.Split(raw, "\n") {
		ln = clean(ln)
		if ln == "" {
			continue
		}
		low := strings.ToLower(ln)
		if strings.Contains(low, "error") || strings.Contains(low, "exception") ||
			strings.Contains(low, "failed") || strings.Contains(low, "invalid") {
			return truncate(ln)
		}
		last = ln
	}
	if last == "" {
		return "(no output)"
	}
	return truncate(last)
}

func truncate(s string) string {
	if len(s) > 140 {
		return s[:140] + "…"
	}
	return s
}

// ansiRE strips terminal escapes so a verdict is readable in a log or a ticket.
var ansiRE = regexp.MustCompile("\x1b\\[[0-9;?<>=]*[ -/]*[@-~]|\x1b[()][0-9A-Za-z]|\x1b[0-9A-Za-z><=]")

// authGateSignatures are the phrases a tool emits when it has stopped to ask for
// a human instead of doing work. Shared with the live gate classifier in
// pkg/agentpty, which watches for the same thing on a running PTY.
var authGateSignatures = []string{
	"not signed in", "sign in", "sign-in", "please log in", "please login",
	"not logged in", "log in to", "login required", "authentication required",
	"unauthorized", "run `login`", "run 'login'",
	"do you trust", "trust the contents", "trust this",
	"you must log in", "session expired", "no api key", "api key not",
	// A rejected CREDENTIAL, not a missing session. The distinction matters to
	// whoever reads the verdict: "needs-auth" sends them to `bashy secret`, and
	// a bare "failed" sends them hunting through a registry that is perfectly
	// fine. This is what the two kimi agents were reporting as "failed".
	"authenticationerror", "invalid api key", "incorrect api key",
	"invalid_api_key", "401", "api key is invalid",
}
