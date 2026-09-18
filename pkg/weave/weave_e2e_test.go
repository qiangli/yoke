package weave

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// runWeave drives a FRESH weave command tree (cobra flag state is
// per-construction) with the given args, capturing stdout+stderr into one
// buffer and resolving the propagated exit code. Mirrors how a host
// (bashy) invokes NewWeaveCmd().
func runWeave(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := newWeaveCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	code := 0
	if err != nil {
		var ec interface{ ExitCode() int }
		if errors.As(err, &ec) {
			code = ec.ExitCode()
		} else {
			code = 1
		}
	}
	return buf.String(), code
}

// runSprint drives `bashy sprint` (the plan/handoff command), where the
// cloudbox shared-session verbs now live under `sprint session`.
func runSprint(t *testing.T, args ...string) (string, int) {
	t.Helper()
	// Most lifecycle fixtures exercise behavior after orientation. Keep their
	// setup explicit in one place; dedicated Sprint 175 tests cover legacy cards
	// with either field missing.
	if len(args) > 0 && args[0] == "add" {
		args = append(args, "--primary-goal", "deliver the fixture outcome", "--spec", "docs/test-plan.md")
	}
	cmd := NewSprintCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	code := 0
	if err != nil {
		var ec interface{ ExitCode() int }
		if errors.As(err, &ec) {
			code = ec.ExitCode()
		} else {
			code = 1
		}
	}
	return buf.String(), code
}

// runSprintSession runs a `sprint session <args>` invocation.
func runSprintSession(t *testing.T, args ...string) (string, int) {
	t.Helper()
	return runSprint(t, append([]string{"session"}, args...)...)
}

// TestWeaveCommandSurface locks in the post-migration command surface:
// the forge-only init-board is gone, cobra's completion subverb is
// disabled, the local-only open + the check introspection verb are
// present, and every lifecycle verb is still wired. Pure structure — no
// git, no filesystem — so it runs on every platform including the
// Windows CI leg.
func TestWeaveCommandSurface(t *testing.T) {
	cmd := newWeaveCmd()
	have := map[string]bool{}
	for _, c := range cmd.Commands() {
		have[c.Name()] = true
	}
	for _, n := range []string{
		"add", "start", "next", "prio", "point", "list", "pause", "resume",
		"autopilot", "status", "log", "remember", "recall", "memory", "attach", "say", "pull", "reverify", "prune", "abandon",
		"kill", "shell", "open", "reset", "wait", "check", "baton",
	} {
		if !have[n] {
			t.Errorf("missing subverb %q", n)
		}
	}
	// The conductor-coordination verbs moved to `bashy sprint` (plan
	// layer) — weave is execution-only now. They must NOT be here.
	for _, n := range []string{"sessions", "join", "note", "steer", "take", "handoff", "roster", "share", "shares", "unshare", "conduct"} {
		if have[n] {
			t.Errorf("subverb %q should have moved to `sprint`", n)
		}
	}
	if have["init-board"] {
		t.Error("init-board is forge-only and must not be registered")
	}
	if have["completion"] {
		t.Error("cobra completion subverb must be disabled")
	}
	if !cmd.CompletionOptions.DisableDefaultCmd {
		t.Error("CompletionOptions.DisableDefaultCmd must be true")
	}

	// Guard the YCODE_* -> WEAVE_* env rename at the doc level: start's
	// help advertises WEAVE_*, and no subverb help may name the old
	// YCODE_LOOM_* contract.
	var start *cobra.Command
	for _, c := range cmd.Commands() {
		if c.Name() == "start" {
			start = c
		}
		if strings.Contains(c.Long, "YCODE_LOOM") {
			t.Errorf("subverb %q help still references YCODE_LOOM_*", c.Name())
		}
	}
	if start == nil || !strings.Contains(start.Long, "WEAVE_*") {
		t.Error("start help should advertise WEAVE_* env vars")
	}
}

// TestWeaveExecuteErrorClassification pins the host-facing contract
// IsStructuredExit/ExitCode exist to fix: a real subverb failure (`prio 1
// --auto`, exercised above) already prints its own envelope and returns an
// *exitCodeError, so a host must NOT print err again — but an unknown
// subcommand never reaches a subverb, prints nothing on its own (the root's
// SilenceErrors), and is NOT a structured exit, so a host MUST print err
// itself or the failure is silent (the bug this pins down: a typo'd verb
// used to exit 1 with zero output).
func TestWeaveExecuteErrorClassification(t *testing.T) {
	t.Setenv("BASHY_AGENTIC", "") // force human rows, not the agent JSON envelope

	// Bare `weave` with no subcommand. A root with neither Run nor RunE is
	// not Runnable, so cobra returns flag.ErrHelp before ValidateArgs ever
	// runs — an Args validator there is dead code and the invocation exits
	// with no diagnostic at all. The root's RunE makes it Runnable, so empty
	// argv emits the same structured envelope every subverb does: a
	// weavecli exit carrying ExitInvalidArg, with the message written to the
	// command's own stderr rather than left to the host.
	//
	// SetArgs([]string{}) — NOT nil: with nil, cobra falls back to
	// os.Args[1:] (command.go:1105 exempts only a binary named
	// "cobra.test"), so the test would run go test's own argv and never
	// exercise empty argv at all.
	{
		cmd := newWeaveCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{})
		err := cmd.Execute()
		if err == nil {
			t.Fatal("bare weave should require a subcommand")
		}
		if !IsStructuredExit(err) {
			t.Errorf("bare weave should be a structured weavecli exit, got %v", err)
		}
		if code := ExitCode(err); code != weavecli.ExitInvalidArg {
			t.Errorf("ExitCode(bare weave) = %d, want %d", code, weavecli.ExitInvalidArg)
		}
		if !strings.Contains(buf.String(), "missing command") {
			t.Errorf("bare weave should name the missing command on stderr, got %q", buf.String())
		}
	}

	// Unknown subcommand: since #142 weave reports this itself (argerr.go
	// wraps every Args validator), same as flag errors — the message goes
	// to the command's own stderr naming the bad verb, and the error is a
	// structured ExitInvalidArg exit so a host driving Execute() stays
	// silent and never double-prints.
	{
		cmd := newWeaveCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"zzz"})
		err := cmd.Execute()
		if err == nil {
			t.Fatal("unknown subcommand should error")
		}
		if !IsStructuredExit(err) {
			t.Errorf("unknown subcommand should be a structured exit (weave already reported it), got %v", err)
		}
		if code := ExitCode(err); code != weavecli.ExitInvalidArg {
			t.Errorf("ExitCode = %d, want %d for an unknown subcommand", code, weavecli.ExitInvalidArg)
		}
		if !strings.Contains(buf.String(), "unknown command") || !strings.Contains(buf.String(), "zzz") {
			t.Errorf("weave should name the bad verb on its own stderr, got %q", buf.String())
		}
	}

	// Real subverb failure: `prio <issue> --auto` already emits its own
	// invalid_arg envelope and propagates a structured exit. A host must
	// recognize this and stay silent to avoid double-printing.
	{
		out, code := runWeave(t, "prio", "1", "--auto", "--json")
		if code == 0 {
			t.Fatalf("prio 1 --auto should fail, got exit=0 out=%s", out)
		}
		cmd := newWeaveCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"prio", "1", "--auto", "--json"})
		err := cmd.Execute()
		if !IsStructuredExit(err) {
			t.Errorf("prio 1 --auto failure should be a structured exit, got %v", err)
		}
		if ExitCode(err) != code {
			t.Errorf("ExitCode(err)=%d should match the subverb's own exit=%d", ExitCode(err), code)
		}
		if strings.Count(buf.String(), "invalid_arg") != 1 {
			t.Errorf("subverb output should contain exactly one envelope, got %q", buf.String())
		}
	}

	// Success: unaffected.
	if err := ExitCode(nil); err != 0 {
		t.Errorf("ExitCode(nil) should be 0, got %d", err)
	}
	if IsStructuredExit(nil) {
		t.Error("IsStructuredExit(nil) should be false")
	}
}

// TestWeaveCheckReportsStatus drives `weave check` and asserts it reports
// open as fully implemented (local-only now) and prio as the lone
// LLM-gated path, with no init-board row. check walks the command tree
// only, so it too needs no git.
func TestWeaveCheckReportsStatus(t *testing.T) {
	t.Setenv("BASHY_AGENTIC", "") // force human rows, not the agent JSON envelope
	out, code := runWeave(t, "check")
	if code != 0 {
		t.Fatalf("check exit=%d out=%s", code, out)
	}
	lineFor := func(name string) string {
		for _, ln := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(ln), name+" ") {
				return ln
			}
		}
		return ""
	}
	if l := lineFor("open"); !strings.Contains(l, "implemented") || strings.Contains(l, "require") {
		t.Errorf("open should be plain 'implemented', got %q", l)
	}
	if l := lineFor("prio"); !strings.Contains(l, "LLM") {
		t.Errorf("prio should name its LLM dependency, got %q", l)
	}
	if strings.Contains(out, "init-board") {
		t.Errorf("check must not list init-board:\n%s", out)
	}
}

// TestWeaveQueueLifecycleE2E exercises the real command tree end-to-end
// against an isolated HOME and a throwaway git repo: seed -> inspect ->
// reprioritize -> allocate a workspace -> open (local-only file:// URL) ->
// error paths -> prune. weave shells out to system git, so this skips
// cleanly where git is absent (e.g. the hermetic Windows leg).
func TestWeaveQueueLifecycleE2E(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("system git not available; weave lifecycle needs it")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("BASHY_AGENTIC", "")

	repo := t.TempDir()
	gitE2E(t, repo, "init", "-q", "-b", "main")
	gitE2E(t, repo, "config", "user.email", "e2e@test.local")
	gitE2E(t, repo, "config", "user.name", "E2E")
	if err := writeFile(filepath.Join(repo, "README.md"), "hello\n"); err != nil {
		t.Fatal(err)
	}
	gitE2E(t, repo, "add", "-A")
	gitE2E(t, repo, "commit", "-q", "-m", "init")
	t.Chdir(repo)

	mustOK := func(label string, args ...string) {
		t.Helper()
		out, code := runWeave(t, append(args, "--json")...)
		if code != 0 || !strings.Contains(out, `"status": "ok"`) {
			t.Fatalf("%s: exit=%d out=%s", label, code, out)
		}
	}

	// seed three issues at mixed priorities
	mustOK("add p1", "add", "alpha", "--priority", "p1")
	mustOK("add p2", "add", "beta")
	mustOK("add p0", "add", "gamma", "--priority", "p0")

	// list sees all three
	if out, code := runWeave(t, "list", "--json"); code != 0 || strings.Count(out, `"id"`) < 3 {
		t.Fatalf("list: exit=%d out=%s", code, out)
	}
	// next peeks the p0 (gamma) without mutating
	if out, _ := runWeave(t, "next", "--json"); !strings.Contains(out, "gamma") {
		t.Fatalf("next should peek the p0 issue, got %s", out)
	}
	// mutate: points + manual priority
	mustOK("point", "point", "1", "3")
	mustOK("prio", "prio", "2", "p0")
	// prio --auto degrades to a dependency_unhealthy envelope (no LLM here)
	if out, code := runWeave(t, "prio", "--auto", "--json"); code == 0 || !strings.Contains(out, "dependency_unhealthy") {
		t.Fatalf("prio --auto should degrade: exit=%d out=%s", code, out)
	}
	// invalid combo: `prio <issue> --auto` must emit a structured
	// invalid_arg envelope, not silently exit non-zero with no message.
	if out, code := runWeave(t, "prio", "1", "--auto", "--json"); code == 0 || !strings.Contains(out, "invalid_arg") {
		t.Fatalf("prio <issue> --auto should be invalid_arg: exit=%d out=%s", code, out)
	}
	if out, code := runWeave(t, "prio", "1", "--auto"); code == 0 || strings.TrimSpace(out) == "" {
		t.Fatalf("prio <issue> --auto must print an error, not exit silently: exit=%d out=%q", code, out)
	}

	// allocate a workspace for issue 1 without spawning a tool
	if out, code := runWeave(t, "start", "--issue", "1", "--no-spawn", "--json"); code != 0 || !strings.Contains(out, `"status": "ok"`) {
		t.Fatalf("start --no-spawn: exit=%d out=%s", code, out)
	}
	// open is local-only: a file:// workspace URL, never a forge field
	out, code := runWeave(t, "open", "1", "--json")
	if code != 0 || !strings.Contains(out, `"workspace_url": "file://`) {
		t.Fatalf("open 1: exit=%d out=%s", code, out)
	}
	if strings.Contains(out, "forge") {
		t.Errorf("open output must not mention a forge: %s", out)
	}
	// error paths
	if out, code := runWeave(t, "open", "999", "--json"); code == 0 || !strings.Contains(strings.ToLower(out), "not found") {
		t.Fatalf("open 999 should be not-found: exit=%d out=%s", code, out)
	}
	if _, code := runWeave(t, "open"); code == 0 {
		t.Error("open with no issue arg should fail (usage)")
	}

	// prune terminal/workspace state without error
	if _, code := runWeave(t, "prune", "--yes"); code != 0 {
		t.Errorf("prune exit=%d", code)
	}
}

// gitE2E runs `git -C dir <args...>` and fails the test on error. Used
// only to stand up / inspect the throwaway repo; the weave commands under
// test do their own git work.
func gitE2E(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", full...)
	// Deterministic identity so the test doesn't depend on global git config.
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=E2E", "GIT_AUTHOR_EMAIL=e2e@test.local",
		"GIT_COMMITTER_NAME=E2E", "GIT_COMMITTER_EMAIL=e2e@test.local",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
