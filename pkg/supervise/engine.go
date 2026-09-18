package supervise

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	gatepkg "github.com/qiangli/yoke/pkg/gate"

	"github.com/qiangli/yoke/pkg/chat"
)

// gateTimeout bounds one gate run so a hung verifier can't wedge the whole
// supervision. Overridable via BASHY_SUPERVISE_GATE_TIMEOUT.
func gateTimeout() time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(os.Getenv("BASHY_SUPERVISE_GATE_TIMEOUT"))); err == nil && d > 0 {
		return d
	}
	return 5 * time.Minute
}

// runGate runs a contract's gate command and returns pass, exit code, and a
// bounded tail of its output. The gate is the ORCHESTRATOR's own check, run
// after the worker's turn, so a "done" from an agent that did not actually make
// the tree pass is caught here. Run through a shell so `&&`, pipes, and globs
// work; bash preferred (fixtures need it), sh as the fallback.
func runGate(ctx context.Context, cwd, gate string) (pass bool, exit int, tail string) {
	sh := "bash"
	if _, err := exec.LookPath(sh); err != nil {
		sh = "sh"
	}
	gctx, cancel := context.WithTimeout(ctx, gateTimeout())
	defer cancel()
	cmd := exec.CommandContext(gctx, sh, "-c", gate)
	cmd.Dir = cwd
	// A gate's exit code is a VERDICT, so it must not inherit ambient controls:
	// a gate that behaves differently because of what was set in the shell that
	// launched it is a verdict whose meaning depends on where it ran. The
	// allowlist keeps what a build or test command needs and drops the rest.
	cmd.Env = gatepkg.Env(cwd)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	cmd.WaitDelay = 5 * time.Second // a gate that spawns children can't hold us open forever
	err := cmd.Run()
	out := buf.String()
	if len(out) > 4000 {
		out = "…" + out[len(out)-4000:]
	}
	if gctx.Err() == context.DeadlineExceeded {
		return false, 124, strings.TrimSpace(out) + "\n(gate timed out)"
	}
	if err == nil {
		return true, 0, strings.TrimSpace(out)
	}
	code := 1
	var ee *exec.ExitError
	if ok := asExit(err, &ee); ok {
		code = ee.ExitCode()
	}
	return false, code, strings.TrimSpace(out)
}

func asExit(err error, target **exec.ExitError) bool {
	for err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			*target = ee
			return true
		}
		type unwrap interface{ Unwrap() error }
		u, ok := err.(unwrap)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// workerPrompt is the contract handed to a spoke: the overall goal, this task's
// goal, and — on a retry — the exact gate that is still failing and its output,
// so the worker fixes the real thing rather than guessing.
func workerPrompt(p *Plan, c *Contract, attempt int, priorGate string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are a worker on a supervised goal. Do the task, then STOP.\n\n")
	fmt.Fprintf(&b, "Overall goal: %s\n\n", p.Goal)
	fmt.Fprintf(&b, "Your task: %s\n", c.Goal)
	if c.gated() {
		fmt.Fprintf(&b, "\nYour work is verified by this gate, which the orchestrator runs itself — make it exit 0:\n  %s\n", c.Gate)
	}
	if attempt > 0 && priorGate != "" {
		fmt.Fprintf(&b, "\nATTEMPT %d. The gate is STILL FAILING. Its last output:\n%s\n\nFix the real cause; do not fake the gate.", attempt+1, indent(priorGate))
	}
	return b.String()
}

func indent(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		b.WriteString("    " + line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// runContract drives one contract to a verdict: dispatch to a worker, run the
// optional gate, and retry on a different fleet member up to maxAttempts. A
// supplied gate is authoritative; otherwise the worker exit is the verdict.
func runContract(ctx context.Context, p *Plan, c *Contract, runner chat.Runner, out chatWriter) Verdict {
	v := Verdict{Contract: c.ID, At: nowFn(), Unverified: !c.gated()}
	priorGate := ""
	for attempt := 0; attempt < p.maxAttempts(); attempt++ {
		worker := p.pick(c, attempt)
		v.Worker = worker
		v.Attempts = attempt + 1
		out.progress(fmt.Sprintf("• %s → %s (attempt %d/%d)", c.ID, worker, attempt+1, p.maxAttempts()))
		p.appendEvent(Event{Kind: "dispatch", Contract: c.ID, Worker: worker, Attempt: attempt})

		res, err := chat.Invoke(ctx, chat.Options{
			Agent:       worker,
			Role:        "worker",
			Instruction: workerPrompt(p, c, attempt, priorGate),
			Files:       p.Brief,
			Cwd:         p.Cwd,
			Sandbox:     p.Sandbox,
			Timeout:     p.turnTimeout(),
			Stream:      liveStream(out),
		}, runner)
		turnText := res.Output
		if err != nil && strings.TrimSpace(turnText) == "" {
			turnText = err.Error()
		}
		file := p.writeTurnFile(c, attempt, turnText)
		p.appendEvent(Event{Kind: "turn", Contract: c.ID, Worker: worker, Attempt: attempt,
			Text: oneLine(turnText), File: file})
		// A gate describes the desired tree, not proof that this worker ran. If
		// launch failed, evaluating an already-green gate would manufacture a
		// successful turn from no work at all.
		if err != nil || res.ExitCode != 0 {
			v.GateExit = res.ExitCode
			if v.GateExit == 0 {
				v.GateExit = 1
			}
			v.Detail = strings.TrimSpace(turnText)
			out.progress(fmt.Sprintf("  ✗ %s worker failed before gate (exit %d)", c.ID, v.GateExit))
			priorGate = v.Detail
			continue
		}

		// Without a gate the worker exit decides, with Unverified preserving the
		// distinction from independent verification.
		if !c.gated() {
			v.Passed = err == nil && res.ExitCode == 0
			v.GateExit = res.ExitCode
			p.appendEvent(Event{Kind: "verdict", Contract: c.ID, Worker: worker, Attempt: attempt,
				Text: fmt.Sprintf("UNVERIFIED (no gate); worker exit %d", res.ExitCode)})
			return v
		}
		pass, code, tail := runGate(ctx, p.Cwd, c.Gate)
		v.GateExit, v.Detail = code, tail
		p.appendEvent(Event{Kind: "gate", Contract: c.ID, Attempt: attempt,
			Text: fmt.Sprintf("exit %d", code)})
		if pass {
			v.Passed = true
			out.progress(fmt.Sprintf("  ✓ %s gate passed", c.ID))
			p.appendEvent(Event{Kind: "verdict", Contract: c.ID, Worker: worker, Attempt: attempt, Text: "PASS"})
			return v
		}
		out.progress(fmt.Sprintf("  ✗ %s gate failed (exit %d)", c.ID, code))
		priorGate = tail
	}
	v.Passed = false
	p.appendEvent(Event{Kind: "verdict", Contract: c.ID, Worker: v.Worker, Text: "FAIL (attempts exhausted)"})
	return v
}

// Run executes the whole supervision: every contract to a verdict, an optional
// supervisor summary when requested, then a filed report. The task verdicts
// alone determine convergence.
func Run(ctx context.Context, p *Plan, runner chat.Runner, out chatWriter) (*Result, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if p.ID == "" {
		p.ID = newID(p.Goal, nowFn())
	}
	if p.Cwd == "" {
		p.Cwd, _ = os.Getwd()
	}
	if err := p.save(); err != nil {
		return nil, err
	}
	for _, f := range p.Brief {
		if _, err := os.Stat(f); err != nil {
			return nil, fmt.Errorf("supervise: --brief %s: %w", f, err)
		}
	}

	res := &Result{Schema: schemaVersion, ID: p.ID, Goal: p.Goal}
	for _, c := range p.Contracts {
		if err := ctx.Err(); err != nil {
			out.progress("supervise: cancelled")
			break
		}
		v := runContract(ctx, p, c, runner, out)
		res.Verdicts = append(res.Verdicts, v)
		if !v.Passed && !p.KeepGoing {
			out.progress(fmt.Sprintf("supervise: %s did not converge; stopping (pass --keep-going to continue)", c.ID))
			break
		}
	}

	res.Converged = converged(res.Verdicts, len(p.Contracts))
	if strings.TrimSpace(p.Supervisor) != "" {
		res.Judgment, res.Judged = supervisorSummary(ctx, p, res.Verdicts, runner, out)
	}
	path, err := p.writeReport(res)
	if err != nil {
		return res, err
	}
	res.Report = path
	return res, nil
}

// converged is the contract spine: every contract passed. A supplied gate is
// authoritative; without one, a successful worker exit passes but remains
// marked Unverified so consumers can distinguish the weaker evidence.
func converged(vs []Verdict, total int) bool {
	if len(vs) < total {
		return false
	}
	for _, v := range vs {
		if !v.Passed {
			return false
		}
	}
	return true
}

// supervisorSummary asks an explicitly selected supervisor for optional context.
// Its availability and opinion never override task verdicts or convergence.
func supervisorSummary(ctx context.Context, p *Plan, vs []Verdict, runner chat.Runner, out chatWriter) (string, bool) {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the supervisor. The workers finished; here are the task results "+
		"(supplied gates are authoritative — do not override them):\n\n")
	for _, v := range vs {
		status := "FAIL"
		switch {
		case v.Passed && v.Unverified:
			status = "PASS (UNVERIFIED — no gate)"
		case v.Passed:
			status = "PASS"
		case v.Unverified:
			status = "FAIL (UNVERIFIED — no gate)"
		}
		fmt.Fprintf(&b, "- %s: %s (worker %s, %d attempt(s), exit %d)\n", v.Contract, status, v.Worker, v.Attempts, v.GateExit)
	}
	fmt.Fprintf(&b, "\nGoal: %s\n\nIn 3-6 sentences: is the goal met? What (if anything) is unconverged or "+
		"risky, and what is the next action? Be specific; do not restate the table.", p.Goal)

	res, err := chat.Invoke(ctx, chat.Options{
		Agent: p.Supervisor, Role: "supervisor", Instruction: b.String(),
		Cwd: p.Cwd, Timeout: p.turnTimeout(), Stream: liveStream(out),
	}, runner)
	if err != nil || strings.TrimSpace(res.Output) == "" {
		return "(supervisor summary unavailable)", false
	}
	p.appendEvent(Event{Kind: "summary", Worker: p.Supervisor, Text: oneLine(res.Output)})
	return strings.TrimSpace(res.Output), true
}

// Result is the machine-readable outcome of a supervision.
type Result struct {
	Schema    string    `json:"schema"`
	ID        string    `json:"id"`
	Goal      string    `json:"goal"`
	Verdicts  []Verdict `json:"verdicts"`
	Converged bool      `json:"converged"`
	Judgment  string    `json:"judgment,omitempty"` // compatibility name for the optional supervisor summary
	Judged    bool      `json:"judged"`             // true only when an explicitly requested summary completed
	Report    string    `json:"report,omitempty"`
}

// chatWriter is the minimal progress sink so the engine can narrate without
// importing cobra. The CLI passes a stdout-backed one; tests pass a no-op.
type chatWriter interface{ progress(string) }

type liveStreamer interface{ liveStream() io.Writer }

func liveStream(out chatWriter) io.Writer {
	if s, ok := out.(liveStreamer); ok {
		return s.liveStream()
	}
	return nil
}

func oneLine(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}
