// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package fleet

import (
	"strings"
	"testing"
)

func TestForkArgvSubstitutesSessionModelPrompt(t *testing.T) {
	tool := Tool{CLI: ToolCLI{Launch: ToolLaunch{
		ForkExec:   "claude --resume {session} --fork-session --dangerously-skip-permissions -p {prompt}",
		SessionEnv: []string{"TEST_CLAUDE_SESS"},
	}}}
	if !tool.CanFork() {
		t.Fatal("CanFork should be true for a tool with fork_exec")
	}
	got := strings.Join(tool.ForkArgv("", "sess-123", "do the thing"), " ")
	want := "claude --resume sess-123 --fork-session --dangerously-skip-permissions -p do the thing"
	if got != want {
		t.Fatalf("ForkArgv = %q\nwant %q", got, want)
	}
	// The prefix is what agentlaunch inserts between the binary and the prompt.
	prefix, ok := tool.ForkArgvPrefix("", "sess-123")
	if !ok {
		t.Fatal("ForkArgvPrefix should resolve")
	}
	if p := strings.Join(prefix, " "); p != "--resume sess-123 --fork-session --dangerously-skip-permissions -p" {
		t.Fatalf("ForkArgvPrefix = %q", p)
	}
	// CurrentSession reads the first set SessionEnv var.
	t.Setenv("TEST_CLAUDE_SESS", "abc123")
	if tool.CurrentSession() != "abc123" {
		t.Fatalf("CurrentSession = %q", tool.CurrentSession())
	}
}

// The optional {model} in a fork template drops when no model is given (inherit the
// session's model) and substitutes when one is (--model transplant).
func TestForkArgvModelIsOptional(t *testing.T) {
	tool := Tool{CLI: ToolCLI{Launch: ToolLaunch{
		ForkExec: "claude --resume {session} --fork-session --model {model} -p {prompt}",
	}}}
	if got := strings.Join(tool.ForkArgv("", "s", "p"), " "); got != "claude --resume s --fork-session -p p" {
		t.Fatalf("empty model not dropped: %q", got)
	}
	if got := strings.Join(tool.ForkArgv("opus-x", "s", "p"), " "); got != "claude --resume s --fork-session --model opus-x -p p" {
		t.Fatalf("model not substituted: %q", got)
	}
}

func TestNoForkTemplateMeansNoFork(t *testing.T) {
	plain := Tool{CLI: ToolCLI{Launch: ToolLaunch{Exec: "codex exec {prompt}"}}}
	if plain.CanFork() {
		t.Fatal("a tool without fork_exec must not claim to fork (codex: headless resume mutates the parent)")
	}
	if _, ok := plain.ForkArgvPrefix("", "s"); ok {
		t.Fatal("ForkArgvPrefix must be false without a fork template")
	}
	if plain.CurrentSession() != "" {
		t.Fatal("CurrentSession with no SessionEnv should be empty")
	}
}

// The shipped claude tool must declare a real fork; codex must not.
func TestBaselineClaudeForksCodexDoesNot(t *testing.T) {
	cat := New(WithoutLocalStore())
	claude, ok := cat.Tool("claude")
	if !ok {
		t.Fatal("baseline claude tool missing")
	}
	if !claude.CanFork() {
		t.Fatal("baseline claude should declare fork_exec (delegate self relies on it)")
	}
	// ycode is first-party and now ships a headless --fork, so it forks too.
	if ycode, ok := cat.Tool("ycode"); ok && !ycode.CanFork() {
		t.Fatal("baseline ycode should declare fork_exec (its --fork inherits the session transcript)")
	}
	if codex, ok := cat.Tool("codex"); ok && codex.CanFork() {
		t.Fatal("codex must NOT declare a fork — its headless resume mutates the parent thread")
	}
}

func TestBaselineCodexDeclaresCurrentSessionEnvironment(t *testing.T) {
	cat := New(WithoutLocalStore())
	codex, ok := cat.Tool("codex")
	if !ok {
		t.Fatal("baseline codex tool missing")
	}
	t.Setenv("CODEX_SESSION_ID", "session-id")
	t.Setenv("CODEX_THREAD_ID", "thread-id")
	if got := codex.CurrentSession(); got != "session-id" {
		t.Fatalf("CurrentSession = %q, want first declared session id", got)
	}
	t.Setenv("CODEX_SESSION_ID", "")
	if got := codex.CurrentSession(); got != "thread-id" {
		t.Fatalf("CurrentSession fallback = %q, want thread id", got)
	}
}
