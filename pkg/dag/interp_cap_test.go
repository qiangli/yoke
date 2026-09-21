// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/policy/advice"
)

// capLangs are the body languages the cap guard must cover identically:
// Classic ```bash and every Bash# tag (bsh/bashsharp official, bashpp/bash++
// aliases). All wire CapExecHandler outermost.
var capLangs = append([]string{"bash"}, BashSharpTags...)

// runCapped builds a one-target DAG with the given Effects: line (empty =
// none) and body, runs it in dir, and returns the result with the engine's
// stderr (the body streams there; the engine is not in capture mode) copied
// into Stderr so tests can read the denial diagnostic.
func runCapped(t *testing.T, dir, lang, effects, body string) TaskResult {
	t.Helper()
	md := "## Tasks\n\n### t\n"
	if effects != "" {
		md += "Effects: " + effects + "\n"
	}
	md += block(lang, body)
	e := contractEngine(t, dir, md)
	report, err := e.Run(context.Background(), "t")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := report.Results[0]
	r.Stderr += e.Stderr.(*bytes.Buffer).String()
	return r
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// A command whose atlas effects fit the declared cap runs normally.
func TestCapAllowedCommandRuns(t *testing.T) {
	for _, lang := range capLangs {
		t.Run(lang, func(t *testing.T) {
			dir := t.TempDir()
			// touch = write; cat = read; echo = pure (always allowed).
			r := runCapped(t, dir, lang, "read, write", "touch out.txt\ncat out.txt\necho done")
			if r.Status != StatusDone || r.ExitCode != 0 {
				t.Fatalf("allowed body should succeed; got %s exit=%d err=%v stderr=%q",
					r.Status, r.ExitCode, r.Err, r.Stderr)
			}
			if !exists(dir, "out.txt") {
				t.Fatal("allowed write did not happen")
			}
		})
	}
}

// A command that needs an effect the target did not declare is REPORTED, not
// denied: the report names the target, the command and the undeclared
// effects, and the command runs — the cap is advisory. rm is served
// in-process by shell.Handler, so this also pins the report running before
// that handler. A repeat of the same command is reported once.
func TestCapOverCapReportedAndRuns(t *testing.T) {
	for _, lang := range capLangs {
		t.Run(lang, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range []string{"a.txt", "b.txt"} {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			r := runCapped(t, dir, lang, "read", "rm a.txt\nrm b.txt")
			if r.Status != StatusDone || r.ExitCode != 0 {
				t.Fatalf("advisory cap must not fail the body; got %s exit=%d err=%v stderr=%q", r.Status, r.ExitCode, r.Err, r.Stderr)
			}
			if exists(dir, "a.txt") || exists(dir, "b.txt") {
				t.Fatal("reported rm did not run")
			}
			for _, want := range []string{`target "t"`, `"rm" needs destroy`, "Effects: read"} {
				if !strings.Contains(r.Stderr, want) {
					t.Errorf("report missing %q: %q", want, r.Stderr)
				}
			}
			if n := strings.Count(r.Stderr, `"rm" needs`); n != 1 {
				t.Errorf("want ONE report for the repeated command, got %d: %q", n, r.Stderr)
			}
		})
	}
}

// Every leaf of a pipeline is checked, not just the head: `echo | tee` under a
// read-only cap reports tee (write); the pipeline runs and writes. The head
// (echo, pure) is never reported.
func TestCapPipelineLeafCovered(t *testing.T) {
	for _, lang := range capLangs {
		t.Run(lang, func(t *testing.T) {
			dir := t.TempDir()
			r := runCapped(t, dir, lang, "read", "echo hi | tee out.txt")
			if r.ExitCode != 0 || !exists(dir, "out.txt") {
				t.Fatalf("pipeline must run; exit=%d stderr=%q", r.ExitCode, r.Stderr)
			}
			if !strings.Contains(r.Stderr, `"tee" needs write`) || strings.Contains(r.Stderr, `"echo"`) {
				t.Errorf("report = %q", r.Stderr)
			}
		})
	}
}

// A command the atlas does not classify is reported as "unknown" and runs:
// the atlas is a table bashy curates, not a law — a tool it has never heard
// of is exactly what must not break a build.
func TestCapUnknownCommandReportedAndRuns(t *testing.T) {
	for _, lang := range capLangs {
		t.Run(lang, func(t *testing.T) {
			dir := t.TempDir()
			r := runCapped(t, dir, lang, "read, write, exec", "sh -c 'touch via-sh.txt'")
			if r.ExitCode != 0 || !exists(dir, "via-sh.txt") {
				t.Fatalf("unclassified command must run; exit=%d stderr=%q", r.ExitCode, r.Stderr)
			}
			if !strings.Contains(r.Stderr, `"sh" needs unknown`) {
				t.Errorf("report = %q", r.Stderr)
			}
		})
	}
}

// No Effects: declaration → no cap → the body is unconstrained exactly as
// before enforcement existed (a destroy-class command runs).
func TestCapAbsentEffectsUnconstrained(t *testing.T) {
	for _, lang := range capLangs {
		t.Run(lang, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "gone.txt"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			r := runCapped(t, dir, lang, "", "rm gone.txt")
			if r.Status != StatusDone || r.ExitCode != 0 {
				t.Fatalf("uncapped body should run; got %s exit=%d stderr=%q", r.Status, r.ExitCode, r.Stderr)
			}
			if exists(dir, "gone.txt") {
				t.Fatal("uncapped rm did not run")
			}
		})
	}
}

// The declaration vocabulary IS policy/advice's: an atlas atom the old dag
// list never knew (exec) validates, and an atom outside the atlas is rejected
// with exit 2 and a message that lists the vocabulary.
func TestCapVocabularyIsAdvice(t *testing.T) {
	if _, err := BuildGraph(doc(t, "## Tasks\n\n### t\nEffects: exec, net\n"+block("bash", "true"))); err != nil {
		t.Fatalf("atlas atoms must validate: %v", err)
	}
	_, err := BuildGraph(doc(t, "## Tasks\n\n### t\nEffects: time\n"+block("bash", "true")))
	if err == nil {
		t.Fatal("non-atlas atom must be rejected")
	}
	if ExitCodeOf(err) != 2 || !strings.Contains(err.Error(), `"time"`) || !strings.Contains(err.Error(), "destroy") {
		t.Errorf("err = %v (exit %d)", err, ExitCodeOf(err))
	}
}

// An unparseable declaration is the one hard failure: WithTaskCap returns
// the error (the engine refuses the body); empty Effects sets no cap. The cap
// never rides the advice key — that one belongs to @guard, whose denial is
// the function author's own request.
func TestWithTaskCapInvalidIsAnError(t *testing.T) {
	if _, err := WithTaskCap(context.Background(), "t", []string{"teleport"}); err == nil {
		t.Fatal("want error for invalid effects")
	}
	ctx, err := WithTaskCap(context.Background(), "t", []string{"read"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ctx.Value(taskCapKey{}).(*taskCap); !ok {
		t.Fatal("valid Effects must set the task cap")
	}
	if _, ok := advice.CapFrom(ctx); ok {
		t.Fatal("the task cap must not ride the advice key")
	}
	ctx2, err := WithTaskCap(context.Background(), "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ctx2.Value(taskCapKey{}).(*taskCap); ok {
		t.Fatal("empty Effects must not set a cap")
	}
}

// The engine refuses to run a body whose Effects do not parse (bypassing
// BuildGraph's validation): failed, exit 2, and the body never executed.
func TestEngineInvalidEffectsRefusesBody(t *testing.T) {
	dir := t.TempDir()
	e := contractEngine(t, dir, "## Tasks\n\n### t\nEffects: read\n"+block("bash", "touch ran.txt"))
	e.Graph.Nodes["t"].Task.Effects = []string{"teleport"}
	report, err := e.Run(context.Background(), "t")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := report.Results[0]
	if r.Status != StatusFailed || r.ExitCode != 2 || r.Err == nil || !strings.Contains(r.Err.Error(), "teleport") {
		t.Fatalf("got %s exit=%d err=%v", r.Status, r.ExitCode, r.Err)
	}
	if exists(dir, "ran.txt") {
		t.Fatal("body ran despite invalid cap")
	}
}

// The shell re-entering itself is classified by its verb, not as an
// unclassified "bashy": `bashy go build` is the atlas entry for go
// (exec,net,write); a flag or a script path in verb position stays unknown.
func TestCapRecursiveBashyClassifiedByVerb(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "renamed-shell.exe")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_EXE", exe)
	ctx := WithTargetEffects(context.Background(), func(name string) ([]string, bool) {
		if name == "t2" {
			return []string{"write"}, true
		}
		if name == "uncapped" {
			return nil, true
		}
		return nil, false
	})
	cases := []struct {
		args    []string
		name    string
		effects []string
	}{
		{[]string{"bashy", "go", "build"}, "go", []string{"exec", "net", "write"}},
		{[]string{"/opt/bin/bashy.exe", "git", "status"}, "git", []string{"cred", "net", "read", "write"}},
		{[]string{exe, "go", "env"}, "go", []string{"exec", "net", "write"}},
		{[]string{"bashy", "dag", "t2"}, "dag t2", []string{"write"}},
		{[]string{"bashy", "dag", "uncapped"}, "dag uncapped", nil},
		{[]string{"bashy", "dag", "missing"}, "dag missing", nil},
		{[]string{"bashy", "-c", "echo"}, "bashy", nil},
		{[]string{"bashy", "scripts/x.sh"}, "bashy", nil},
		{[]string{"bashy"}, "bashy", nil},
		{[]string{"bashy", "pwd"}, "pwd", []string{"read"}},
	}
	for _, c := range cases {
		name, effects := classifyCommand(ctx, c.args)
		if name != c.name || strings.Join(effects, ",") != strings.Join(c.effects, ",") {
			t.Errorf("%v: got (%q, %v), want (%q, %v)", c.args, name, effects, c.name, c.effects)
		}
	}
}

// shimBashy puts a fake `bashy` first on PATH that records it ran, so the
// end-to-end recursive tests exercise the report without running the real
// shell.
func shimBashy(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("sh shim")
	}
	bin := filepath.Join(dir, "shim")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\ntouch \"$(dirname \"$0\")/../bashy-ran.txt\"\n"
	if err := os.WriteFile(filepath.Join(bin, "bashy"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// End to end: a body's `bashy go build` under Effects: write is reported as
// go's undeclared effects, naming the verb — and runs.
func TestCapRecursiveBashyReportNamesVerb(t *testing.T) {
	for _, lang := range capLangs {
		t.Run(lang, func(t *testing.T) {
			dir := t.TempDir()
			shimBashy(t, dir)
			r := runCapped(t, dir, lang, "write", "bashy go build ./...")
			if r.ExitCode != 0 || !exists(dir, "bashy-ran.txt") {
				t.Fatalf("reported command must run; exit=%d stderr=%q", r.ExitCode, r.Stderr)
			}
			if !strings.Contains(r.Stderr, `"go" needs exec,net`) {
				t.Errorf("report = %q", r.Stderr)
			}
		})
	}
}

// `bashy dag <t>` takes <t>'s own declaration: t2 declares write, so a caller
// capped to read reports "dag t2" needing write.
func TestCapRecursiveDagTakesSubtargetEffects(t *testing.T) {
	dir := t.TempDir()
	shimBashy(t, dir)
	md := "## Tasks\n\n### t\nEffects: read\n" + block("bash", "bashy dag t2") +
		"\n### t2\nEffects: write\n" + block("bash", "touch t2.txt")
	e := contractEngine(t, dir, md)
	report, err := e.Run(context.Background(), "t")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := report.Results[0]
	stderr := r.Stderr + e.Stderr.(*bytes.Buffer).String()
	if r.ExitCode != 0 || !exists(dir, "bashy-ran.txt") {
		t.Fatalf("reported command must run; exit=%d stderr=%q", r.ExitCode, stderr)
	}
	if !strings.Contains(stderr, `"dag t2" needs write`) {
		t.Errorf("report = %q", stderr)
	}
}
