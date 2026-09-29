package toolcmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/fleet"
)

// stubInvoke replaces chat.Invoke for one test and records the options it
// was handed. Nothing is ever executed: no agent CLI, no bashy self binary.
func stubInvoke(t *testing.T, out string, code int, err error) *chat.Options {
	t.Helper()
	t.Setenv("BASHY_SELF", filepath.Join(t.TempDir(), "no-such-bashy"))
	t.Setenv("BASHY_HOME", t.TempDir())
	got := &chat.Options{}
	prevInvoke, prevModel, prevFin := invoke, resolveModel, finalizeArgs
	invoke = func(ctx context.Context, opt chat.Options, r chat.Runner) (chat.Result, error) {
		*got = opt
		args := opt.ExecArgv
		if args == nil {
			args = []string{"-p", opt.Instruction}
		}
		if opt.DryRun {
			return chat.Result{Agent: "fakebin", Args: args}, nil
		}
		return chat.Result{Agent: "fakebin", Args: args, ExitCode: code, Output: out}, err
	}
	resolveModel = func(name string) (string, error) {
		if _, m, ok := strings.Cut(name, ":"); ok {
			return "id-" + m, nil
		}
		return "", nil
	}
	finalizeArgs = func(tool string, args []string, _ agentlaunch.Options) ([]string, error) { return args, nil }
	t.Cleanup(func() { invoke, resolveModel, finalizeArgs = prevInvoke, prevModel, prevFin })
	return got
}

func claudeTool() fleet.Tool {
	t := fleet.Tool{Name: "claude", Kind: "cli"}
	t.CLI.Binary = "claude"
	t.CLI.Launch.Exec = "claude --model {model} -p {prompt}"
	t.CLI.Launch.EventsStdout = "--output-format stream-json --verbose"
	t.CLI.Launch.EventsDone = fleet.EventsDone{Field: "type", Values: []string{"result"}}
	t.CLI.Launch.EventsOutcome = fleet.EventsOutcome{Path: "is_error", OK: []string{"false"}}
	return t
}

func codexTool() fleet.Tool {
	t := fleet.Tool{Name: "codex", Kind: "cli"}
	t.CLI.Binary = "codex"
	t.CLI.Launch.Exec = "codex exec --model {model} {prompt}"
	t.CLI.Launch.EventsStdout = "--json"
	t.CLI.Launch.EventsDone = fleet.EventsDone{Field: "type", Values: []string{"turn.completed"}}
	return t
}

func TestRenderSlash(t *testing.T) {
	cases := []struct{ slash, args, want string }{
		{"/deep-research {args}", "why is the sky blue", "/deep-research why is the sky blue"},
		{"/review", "focus on auth", "/review focus on auth"},
		{"/review", "", "/review"},
		{"/plan {args} now", "", "/plan  now"},
	}
	for _, c := range cases {
		if got := RenderSlash(c.slash, c.args); got != c.want {
			t.Errorf("RenderSlash(%q,%q) = %q, want %q", c.slash, c.args, got, c.want)
		}
	}
}

func TestRenderExec(t *testing.T) {
	got, err := RenderExec("codex exec review --json -m {model} {args}", []string{"--base", "main", "focus on auth"}, "", "/w", "/review")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "exec", "review", "--json", "--base", "main", "focus on auth"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	got, _ = RenderExec("codex exec review -m {model} -C {workspace} {args}", nil, "gpt-5", "/w", "")
	want = []string{"codex", "exec", "review", "-m", "gpt-5", "-C", "/w"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSplitArgs(t *testing.T) {
	got, err := SplitArgs(`--base main "focus on auth" 'a b' c\ d`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--base", "main", "focus on auth", "a b", "c d"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	if _, err := SplitArgs(`"open`); err == nil {
		t.Fatal("unterminated quote accepted")
	}
}

func TestPrintTemplateUsesSlashAsPrompt(t *testing.T) {
	got := stubInvoke(t, "", 0, nil)
	tool := claudeTool()
	cmd := fleet.ToolCommand{Name: "deep-research", Slash: "/deep-research {args}", Mode: "print"}
	res, err := Run(context.Background(), tool, cmd, "tides", Options{DryRun: true, Model: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Instruction != "/deep-research tides" || got.ExecArgv != nil || got.Agent != "claude:opus" || !got.DryRun {
		t.Fatalf("chat options: %+v", got)
	}
	if got.Stream == nil {
		t.Fatal("Stream must be set so chat inserts events_stdout")
	}
	if res.Outcome != OutcomeDryRun || res.Slash != "/deep-research tides" {
		t.Fatalf("res: %+v", res)
	}
}

func TestPrintExecOverride(t *testing.T) {
	got := stubInvoke(t, "", 0, nil)
	tool := codexTool()
	cmd := fleet.ToolCommand{Name: "review", Slash: "/review {args}", Mode: "print", Exec: "codex exec review --json {args}"}
	res, err := Run(context.Background(), tool, cmd, "", Options{DryRun: true, Argv: []string{"--uncommitted", "look at auth"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "review", "--json", "--uncommitted", "look at auth"}
	if !reflect.DeepEqual(got.ExecArgv, want) {
		t.Fatalf("ExecArgv %q want %q", got.ExecArgv, want)
	}
	if !reflect.DeepEqual(res.Argv, append([]string{"fakebin"}, want...)) {
		t.Fatalf("res.Argv %q", res.Argv)
	}
	// args string split when no Argv given
	_, _ = Run(context.Background(), tool, cmd, `--base main "x y"`, Options{DryRun: true})
	if w := []string{"exec", "review", "--json", "--base", "main", "x y"}; !reflect.DeepEqual(got.ExecArgv, w) {
		t.Fatalf("split ExecArgv %q", got.ExecArgv)
	}
	// wrong binary refused
	bad := cmd
	bad.Exec = "sh -c {args}"
	if _, err := Run(context.Background(), tool, bad, "", Options{DryRun: true}); err == nil {
		t.Fatal("exec template running another binary was accepted")
	}
}

const claudeOK = `{"type":"system","subtype":"init","session_id":"s-1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"thinking"}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":2,"result":"The answer is 42.","session_id":"s-1"}
`

const claudeUnavailable = `{"type":"system","subtype":"init","session_id":"s-2"}
{"type":"result","subtype":"success","is_error":false,"num_turns":0,"result":"/plan isn't available in this environment.","session_id":"s-2","total_cost_usd":0}
`

func TestPrintTurnOutput(t *testing.T) {
	stubInvoke(t, claudeOK, 0, nil)
	cmd := fleet.ToolCommand{Name: "deep-research", Slash: "/deep-research {args}", Mode: "print"}
	res, err := Run(context.Background(), claudeTool(), cmd, "q", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeSuccess || res.Text != "The answer is 42." || res.Session != "s-1" || res.Verdict != "succeeded" {
		t.Fatalf("res: %+v", res)
	}
	if _, err := os.Stat(res.Dir); !os.IsNotExist(err) {
		t.Fatalf("temp workdir %s not removed (%v)", res.Dir, err)
	}
}

func TestPrintTranscriptOutput(t *testing.T) {
	stubInvoke(t, claudeOK, 0, nil)
	cmd := fleet.ToolCommand{Name: "deep-research", Slash: "/deep-research", Mode: "print", Output: "transcript"}
	res, err := Run(context.Background(), claudeTool(), cmd, "q", Options{})
	if err != nil || res.Text != claudeOK {
		t.Fatalf("res %+v err %v", res, err)
	}
}

func TestPrintFileOutput(t *testing.T) {
	stubInvoke(t, claudeOK, 0, nil)
	dir := t.TempDir()
	for _, n := range []string{"report.md", "notes.md", "other.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := fleet.ToolCommand{Name: "deep-research", Slash: "/deep-research", Mode: "print", Output: "file:*.md"}
	res, err := Run(context.Background(), claudeTool(), cmd, "q", Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "notes.md"), filepath.Join(dir, "report.md")}
	if !reflect.DeepEqual(res.Artifacts, want) {
		t.Fatalf("artifacts %q want %q", res.Artifacts, want)
	}
}

func TestPrintUnavailableIsNotSuccess(t *testing.T) {
	stubInvoke(t, claudeUnavailable, 0, nil)
	cmd := fleet.ToolCommand{Name: "plan", Slash: "/plan {args}", Mode: "print"}
	res, err := Run(context.Background(), claudeTool(), cmd, "say hi", Options{})
	if err == nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if res.Outcome != OutcomeUnavailable {
		t.Fatalf("outcome %q, want unavailable", res.Outcome)
	}
}

func TestPrintFailureModes(t *testing.T) {
	cmd := fleet.ToolCommand{Name: "review", Slash: "/review", Mode: "print"}
	stubInvoke(t, "", 1, errors.New("exit status 1"))
	if res, err := Run(context.Background(), claudeTool(), cmd, "", Options{}); err == nil || res.Outcome != OutcomeError {
		t.Fatalf("nonzero exit: %+v %v", res, err)
	}
	// declared outcome with no success verdict is not success
	stubInvoke(t, `{"type":"result","is_error":true,"num_turns":3,"result":"boom"}`, 0, nil)
	if res, err := Run(context.Background(), claudeTool(), cmd, "", Options{}); err == nil || res.Outcome != OutcomeError {
		t.Fatalf("is_error true: %+v %v", res, err)
	}
	// codex: no events_outcome declared; final message from the last agent item
	stubInvoke(t, `{"type":"thread.started","thread_id":"th-9"}
{"type":"item.completed","item":{"type":"agent_message","text":"LGTM"}}
{"type":"turn.completed","usage":{}}`, 0, nil)
	res, err := Run(context.Background(), codexTool(), cmd, "", Options{})
	if err != nil || res.Text != "LGTM" || res.Session != "th-9" || res.Outcome != OutcomeSuccess || res.Verdict != "unverified" {
		t.Fatalf("codex: %+v %v", res, err)
	}
}

func TestRunRefusesInvalidCommand(t *testing.T) {
	stubInvoke(t, "", 0, nil)
	bad := fleet.ToolCommand{Name: "plan", Slash: "/plan", Mode: "print", Steps: []fleet.ToolCommandStep{{Say: "1"}}}
	if _, err := Run(context.Background(), claudeTool(), bad, "", Options{DryRun: true}); err == nil {
		t.Fatal("invalid command ran")
	}
}

// TestRealInvokeWithFakeRunner drives the REAL chat.Invoke with a fake
// chat.Runner: the argv reaches the runner, and nothing execs a binary.
func TestRealInvokeWithFakeRunner(t *testing.T) {
	t.Setenv("BASHY_SELF", filepath.Join(t.TempDir(), "no-such-bashy"))
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv(agentlaunch.UnsafeLaunchEnv, "1")
	fr := &fakeRunner{out: claudeOK}
	prev := runner
	runner = fr
	t.Cleanup(func() { runner = prev })
	cmd := fleet.ToolCommand{Name: "deep-research", Slash: "/deep-research {args}", Mode: "print"}
	res, err := Run(context.Background(), claudeTool(), cmd, "tides", Options{})
	if err != nil && !strings.Contains(err.Error(), "budget") {
		t.Fatalf("run: %v (%+v)", err, res)
	}
	if err != nil {
		t.Skipf("host budget policy refused the fake turn: %v", err)
	}
	if fr.calls != 1 || fr.args[len(fr.args)-1] != "/deep-research tides" {
		t.Fatalf("runner calls=%d args=%q", fr.calls, fr.args)
	}
	if res.Text != "The answer is 42." {
		t.Fatalf("text %q", res.Text)
	}
}

type fakeRunner struct {
	out   string
	calls int
	args  []string
}

func (f *fakeRunner) Run(ctx context.Context, agent string, args []string, cwd string) (string, int, error) {
	f.calls++
	f.args = args
	return f.out, 0, nil
}

func TestRefusalsAreStructured(t *testing.T) {
	cmd := fleet.ToolCommand{Name: "review", Slash: "/review", Mode: "print"}
	stubInvoke(t, "", 2, errors.New(`agent launch: refusing to launch "claude" with --dangerously-skip-permissions`))
	res, err := Run(context.Background(), claudeTool(), cmd, "", Options{})
	var re *RefusalError
	if !errors.As(err, &re) || re.Kind != RefusalLaunchGuard || res.Refusal != RefusalLaunchGuard {
		t.Fatalf("guard: %+v %v", res, err)
	}
	if !strings.Contains(res.Hint, "BASHY_ALLOW_UNSAFE_AGENT_LAUNCH") || !strings.Contains(res.Hint, "contain") {
		t.Fatalf("hint %q", res.Hint)
	}
	if os.Getenv(agentlaunch.UnsafeLaunchEnv) != "" {
		t.Fatal("toolcmd set the unsafe-launch bypass")
	}
	stubInvoke(t, "", 2, errors.New("chat: agent claude is already live (pid 1) — an agent is one identity"))
	res, err = Run(context.Background(), claudeTool(), cmd, "", Options{})
	if !errors.As(err, &re) || re.Kind != RefusalAgentLive || !strings.Contains(res.Hint, "--agent") {
		t.Fatalf("live: %+v %v", res, err)
	}
}
