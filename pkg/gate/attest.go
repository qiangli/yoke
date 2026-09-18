package gate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// WHY THIS LIVES HERE AND NOT IN pkg/herald.
//
// A self-reported completion is a claim, and running a command to settle it is
// the same primitive whatever protocol carried the claim: A2A's
// TASK_STATE_COMPLETED and ACP's end_turn are both the agent's own word that it
// finished, with no way for the driver to disagree.
//
// It was written in pkg/herald because A2A needed it first, and pkg/acp then
// imported herald to reach it — which pinned the arrow herald -> acp shut. ACP
// is the protocol a HOST speaks to drive us, so `herald acp` has to construct
// an acp.Agent, and herald could not import acp while acp imported herald.
//
// The cycle was never structural. The gate simply belonged one level down, in
// the package that already owns "run a command, let its exit status be the
// verdict". Both protocol packages now import gate, and neither imports the
// other.
//
// Distinct from Run/Definition in this same package: those resolve and execute
// the PROJECT's declared gate. This runs one arbitrary command supplied for a
// single delegated task, and reports a verdict shaped for that.

// Outcome is the verdict on one delegated task.
type Outcome struct {
	// Ran reports whether a gate was executed at all. False means the task
	// was delegated with no gate — permitted, but the result is unverified.
	Ran bool `json:"ran"`
	// Passed is the verdict. Meaningless unless Ran.
	Passed bool `json:"passed"`
	// Abstained means an OPTED-IN mutation probe ran and did not prove the
	// gate can fail. The pass is therefore unbelievable and is withheld: a
	// caller who switched probing on asked for exactly that strictness.
	//
	// Silence does NOT abstain. See Silent.
	Abstained bool `json:"abstained"`
	// Silent reports that the gate passed while producing no output.
	//
	// It is an OBSERVATION, NOT A VERDICT — Passed stays true. Silence on
	// success is the Unix convention, not a defect: `go build ./...`,
	// `go vet`, and `test -f x` all say nothing when they work, and this
	// project's own gate is `go vet ... && go test ...`. Flipping Passed here
	// would abstain on a clean vet and break every quiet gate in the tree.
	//
	// It is still worth recording. A gate that is ALWAYS silent is a
	// candidate for vacuity, and a caller comparing runs can see that. The
	// judgement belongs to the caller; the flag just refuses to hide it.
	Silent bool `json:"silent,omitempty"`
	// Where records who ran it: "peer" (the extension) or "local" (fallback).
	Where string `json:"where"`
	// Command is the gate as given.
	Command string `json:"command,omitempty"`
	// ExitCode is the gate's exit status when run locally.
	ExitCode int `json:"exit_code"`
	// Output is captured combined output, truncated for transport.
	Output string `json:"output,omitempty"`
	// OutputBytes records evidence without requiring consumers to retain it.
	OutputBytes int `json:"output_bytes"`
	// Probe records whether this run demonstrated that the command can fail.
	Probe ProbeResult `json:"probe"`
	// PeerClaimed is what the counterparty said before the gate ran. Recorded
	// because the gap between claim and verdict is the interesting signal — a
	// peer that habitually claims COMPLETED on a failing gate is a peer whose
	// reliability ledger should say so.
	PeerClaimed string `json:"peer_claimed,omitempty"`
	// Elapsed is how long the gate took.
	Elapsed time.Duration `json:"elapsed_ns,omitempty"`
}

// Trusted reports whether the outcome may be treated as success.
//
// The whole point: an unrun gate is NOT success. Callers must branch on this,
// never on the counterparty's reported state.
func (o Outcome) Trusted() bool { return o.Ran && o.Passed }

// Summary is a one-line human rendering.
func (o Outcome) Summary() string {
	switch {
	case !o.Ran:
		return "UNVERIFIED (no gate ran)"
	case o.Abstained:
		return "ABSTAINED (probe did not prove this gate can fail)"
	case o.Passed && o.Silent:
		return fmt.Sprintf("PASS (%s gate, no output)", o.Where)
	case o.Passed:
		return fmt.Sprintf("PASS (%s gate)", o.Where)
	default:
		return fmt.Sprintf("FAIL (%s gate, exit %d)", o.Where, o.ExitCode)
	}
}

// maxAttestOutput caps captured output so a runaway gate cannot balloon a task
// record. The tail is kept: failures print last.
const maxAttestOutput = 16 << 10

// RunLocal executes the gate command in dir and returns the verdict.
//
// Shelling out here is correct and not a violation of the no-shell-out rule:
// that rule forbids a tool from spawning programs to implement ITS OWN
// behavior. A gate's entire documented purpose IS to run the operand command,
// exactly like timeout(1) or xargs(1) — the same carve-out those tools use.
func RunLocal(ctx context.Context, dir, command, peerClaimed string) Outcome {
	return RunLocalWithProbe(ctx, dir, command, peerClaimed, nil)
}

// RunLocalWithProbe executes a local gate with an optional reversible
// perturbation. The same command must fail under the mutation and pass after
// restoration before its green result is trusted.
func RunLocalWithProbe(ctx context.Context, dir, command, peerClaimed string, probe *MutationProbe) Outcome {
	out := Outcome{Where: "local", Command: command, PeerClaimed: peerClaimed}
	if strings.TrimSpace(command) == "" {
		return out // Ran stays false: no gate and no verified verdict.
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}

	if probe == nil {
		out = runLocal(ctx, dir, command, peerClaimed)
		// Record silence; do not punish it. See Outcome.Silent.
		out.Silent = out.Passed && out.OutputBytes == 0
		return out
	}

	runs := 0
	out.Probe = ProveGuardMutation(ctx, *probe, func(ctx context.Context) (bool, error) {
		runs++
		got := runLocal(ctx, dir, command, peerClaimed)
		if runs == 2 {
			out = got
		}
		return got.Passed, nil
	})
	probeResult := out.Probe
	if runs < 2 {
		out = Outcome{Ran: runs > 0, Where: "local", Command: command, PeerClaimed: peerClaimed}
	}
	out.Probe = probeResult
	// Only an unproved PROBE withholds the pass. Probing is opt-in, so a
	// caller who enabled it has asked not to believe a gate that was never
	// shown capable of failing. Silence alone is recorded, never punished.
	out.Silent = out.Passed && out.OutputBytes == 0
	if out.Ran && !out.Probe.Proved {
		out.Passed = false
		out.Abstained = true
	}
	return out
}

func runLocal(ctx context.Context, dir, command, peerClaimed string) Outcome {
	out := Outcome{Where: "local", Command: command, PeerClaimed: peerClaimed}
	start := time.Now()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	prepareGateCommand(cmd)
	cmd.Dir = dir
	// The gate must not inherit the operator's secrets: it is arbitrary
	// operator-supplied code being run to judge ANOTHER party's output.
	cmd.Env = attestEnv(dir)
	raw, err := cmd.CombinedOutput()
	out.Elapsed = time.Since(start)
	out.Ran = true
	out.Output = truncateTail(string(raw), maxAttestOutput)
	out.OutputBytes = len(raw)

	if err == nil {
		out.Passed = true
		return out
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		out.ExitCode = ee.ExitCode()
	} else {
		// The gate could not be executed at all (missing shell, bad dir).
		// That is NOT a pass, and it is not a counterparty failure either — it
		// is an unusable gate, which must be loud rather than silently
		// permissive.
		out.ExitCode = -1
		out.Output = strings.TrimSpace(out.Output + "\ngate: could not run: " + err.Error())
	}
	return out
}

// attestEnv builds a minimal environment for gate execution. PATH and HOME are
// preserved because a gate is usually a build or test command; the vault
// variables are not.
//
// HERALD_GATE_DIR is kept under its original name deliberately: it is an
// observable a gate script may already read, and renaming it to match this
// package would break those scripts for no gain.
func attestEnv(dir string) []string {
	return Env(dir)
}

// Env is attestEnv exported for the OTHER gate executors.
//
// Every executor that runs a gate — a command whose exit code becomes a VERDICT
// — must build its environment here rather than from os.Environ(). The list is
// an ALLOWLIST on purpose: a denylist is fail-open against variable names that
// do not exist yet, and the whole point is that a control invented next year
// cannot silently change what a gate decides. A gate that inherits an ambient
// control is a verdict whose meaning depends on where it ran.
//
// PATH and HOME are preserved because a gate is usually a build or test
// command; the Go cache variables for the same reason.
func Env(dir string) []string {
	keep := []string{"PATH", "HOME", "LANG", "TMPDIR", "GOCACHE", "GOMODCACHE", "GOPATH"}
	env := make([]string, 0, len(keep)+2)
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, "LC_ALL=C", "HERALD_GATE_DIR="+dir)
	return env
}

// truncateTail keeps the last n bytes, which is where a failure explains itself.
func truncateTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…(truncated)…\n" + s[len(s)-n:]
}

// controlPrefixes are the environment namespaces that carry BASHY'S OWN
// controls — the variables that change what a command DOES rather than what it
// reports. They are the ones a gate must never inherit.
var controlPrefixes = []string{"BASHY_", "AGENTIC"}

// ScrubControls removes bashy's control variables from an otherwise inherited
// environment.
//
// It exists for executors that CANNOT use Env: weave's verify runs an arbitrary
// operator-supplied build or test command, which legitimately needs the wider
// toolchain environment (GOFLAGS, CGO_ENABLED, SSH_AUTH_SOCK, proxy settings, a
// compiler's own variables) that no fixed allowlist can enumerate without
// breaking real commands.
//
// The guarantee is deliberately NARROWER than Env's and the difference matters:
// Env is fail-closed against every unknown name, while this is fail-closed only
// within the namespaces bashy controls. That is sound for the threat it
// addresses — an ambient bashy control reaching a gate — because those controls
// live in a namespace we own, so a new one is caught by prefix without being
// added here. It is NOT sound against a third-party variable that changes
// behavior, and callers should prefer Env whenever their gate's environment can
// actually be enumerated.
func ScrubControls(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if ok && hasControlPrefix(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func hasControlPrefix(name string) bool {
	for _, p := range controlPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
