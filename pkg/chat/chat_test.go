package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/qiangli/yoke/pkg/reduce"
)

type fakeRunner struct {
	agent  string
	args   []string
	cwd    string
	output string
}

type eventRunner struct{ output, events string }

func (r eventRunner) Run(_ context.Context, _ string, args []string, _ string) (string, int, error) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--events" {
			if err := os.WriteFile(args[i+1], []byte(r.events), 0o600); err != nil {
				return "", 1, err
			}
		}
	}
	return r.output, 0, nil
}

func (f *fakeRunner) Run(ctx context.Context, agent string, args []string, cwd string) (string, int, error) {
	f.agent, f.args, f.cwd = agent, append([]string{}, args...), cwd
	if f.output != "" {
		return f.output, 0, nil
	}
	return "ok\n", 0, nil
}

func TestInvokeEmitsNestedTier1SpansWithoutContent(t *testing.T) {
	permitUnsafeLaunch(t)
	pinCatalog(t)
	const promptSecret = "PROMPT_SECRET_sk-live-callsite"
	const completionSecret = "COMPLETION_SECRET_ghp_callsite"

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(old) })

	r := &fakeRunner{output: completionSecret}
	if _, err := Invoke(context.Background(), Options{
		Agent: "codex", Instruction: promptSecret, Cwd: t.TempDir(),
	}, r); err != nil {
		t.Fatal(err)
	}

	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("ended spans = %d, want turn + call", len(spans))
	}
	call, turn := spans[0], spans[1]
	if call.Parent().SpanID() != turn.SpanContext().SpanID() {
		t.Fatalf("call parent = %s, want turn %s", call.Parent().SpanID(), turn.SpanContext().SpanID())
	}
	wire, err := json.Marshal(spans)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{promptSecret, completionSecret, "gen_ai.input.messages", "gen_ai.output.messages"} {
		if strings.Contains(string(wire), forbidden) {
			t.Errorf("call-site span leaked Tier 2/3 content %q", forbidden)
		}
	}
}

func TestResolveAgentRole(t *testing.T) {
	got, err := ResolveAgent("", "conductor")
	if err != nil {
		t.Fatal(err)
	}
	if got != "claude" {
		t.Fatalf("conductor role resolved to %q, want claude", got)
	}
}

func TestBuildPromptIncludesContext(t *testing.T) {
	prompt, err := BuildPrompt(Options{
		Instruction: "implement the issue",
		Context:     []string{"deployment target: staging"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "implement the issue") || !strings.Contains(prompt, "deployment target: staging") {
		t.Fatalf("prompt missing expected content:\n%s", prompt)
	}
}

func TestInvokeUsesSeededHeadlessContract(t *testing.T) {
	permitUnsafeLaunch(t)
	pinCatalog(t)
	r := &fakeRunner{}
	res, err := Invoke(context.Background(), Options{
		Agent:       "codex",
		Instruction: "review this",
		Cwd:         "/tmp/work",
	}, r)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || r.agent != "codex" {
		t.Fatalf("unexpected result=%+v runner.agent=%q", res, r.agent)
	}
	if len(r.args) < 5 || r.args[0] != "exec" || r.args[1] != "--skip-git-repo-check" {
		t.Fatalf("missing codex headless contract: %#v", r.args)
	}
	if r.args[len(r.args)-1] != "review this" {
		t.Fatalf("last arg should be prompt, got %#v", r.args)
	}
}

func TestInvokeReducesOversizeAgentTurnAndSpillsFullOutput(t *testing.T) {
	permitUnsafeLaunch(t)
	pinCatalog(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	full := strings.Repeat("agent progress at "+filepath.Join(home, "fixture", "result")+" must remain recoverable\n", 1400)
	canonical := strings.ReplaceAll(full, home, "$HOME")
	r := &fakeRunner{output: full}

	res, err := Invoke(context.Background(), Options{
		Agent: "codex", Instruction: "summarize", Cwd: t.TempDir(),
	}, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Output) > reduce.DefaultBudgetBytes {
		t.Fatalf("reduced output = %d bytes, budget = %d", len(res.Output), reduce.DefaultBudgetBytes)
	}
	if !strings.Contains(res.Output, "full: bashy out ") {
		t.Fatalf("result has no recovery marker: %q", res.Output[len(res.Output)-min(len(res.Output), 300):])
	}
	entries, err := os.ReadDir(filepath.Join(home, ".bashy", "chat", "output"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("spill entries = %d, want 1", len(entries))
	}
	spilled, err := os.ReadFile(filepath.Join(home, ".bashy", "chat", "output", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if string(spilled) != canonical {
		t.Fatalf("spilled output differs from canonical complete turn")
	}
	if strings.Contains(res.Output, home) || strings.Contains(string(spilled), home) {
		t.Fatalf("raw fixture home reached reduced diagnostics or recovery artifact")
	}
	var recovered bytes.Buffer
	handle := strings.Fields(strings.SplitN(strings.SplitN(res.Output, "full: bashy out ", 2)[1], " |", 2)[0])[0]
	if err := reduce.Recover(reduce.NewStore(filepath.Join(home, ".bashy", "chat", "output")), handle, &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.String() != canonical {
		t.Fatal("recovery did not return the canonical complete turn")
	}
}

func TestInvokeDeduplicatesTelemetryHintsWithRecoverableArtifact(t *testing.T) {
	permitUnsafeLaunch(t)
	pinCatalog(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	if err := os.MkdirAll(filepath.Join(home, "config", "bashy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config", "bashy", "secrets.map"), []byte("CHAT_HINT_SECRET=@chat-hint-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const secret = "synthetic-hint-secret-285"
	t.Setenv("CHAT_HINT_SECRET", secret)
	hint := "bashy: telemetry on → " + filepath.Join(home, ".agents", "otel", "spool", "spans.jsonl") + " (service=bashy)\n"
	full := "start " + secret + "\n" + hint + "work remains\n" + hint + hint + "done\n"
	canonicalRedacted := strings.ReplaceAll(strings.ReplaceAll(full, home, "$HOME"), secret, "[redacted:CHAT_HINT_SECRET]")
	var stream bytes.Buffer

	res, err := Invoke(context.Background(), Options{
		Agent: "codex", Instruction: "summarize", Cwd: t.TempDir(), Stream: &stream,
	}, &fakeRunner{output: full})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != stream.String() {
		t.Fatalf("result and model-visible stream diverged:\nresult=%q\nstream=%q", res.Output, stream.String())
	}
	if strings.Count(res.Output, "bashy: telemetry on") != 1 ||
		strings.Count(res.Output, "2 duplicate telemetry hints suppressed") != 1 {
		t.Fatalf("duplicate telemetry view = %q", res.Output)
	}
	if len(res.Output) > reduce.DefaultBudgetBytes || strings.Contains(res.Output, home) || strings.Contains(res.Output, secret) {
		t.Fatalf("unbounded or private model-visible view: %q", res.Output)
	}
	parts := strings.SplitN(res.Output, "full: bashy out ", 2)
	if len(parts) != 2 {
		t.Fatalf("missing runnable recovery: %q", res.Output)
	}
	handle := strings.Fields(parts[1])[0]
	var recovered bytes.Buffer
	if err := reduce.Recover(reduce.NewStore(filepath.Join(home, ".bashy", "chat", "output")), handle, &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.String() != canonicalRedacted {
		t.Fatalf("recovered artifact is not complete canonicalized/redacted input:\ngot  %q\nwant %q", recovered.String(), canonicalRedacted)
	}
}

func TestInvokeKeepsSmallAgentTurnInline(t *testing.T) {
	permitUnsafeLaunch(t)
	pinCatalog(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	r := &fakeRunner{output: "small answer at " + filepath.Join(home, "fixture") + "\n"}

	res, err := Invoke(context.Background(), Options{
		Agent: "codex", Instruction: "summarize", Cwd: t.TempDir(),
	}, r)
	if err != nil {
		t.Fatal(err)
	}
	want := "small answer at " + filepath.Join("$HOME", "fixture") + "\n"
	if res.Output != want {
		t.Fatalf("small output = %q, want %q", res.Output, want)
	}
	if _, err := os.Stat(filepath.Join(home, ".bashy", "chat", "output")); !os.IsNotExist(err) {
		t.Fatalf("small output created a spill store: %v", err)
	}
}

func TestInvokeRedactsBeforeInlineSpillAndStream(t *testing.T) {
	permitUnsafeLaunch(t)
	pinCatalog(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	if err := os.MkdirAll(filepath.Join(home, "config", "bashy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config", "bashy", "secrets.map"), []byte("CHAT_TEST_SECRET=@chat-test-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const secret = "synthetic-chat-secret-9f6d"
	t.Setenv("CHAT_TEST_SECRET", secret)
	full := strings.Repeat("progress "+secret+" at "+filepath.Join(home, "fixture")+"\n", 3000)
	var stream bytes.Buffer
	res, err := Invoke(context.Background(), Options{Agent: "codex", Instruction: "summarize", Cwd: t.TempDir(), Stream: &stream}, &fakeRunner{output: full})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Output) > reduce.DefaultBudgetBytes || stream.Len() > reduce.DefaultBudgetBytes {
		t.Fatal("unbounded reduced view")
	}
	if strings.Contains(res.Output, secret) || strings.Contains(stream.String(), secret) {
		t.Fatal("secret reached a model-visible view")
	}
	if strings.Contains(res.Output, home) || strings.Contains(stream.String(), home) {
		t.Fatal("raw fixture home reached result or Stream")
	}
	entries, err := os.ReadDir(filepath.Join(home, ".bashy", "chat", "output"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		blob, err := os.ReadFile(filepath.Join(home, ".bashy", "chat", "output", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(blob), secret) {
			t.Fatal("secret reached recovery artifact")
		}
	}
}

func TestInvokeReductionOptOutStillRedacts(t *testing.T) {
	permitUnsafeLaunch(t)
	pinCatalog(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_OUTPUT_REDUCE", "off")
	full := strings.Repeat("complete answer\n", 4000)
	res, err := Invoke(context.Background(), Options{Agent: "codex", Instruction: "summarize", Cwd: t.TempDir()}, &fakeRunner{output: full})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != full {
		t.Fatal("explicit opt-out did not retain complete output")
	}
}

func TestInvokeBoundsEventStream(t *testing.T) {
	permitUnsafeLaunch(t)
	pinCatalog(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	var events bytes.Buffer
	full := strings.Repeat("{\"type\":\"tool.call\",\"path\":\""+filepath.Join(home, "fixture")+"\"}\n", 2000)
	_, err := Invoke(context.Background(), Options{Agent: "ycode", Instruction: "summarize", Cwd: t.TempDir(), EventStream: &events}, eventRunner{output: "ok\n", events: full})
	if err != nil {
		t.Fatal(err)
	}
	if events.Len() > reduce.DefaultBudgetBytes {
		t.Fatal("unbounded event view")
	}
	if !strings.Contains(events.String(), "full: bashy out ") {
		t.Fatal("event recovery marker missing")
	}
	if strings.Contains(events.String(), home) {
		t.Fatal("raw fixture home reached EventStream")
	}
}

func TestInvokeCanOverrideCodexSandbox(t *testing.T) {
	permitUnsafeLaunch(t)
	pinCatalog(t)
	// A non-danger sandbox override sets --sandbox <value>.
	r := &fakeRunner{}
	_, err := Invoke(context.Background(), Options{
		Agent: "codex", Instruction: "commit this", Sandbox: "workspace-write",
	}, r)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.args, " "); !strings.Contains(got, "--sandbox workspace-write") {
		t.Fatalf("sandbox override missing from args: %#v", r.args)
	}
}

func TestInvokeCodexDangerFullAccessIsNonInteractive(t *testing.T) {
	// danger-full-access → the fully non-interactive bypass flag (no approval/trust
	// popup that would hang a headless runner), and NOT a plain --sandbox value.
	// Asserts the RENDERING; guardUnsafeArgs (tested in TestUnsafeLaunch*) is what
	// decides whether this rendering is permitted to run at all.
	permitUnsafeLaunch(t)
	r := &fakeRunner{}
	_, err := Invoke(context.Background(), Options{
		Agent: "codex", Instruction: "commit this", Sandbox: "danger-full-access",
	}, r)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.args, " ")
	if !strings.Contains(got, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("expected non-interactive bypass flag: %#v", r.args)
	}
	if strings.Contains(got, "--sandbox") {
		t.Fatalf("danger-full-access must not emit --sandbox: %#v", r.args)
	}
}

func TestInvokeAiderHeadlessProfile(t *testing.T) {
	// aider must be driven headlessly with --message (prompt appended as its
	// value) + --yes-always + --no-git; bare `aider <prompt>` opens the TUI.
	// --yes-always is aider's approval-gate kill-switch, so it is emitted only
	// when unsafe launches are permitted — this test asserts that full headless
	// argv, so it opts in (the default now launches aider under its own gate).
	permitUnsafeLaunch(t)
	r := &fakeRunner{}
	_, err := Invoke(context.Background(), Options{
		Agent: "aider", Instruction: "review this",
	}, r)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.args, " ")
	if !strings.Contains(got, "--yes-always") || !strings.Contains(got, "--no-git") {
		t.Fatalf("aider headless flags missing: %#v", r.args)
	}
	if n := len(r.args); n < 2 || r.args[n-2] != "--message" || r.args[n-1] != "review this" {
		t.Fatalf("prompt must be the --message value (last two args): %#v", r.args)
	}
}

func TestForcedShellEnv(t *testing.T) {
	base := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/u",
		"SHELL=/bin/zsh",             // should be replaced
		"CLAUDE_CODE_SHELL=/bin/zsh", // should be replaced
	}
	got := forcedShellEnv(base, "/opt/bashy", "/shims")

	find := func(prefix string) string {
		var v string
		n := 0
		for _, kv := range got {
			if strings.HasPrefix(kv, prefix) {
				v = kv[len(prefix):]
				n++
			}
		}
		if n != 1 {
			t.Fatalf("expected exactly one %q, found %d in %#v", prefix, n, got)
		}
		return v
	}

	if p := find("PATH="); p != "/shims"+string(os.PathListSeparator)+"/usr/bin:/bin" {
		t.Fatalf("shim dir not prepended to PATH: %q", p)
	}
	if runtime.GOOS == "windows" {
		for _, kv := range got {
			if strings.HasPrefix(kv, "SHELL=") {
				t.Fatalf("SHELL should not be set on Windows: %#v", got)
			}
		}
	} else {
		if s := find("SHELL="); s != "/opt/bashy" {
			t.Fatalf("SHELL not pinned to bashy: %q", s)
		}
	}
	if s := find("CLAUDE_CODE_SHELL="); s != "/opt/bashy" {
		t.Fatalf("CLAUDE_CODE_SHELL not pinned to bashy: %q", s)
	}
	if h := find("HOME="); h != "/home/u" {
		t.Fatalf("unrelated var mangled: HOME=%q", h)
	}
}

func TestForcedShellEnvNoShimNoPath(t *testing.T) {
	// With no shim dir, PATH is left untouched and no PATH entry is invented.
	got := forcedShellEnv([]string{"PATH=/usr/bin", "HOME=/h"}, "/opt/bashy", "")
	if !slices.Contains(got, "PATH=/usr/bin") {
		t.Fatalf("PATH should be unchanged when shimDir empty: %#v", got)
	}
}
