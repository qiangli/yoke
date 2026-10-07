package toolcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/fleet"
)

// Test seams. Tests replace invoke (or pass a fake chat.Runner through
// runner) so no unit test ever spawns an agent CLI or bashy's own binary.
var (
	invoke                   = chat.Invoke
	runner       chat.Runner = nil // nil = chat's default exec runner
	resolveModel             = func(name string) (string, error) {
		l, err := agentlaunch.Resolve(name, agentlaunch.Options{DryRun: true})
		return l.Model, err
	}
	finalizeArgs = agentlaunch.FinalizeArgs
	mkTemp       = func() (string, error) { return os.MkdirTemp("", "bashy-toolcmd-") }
)

// printPlan is a fully rendered print-mode invocation.
type printPlan struct {
	Agent    string   // name chat.Invoke resolves (tool, tool:model, or an agent nick)
	Slash    string   // the rendered slash line (always; the prompt when there is no exec)
	ExecArgv []string // exec override: the full argv INCLUDING the binary; nil = the tool's launch template
}

// agentName is what the launch resolves: an explicit agent, else the tool,
// bound to the requested model when one is given.
func agentName(tool fleet.Tool, opts Options) string {
	if opts.Agent != "" {
		return opts.Agent
	}
	if opts.Model != "" {
		return tool.Name + ":" + opts.Model
	}
	return tool.Name
}

// planPrint renders the argv for a print command without running it.
// workspace is the workdir substituted for {workspace} in an exec template.
func planPrint(tool fleet.Tool, cmd fleet.ToolCommand, args string, opts Options, workspace string) (printPlan, error) {
	p := printPlan{Agent: agentName(tool, opts), Slash: RenderSlash(cmd.Slash, args)}
	if strings.TrimSpace(cmd.Exec) == "" {
		return p, nil
	}
	argv := opts.Argv
	if argv == nil {
		var err error
		if argv, err = SplitArgs(args); err != nil {
			return p, fmt.Errorf("toolcmd: %s:%s: %w", tool.Name, cmd.Name, err)
		}
	}
	model := ""
	if strings.Contains(cmd.Exec, fleet.ModelToken) {
		var m string
		var err error
		if opts.Catalog != nil {
			var launch agentlaunch.Launch
			launch, err = agentlaunch.ResolveWithCatalog(p.Agent, agentlaunch.Options{DryRun: true}, func() *fleet.Catalog { return opts.Catalog })
			m = launch.Model
		} else {
			m, err = resolveModel(p.Agent)
		}
		if err != nil {
			return p, fmt.Errorf("toolcmd: %s:%s: resolve model: %w", tool.Name, cmd.Name, err)
		}
		model = m
	}
	out, err := RenderExec(cmd.Exec, argv, model, workspace, p.Slash)
	if err != nil {
		return p, fmt.Errorf("toolcmd: %s:%s: %w", tool.Name, cmd.Name, err)
	}
	if bin := tool.Binary(); out[0] != bin && out[0] != tool.Name {
		return p, fmt.Errorf("toolcmd: %s:%s: exec template runs %q, not the tool's binary %q", tool.Name, cmd.Name, out[0], bin)
	}
	p.ExecArgv = out
	return p, nil
}

// RenderExec renders a per-command exec template. Tokens are whitespace
// separated (a flag list, never shell): {args} expands to the caller's argv
// (zero or more tokens), {model} to the model id (dropping a preceding flag
// when no model is bound, like the launch templates), {workspace} to the
// workdir and {prompt} to the rendered slash line.
func RenderExec(tmpl string, args []string, model, workspace, prompt string) ([]string, error) {
	fields := strings.Fields(tmpl)
	if len(fields) == 0 {
		return nil, errors.New("exec template is empty")
	}
	out := make([]string, 0, len(fields)+len(args))
	for _, f := range fields {
		switch f {
		case "{args}":
			out = append(out, args...)
		case fleet.ModelToken:
			if model != "" {
				out = append(out, model)
			} else if n := len(out); n > 1 && strings.HasPrefix(out[n-1], "-") {
				out = out[:n-1]
			}
		case fleet.WorkspaceToken:
			out = append(out, workspace)
		case fleet.PromptToken:
			out = append(out, prompt)
		default:
			out = append(out, strings.ReplaceAll(f, fleet.WorkspaceToken, workspace))
		}
	}
	return out, nil
}

// SplitArgs splits s into words honouring single quotes, double quotes and
// backslash escapes. No expansion of any kind happens.
func SplitArgs(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inWord := false
	var quote rune
	esc := false
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case r == '\\' && quote != '\'':
			esc, inWord = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote in args", quote)
	}
	if esc {
		return nil, errors.New("trailing backslash in args")
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}

// runPrint runs a print-mode command once through chat.Invoke.
func runPrint(ctx context.Context, tool fleet.Tool, cmd fleet.ToolCommand, args string, opts Options) (Result, error) {
	started := time.Now()
	res := Result{Tool: tool.Name, Command: cmd.Name, Mode: cmd.Mode, Started: started}
	finish := func(outcome string, err error) (Result, error) {
		res.Outcome, res.Duration = outcome, time.Since(started).Round(time.Millisecond).String()
		if err != nil {
			res.Error = err.Error()
		}
		return res, err
	}

	dir, tempDir := opts.Dir, false
	if dir == "" {
		if opts.DryRun {
			dir = "<tempdir>"
		} else {
			d, err := mkTemp()
			if err != nil {
				return finish(OutcomeError, fmt.Errorf("toolcmd: workdir: %w", err))
			}
			dir, tempDir = d, true
		}
	} else if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	res.Dir = dir

	plan, err := planPrint(tool, cmd, args, opts, dir)
	res.Slash = plan.Slash
	if err != nil {
		cleanupTemp(tempDir, dir, cmd)
		return finish(OutcomeError, err)
	}

	copt := chat.Options{
		Agent:       plan.Agent,
		Catalog:     opts.Catalog,
		Instruction: plan.Slash,
		Cwd:         dir,
		DryRun:      opts.DryRun,
		// A non-nil Stream asks chat for the tool's declared events_stdout
		// argv, which is what makes the verdict structured.
		Stream: io.Discard,
	}
	if opts.Stdout != nil {
		copt.Stream = opts.Stdout
	}
	if plan.ExecArgv != nil {
		fin, err := finalizeArgs(tool.Name, plan.ExecArgv[1:], agentlaunch.Options{DryRun: opts.DryRun})
		if err != nil {
			cleanupTemp(tempDir, dir, cmd)
			return finish(OutcomeError, err)
		}
		copt.ExecArgv = fin
	}

	timeout := CommandTimeout(cmd, opts)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cres, ierr := invoke(runCtx, copt, runner)
	res.Argv = append([]string{cres.Agent}, cres.Args...)
	res.ExitCode = cres.ExitCode
	if opts.DryRun {
		if ierr != nil {
			return finish(OutcomeError, ierr)
		}
		return finish(OutcomeDryRun, nil)
	}
	defer cleanupTemp(tempDir, dir, cmd)

	ev := parseEvents(tool, cres.Output)
	res.Session = ev.session
	res.Verdict = string(ev.verdict)
	switch output := cmd.Output; {
	case output == OutputTranscript:
		res.Text = cres.Output
	default:
		res.Text = ev.final
		if res.Text == "" && !ev.parsed {
			res.Text = strings.TrimSpace(cres.Output)
		}
	}
	if glob, ok := strings.CutPrefix(cmd.Output, OutputFilePrefix); ok {
		res.Artifacts = globArtifacts(dir, glob)
	}

	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		return finish(OutcomeTimeout, fmt.Errorf("toolcmd: %s:%s: timed out after %s", tool.Name, cmd.Name, timeout))
	case ctx.Err() != nil:
		return finish(OutcomeCancelled, fmt.Errorf("toolcmd: %s:%s: %w", tool.Name, cmd.Name, ctx.Err()))
	case ev.unavailable:
		// The probe lesson (docs/tool-commands-design.md §5): claude reports
		// subtype:success, is_error:false for a built-in it cannot run in print
		// mode. The exit status and verdict are not evidence; this is.
		return finish(OutcomeUnavailable, fmt.Errorf("%w: %s:%s: %s", ErrUnavailable, tool.Name, cmd.Name, firstLine(ev.final)))
	case ierr != nil:
		return finish(OutcomeError, ierr)
	case cres.ExitCode != 0:
		return finish(OutcomeError, fmt.Errorf("toolcmd: %s:%s: exit status %d", tool.Name, cmd.Name, cres.ExitCode))
	case ev.failed:
		return finish(OutcomeError, fmt.Errorf("toolcmd: %s:%s: the tool reported a failed turn", tool.Name, cmd.Name))
	case tool.CLI.Launch.EventsOutcome.Declared() && ev.parsed && ev.verdict != fleet.VerdictSucceeded:
		return finish(OutcomeError, fmt.Errorf("toolcmd: %s:%s: no success verdict from the tool (%s)", tool.Name, cmd.Name, ev.verdict))
	}
	return finish(OutcomeSuccess, nil)
}

// cleanupTemp removes a runner-created workdir unless it holds artifacts
// the caller asked for.
func cleanupTemp(temp bool, dir string, cmd fleet.ToolCommand) {
	if !temp || strings.HasPrefix(cmd.Output, OutputFilePrefix) {
		return
	}
	_ = os.RemoveAll(dir)
}

func globArtifacts(dir, glob string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, glob))
	var out []string
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && !st.IsDir() {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// unavailableRe matches claude's refusal of a built-in in print mode
// ("/plan isn't available in this environment."), measured 2026-09-28.
var unavailableRe = regexp.MustCompile(`(?i)(isn't|is not) available in this environment`)

// eventSummary is what the print runner reads out of a tool's event stream.
type eventSummary struct {
	parsed      bool // at least one JSON event line
	final       string
	session     string
	verdict     fleet.Verdict
	failed      bool
	unavailable bool
}

// parseEvents reads the NDJSON stream a tool writes under events_stdout:
// the terminal event (events_done) carries the verdict (events_outcome);
// the final message is the terminal event's `result` text (claude, agy) or
// the last agent message item (codex).
func parseEvents(tool fleet.Tool, out string) eventSummary {
	s := eventSummary{verdict: fleet.VerdictUnverified}
	launch := tool.CLI.Launch
	var lastText string
	for _, raw := range bytes.Split([]byte(out), []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var obj map[string]any
		if json.Unmarshal(line, &obj) != nil {
			continue
		}
		s.parsed = true
		for _, k := range []string{"session_id", "thread_id"} {
			if v, ok := obj[k].(string); ok && v != "" && s.session == "" {
				s.session = v
			}
		}
		if t := eventText(obj); t != "" {
			lastText = t
		}
		if typ, _ := obj["type"].(string); typ == "turn.failed" || typ == "error" {
			s.failed = true
		}
		terminal := launch.EventsDone.Declared() && launch.EventsDone.Match(line)
		if !launch.EventsDone.Declared() {
			_, terminal = obj["result"]
		}
		if !terminal {
			continue
		}
		s.verdict = launch.EventsOutcome.Read(line)
		if r, ok := obj["result"].(string); ok {
			lastText = r
			if n, ok := obj["num_turns"].(float64); ok && n == 0 && unavailableRe.MatchString(r) {
				s.unavailable = true
			}
		}
	}
	s.final = strings.TrimSpace(lastText)
	return s
}

// eventText pulls a message text out of one event, for the shapes measured
// on the wire: codex item.completed {item:{type:agent_message,text}}, a
// claude assistant message {message:{content:[{type:text,text}]}}.
func eventText(obj map[string]any) string {
	if item, ok := obj["item"].(map[string]any); ok {
		if typ, _ := item["type"].(string); typ == "agent_message" || typ == "assistant_message" {
			if t, ok := item["text"].(string); ok {
				return t
			}
		}
		return ""
	}
	if msg, ok := obj["message"].(map[string]any); ok {
		if role, _ := msg["role"].(string); role != "" && role != "assistant" {
			return ""
		}
		var b strings.Builder
		if parts, ok := msg["content"].([]any); ok {
			for _, p := range parts {
				if pm, ok := p.(map[string]any); ok && pm["type"] == "text" {
					if t, ok := pm["text"].(string); ok {
						b.WriteString(t)
					}
				}
			}
		}
		return b.String()
	}
	return ""
}
