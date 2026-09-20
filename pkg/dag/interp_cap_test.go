// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/policy/advice"
)

// capLangs are the two body languages the cap guard must cover identically:
// Classic ```bash and ```bashpp (Bash#). Both wire CapExecHandler outermost.
var capLangs = []string{"bash", "bashpp"}

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

// A command that needs an effect the target did not declare is denied BEFORE
// it runs: the shell sees status 126, the diagnostic names the command and the
// denied effects, and the side effect never happens. rm is served in-process
// by shell.Handler, so this also pins the guard running before that handler.
func TestCapOverCapDeniedBeforeSideEffects(t *testing.T) {
	for _, lang := range capLangs {
		t.Run(lang, func(t *testing.T) {
			dir := t.TempDir()
			victim := filepath.Join(dir, "victim.txt")
			if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			r := runCapped(t, dir, lang, "read", "rm victim.txt")
			if r.Status != StatusFailed || r.ExitCode != CapDeniedStatus {
				t.Fatalf("want failed/126, got %s exit=%d err=%v", r.Status, r.ExitCode, r.Err)
			}
			if !exists(dir, "victim.txt") {
				t.Fatal("denied rm still deleted the file — side effect leaked")
			}
			for _, want := range []string{`denied "rm"`, "destroy", "declared: read"} {
				if !strings.Contains(r.Stderr, want) {
					t.Errorf("diagnostic missing %q: %q", want, r.Stderr)
				}
			}
		})
	}
}

// Every leaf of a pipeline is checked, not just the head: `echo | tee` under a
// read-only cap denies tee (write) with 126 as the pipeline status and writes
// nothing. The head (echo, pure) is allowed.
func TestCapPipelineLeafCovered(t *testing.T) {
	for _, lang := range capLangs {
		t.Run(lang, func(t *testing.T) {
			dir := t.TempDir()
			r := runCapped(t, dir, lang, "read", "echo hi | tee out.txt")
			if r.ExitCode != CapDeniedStatus {
				t.Fatalf("want 126, got exit=%d stderr=%q", r.ExitCode, r.Stderr)
			}
			if exists(dir, "out.txt") {
				t.Fatal("denied pipeline leaf still wrote its output")
			}
			if !strings.Contains(r.Stderr, `denied "tee"`) || !strings.Contains(r.Stderr, "write") {
				t.Errorf("diagnostic = %q", r.Stderr)
			}
		})
	}
}

// A command the atlas does not classify fails closed as "unknown" (126, not
// the shell's 127), and never reaches the exec handler — the write it would
// have done through a real /bin/sh does not happen.
func TestCapUnknownCommandFailsClosed(t *testing.T) {
	for _, lang := range capLangs {
		t.Run(lang, func(t *testing.T) {
			dir := t.TempDir()
			r := runCapped(t, dir, lang, "read, write, exec", "sh -c 'touch via-sh.txt'")
			if r.ExitCode != CapDeniedStatus {
				t.Fatalf("want 126 for unclassified command, got exit=%d stderr=%q", r.ExitCode, r.Stderr)
			}
			if exists(dir, "via-sh.txt") {
				t.Fatal("unclassified command ran anyway")
			}
			if !strings.Contains(r.Stderr, `denied "sh"`) || !strings.Contains(r.Stderr, "unknown") {
				t.Errorf("diagnostic = %q", r.Stderr)
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

// An unparseable declaration on a hand-assembled Task never yields the
// unconstrained context: WithTaskCap returns an error AND a deny-all cap.
func TestWithTaskCapInvalidFailsClosed(t *testing.T) {
	ctx, err := WithTaskCap(context.Background(), []string{"teleport"})
	if err == nil {
		t.Fatal("want error for invalid effects")
	}
	cap, ok := advice.CapFrom(ctx)
	if !ok {
		t.Fatal("invalid effects returned the original unconstrained context")
	}
	if denied := cap.Exceeded([]string{"read"}); len(denied) == 0 {
		t.Fatal("fail-closed cap must deny a read")
	}
	// Empty Effects: no cap, ctx unchanged.
	ctx2, err := WithTaskCap(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := advice.CapFrom(ctx2); ok {
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
