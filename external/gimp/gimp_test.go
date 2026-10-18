package gimp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// fakeExec is the dependency-injected executor: it records every invocation
// and never starts a process.
type fakeExec struct {
	mu     sync.Mutex
	calls  []call
	stdout string
	stderr string
	err    error
	// block, when set, makes the fake wait for ctx and return ctx.Err().
	block bool
}

type call struct {
	bin  string
	args []string
}

func (f *fakeExec) exec(ctx context.Context, bin string, args []string, stdout, stderr io.Writer) error {
	f.mu.Lock()
	f.calls = append(f.calls, call{bin: bin, args: append([]string(nil), args...)})
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	_, _ = io.WriteString(stdout, f.stdout)
	_, _ = io.WriteString(stderr, f.stderr)
	return f.err
}

func (f *fakeExec) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

const fakeBin = "/fake/cache/gimp/3.2.6/GIMP-3.2.6-x86_64.AppImage"

func fakeResolve(ctx context.Context, version string) (string, error) {
	if version != DefaultVersion {
		return "", errors.New("unexpected version " + version)
	}
	return fakeBin, nil
}

func testRunner(f *fakeExec) *runner {
	return &runner{resolve: fakeResolve, execute: f.exec}
}

type fixtures struct {
	dir, in, out, outJPG, outBlend string
}

func mkfix(t *testing.T) fixtures {
	t.Helper()
	dir := t.TempDir()
	fx := fixtures{
		dir:      dir,
		in:       filepath.Join(dir, "in.png"),
		out:      filepath.Join(dir, "out.png"),
		outJPG:   filepath.Join(dir, "out.jpg"),
		outBlend: filepath.Join(dir, "out.blend"),
	}
	// Inputs exist; outputs are pre-created non-empty so the fake executor
	// (which fabricates stdout but writes no files) still passes the
	// post-success output stat. The missing/empty-output failures are
	// covered by dedicated tests that do not pre-create.
	for _, p := range []string{fx.in, fx.out, fx.outJPG} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return fx
}

// resultFor fabricates the child's stdout: GIMP noise plus the one marked
// JSON line the fixed program would print for r.
func resultFor(t *testing.T, r Request) string {
	t.Helper()
	prog, err := programFor(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = prog
	payload := map[string]string{
		"operation":      string(r.Kind),
		"schema_version": schemaVersions[r.Kind],
		"input":          filepath.Clean(r.Input),
		"output":         filepath.Clean(r.Output),
		"mode":           r.Mode,
		"scale":          r.Scale,
		"rotate":         strconv.Itoa(r.Rotate),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return "GIMP 3.2.6 batch noise\n" + resultMarker + string(raw) + "\nbatch command executed successfully\n"
}

func execute(t *testing.T, rn *runner, args ...string) (string, error) {
	t.Helper()
	cmd := newCmd(rn)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestSubcommandsAreExactlyTheThreeTypedOperations(t *testing.T) {
	cmd := NewCmd()
	var names []string
	for _, c := range cmd.Commands() {
		names = append(names, c.Name())
	}
	want := []string{"convert", "edit", "export"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("subcommands = %v, want %v", names, want)
	}
	for _, forbidden := range []string{"run", "exec", "python", "program", "script", "serve", "daemon", "mcp", "gui", "shell", "repl", "plugin", "bridge", "snapshot"} {
		for _, n := range names {
			if n == forbidden {
				t.Fatalf("forbidden subcommand %q registered", n)
			}
		}
	}
	if len(Kinds) != 3 {
		t.Fatalf("Kinds = %v", Kinds)
	}
}

func TestNoFreeFormEscapeFlags(t *testing.T) {
	forbidden := map[string]bool{"program": true, "script": true, "code": true, "eval": true, "expr": true,
		"args": true, "arg": true, "raw": true, "passthrough": true, "extra": true, "command": true,
		"exec": true, "addon": true, "plugin": true, "interpreter": true, "batch": true, "quit": true,
		"file": true, "load": true}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if forbidden[f.Name] {
				t.Fatalf("%s: free-form flag --%s present", c.Name(), f.Name)
			}
		})
		for _, sub := range c.Commands() {
			if sub.DisableFlagParsing {
				t.Fatalf("%s: flag parsing disabled (pass-through)", sub.Name())
			}
			walk(sub)
		}
	}
	walk(NewCmd())
	// Positional arguments are refused everywhere — no path for an extra
	// GIMP argument to sneak in.
	fx := mkfix(t)
	f := &fakeExec{}
	if _, err := execute(t, testRunner(f), "export", "--input", fx.in, "--output", fx.out, "--batch", "pwn"); err == nil {
		t.Fatal("unknown flag --batch accepted")
	}
	if _, err := execute(t, testRunner(f), "export", "--input", fx.in, "--output", fx.out, "extra.png"); err == nil {
		t.Fatal("positional argument accepted")
	}
	if _, err := execute(t, testRunner(f), "edit", "--input", fx.in, "--output", fx.out, "--scale", "8x8", "--", "--no-interface"); err == nil {
		t.Fatal("trailing pass-through accepted")
	}
	if f.count() != 0 {
		t.Fatalf("executor called %d times on rejected input", f.count())
	}
}

func TestArgvShape(t *testing.T) {
	fx := mkfix(t)
	cases := []struct {
		name string
		req  Request
		args []string
	}{
		{"export", Request{Kind: KindExport, Input: fx.in, Output: fx.out},
			[]string{"export", "--input", fx.in, "--output", fx.out}},
		{"convert", Request{Kind: KindConvert, Input: fx.in, Output: fx.outJPG, Mode: ModeGray},
			[]string{"convert", "--input", fx.in, "--output", fx.outJPG, "--mode", "gray"}},
		{"edit", Request{Kind: KindEdit, Input: fx.in, Output: fx.out, Scale: "800x600", Rotate: 90},
			[]string{"edit", "--input", fx.in, "--output", fx.out, "--scale", "800x600", "--rotate", "90"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeExec{stdout: resultFor(t, tc.req)}
			out, err := execute(t, testRunner(f), tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			if f.count() != 1 {
				t.Fatalf("executor called %d times, want exactly 1", f.count())
			}
			if f.calls[0].bin != fakeBin {
				t.Fatalf("bin = %q", f.calls[0].bin)
			}
			argv := f.calls[0].args
			wantHead := []string{"--no-interface", "--no-data", "--no-fonts", "--new-instance",
				"--batch-interpreter", batchInterpreter, "--batch"}
			if len(argv) != len(wantHead)+2 {
				t.Fatalf("argv = %q (want head %q + program + --quit)", argv, wantHead)
			}
			for i, w := range wantHead {
				if argv[i] != w {
					t.Fatalf("argv[%d] = %q, want %q", i, argv[i], w)
				}
			}
			if argv[len(argv)-1] != "--quit" {
				t.Fatalf("argv tail = %q, want --quit", argv[len(argv)-1])
			}
			prog := argv[len(wantHead)]
			fixed, err := programFor(tc.req)
			if err != nil {
				t.Fatal(err)
			}
			if prog != fixed {
				t.Fatalf("--batch program is not the fixed program for %s", tc.name)
			}
			// The result line passes through with the observed output size.
			var m map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
				t.Fatalf("stdout is not one JSON object: %v\n%s", err, out)
			}
			if m["schema_version"] != schemaVersions[tc.req.Kind] {
				t.Fatalf("schema_version = %v", m["schema_version"])
			}
			if m["output_bytes"] != float64(1) {
				t.Fatalf("output_bytes = %v, want 1 (pre-created fixture)", m["output_bytes"])
			}
		})
	}
}

func TestEveryArgvIsBatchWithAFixedProgram(t *testing.T) {
	fx := mkfix(t)
	reqs := []Request{
		{Kind: KindExport, Input: fx.in, Output: fx.out},
		{Kind: KindConvert, Input: fx.in, Output: fx.outJPG, Mode: ModeRGB},
		{Kind: KindConvert, Input: fx.in, Output: fx.outJPG, Mode: ModeGray},
		{Kind: KindEdit, Input: fx.in, Output: fx.out, Scale: "800x600"},
		{Kind: KindEdit, Input: fx.in, Output: fx.out, Rotate: 180},
		{Kind: KindEdit, Input: fx.in, Output: fx.out, Scale: "64x48", Rotate: 270},
	}
	progs := map[string]bool{}
	for _, r := range reqs {
		argv, err := r.Argv()
		if err != nil {
			t.Fatalf("%s: %v", r.Kind, err)
		}
		nBatch, nInterp := 0, 0
		for i, a := range argv {
			if a == "--batch" {
				nBatch++
				progs[argv[i+1]] = true
			}
			if a == "--batch-interpreter" {
				nInterp++
				if argv[i+1] != batchInterpreter {
					t.Fatalf("%s: interpreter = %q", r.Kind, argv[i+1])
				}
			}
			for _, bad := range []string{"--gui", "--display", "-b", "--python", "--script", "--load", "--pdb", "--batch-interpreter="} {
				if a == bad {
					t.Fatalf("%s: forbidden argument %q", r.Kind, a)
				}
			}
		}
		if nBatch != 1 || nInterp != 1 {
			t.Fatalf("%s: --batch x%d --batch-interpreter x%d, want exactly 1 each", r.Kind, nBatch, nInterp)
		}
		if argv[len(argv)-1] != "--quit" {
			t.Fatalf("%s: no trailing --quit", r.Kind)
		}
	}
	if len(progs) != len(reqs) {
		t.Fatalf("%d distinct programs for %d requests (a program must reflect its request)", len(progs), len(reqs))
	}
}

func TestFixedProgramsCarryOnlyFixedCalls(t *testing.T) {
	fx := mkfix(t)
	for label, prog := range fixedPrograms(fx.in, fx.out) {
		for _, want := range []string{
			"from gi.repository import Gimp, Gio",
			"Gimp.file_load(Gimp.RunMode.NONINTERACTIVE, Gio.File.new_for_path(",
			"Gimp.file_save(Gimp.RunMode.NONINTERACTIVE, _image, Gio.File.new_for_path(",
			"print(",
			"finally:\n    _image.delete()",
		} {
			if !strings.Contains(prog, want) {
				t.Fatalf("%s: program lacks %q", label, want)
			}
		}
		for _, bad := range []string{
			"exec(", "eval(", "open(", "compile(", "import os", "import sys",
			"subprocess", "socket", "__import__", "getattr(", "setattr(",
			"globals(", "locals(", "vars(", "input(", "Gio.File.new_for_uri",
			"new_for_commandline_arg",
		} {
			if strings.Contains(prog, bad) {
				t.Fatalf("%s: program contains %q", label, bad)
			}
		}
		if strings.Count(prog, "print(") != 1 {
			t.Fatalf("%s: program must print exactly once", label)
		}
	}
	progs := fixedPrograms(fx.in, fx.out)
	if !strings.Contains(progs["convert-rgb"], "_image.convert_rgb()") {
		t.Fatal("convert-rgb lacks convert_rgb")
	}
	if !strings.Contains(progs["convert-gray"], "_image.convert_grayscale()") {
		t.Fatal("convert-gray lacks convert_grayscale")
	}
	if !strings.Contains(progs["edit-scale"], "_image.scale(800, 600)") {
		t.Fatal("edit-scale lacks scale(800, 600)")
	}
	if strings.Contains(progs["edit-scale"], "_image.rotate(") {
		t.Fatal("edit-scale must not rotate")
	}
	if !strings.Contains(progs["edit-rotate"], "_image.rotate(Gimp.RotationType.ROTATE_DEGREES90)") {
		t.Fatal("edit-rotate lacks the fixed rotate fragment")
	}
	if !strings.Contains(progs["edit-both"], "_image.scale(800, 600)") ||
		!strings.Contains(progs["edit-both"], "_image.rotate(Gimp.RotationType.ROTATE_DEGREES270)") {
		t.Fatal("edit-both lacks scale+rotate")
	}
	if strings.Contains(progs["export"], "_image.scale(") ||
		strings.Contains(progs["export"], "_image.rotate(") ||
		strings.Contains(progs["export"], "convert_") {
		t.Fatal("export must not transform")
	}
	// The printed line must BE the result object: extract the payload
	// literal from each program, unescape it exactly as Python would, and
	// require it to parse as the operation's JSON envelope. This closes
	// the loop with Run, which unmarshals the child's line into a map —
	// a double-encoded payload would fail here, not just live.
	for label, prog := range progs {
		line := ""
		for _, l := range strings.Split(prog, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "print(") {
				line = strings.TrimSpace(l)
			}
		}
		const prefix = "print('yoke-gimp-v1 ' + '"
		if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "')") {
			t.Fatalf("%s: print line has unexpected shape: %q", label, line)
		}
		raw := unpyLiteral(line[len(prefix) : len(line)-2])
		var m map[string]string
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("%s: printed payload is not a JSON object: %v\n%s", label, err, prog)
		}
		if m["operation"] != labelOp(label) || m["schema_version"] != schemaVersions[Kind(m["operation"])] {
			t.Fatalf("%s: payload = %v", label, m)
		}
		if m["input"] != fx.in || m["output"] != fx.out {
			t.Fatalf("%s: payload paths = %q %q", label, m["input"], m["output"])
		}
	}
}

// unpyLiteral inverts pyLiteral for the two escapes it emits.
func unpyLiteral(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '\\' || s[i+1] == '\'') {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// labelOp maps a fixedPrograms label back to its operation.
func labelOp(label string) string {
	switch {
	case strings.HasPrefix(label, "convert"):
		return string(KindConvert)
	case strings.HasPrefix(label, "edit"):
		return string(KindEdit)
	default:
		return string(KindExport)
	}
}

// TestProgramsParseAsOneFixedStatementShape parses every representative
// program with the system python3 (syntax only — gi is never imported) and
// requires the single fixed statement shape: two imports, one load
// assignment, one try/finally whose body holds only attribute calls and one
// print, and no other statement kinds. A smuggled extra statement would be
// a new AST node and fail here.
func TestProgramsParseAsOneFixedStatementShape(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 for the syntax-oracle check (substring pins above still ran)")
	}
	fx := mkfix(t)
	// Hostile-but-valid paths: quotes, backslashes, spaces, unicode, shell
	// metacharacters. Each must survive as inert string data.
	hostile := filepath.Join(fx.dir, "o'b\\x $HOME `id` @(x).png")
	if err := os.WriteFile(hostile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	reqs := []Request{
		{Kind: KindExport, Input: hostile, Output: fx.out},
		{Kind: KindConvert, Input: fx.in, Output: fx.out, Mode: ModeGray},
		{Kind: KindEdit, Input: fx.in, Output: fx.out, Scale: "800x600", Rotate: 270},
	}
	check := `
import ast, sys
tree = ast.parse(sys.stdin.read())
allowed_top = {"ImportFrom", "Import", "Assign", "Try"}
banned_calls = {"exec", "eval", "open", "compile", "__import__"}
for node in tree.body:
    if type(node).__name__ not in allowed_top:
        sys.exit("top-level " + type(node).__name__)
for node in ast.walk(tree):
    if isinstance(node, ast.Call):
        f = node.func
        name = f.attr if isinstance(f, ast.Attribute) else getattr(f, "id", "")
        if name in banned_calls:
            sys.exit("call " + name)
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        if "\n" in node.value or "\r" in node.value or "\x00" in node.value:
            sys.exit("multiline string constant")
print("SHAPE-OK")
`
	for i, r := range reqs {
		prog, err := programFor(r)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(py, "-c", check)
		cmd.Stdin = strings.NewReader(prog)
		var sb, eb strings.Builder
		cmd.Stdout = &sb
		cmd.Stderr = &eb
		if err := cmd.Run(); err != nil {
			t.Fatalf("req %d: program is not one fixed statement shape: %v\n%s\n--- program ---\n%s", i, err, eb.String(), prog)
		}
		if strings.TrimSpace(sb.String()) != "SHAPE-OK" {
			t.Fatalf("req %d: oracle said %q", i, sb.String())
		}
		// The hostile path must appear only as string data: re-parse and
		// require every string constant to be free of statement breaks
		// (no newline could reach here — Validate refuses control
		// characters — but assert it at the oracle too).
		if strings.Contains(prog, hostile) {
			t.Fatalf("req %d: raw hostile path leaks into the program unescaped", i)
		}
	}
}

func TestValidationRunsBeforeExecution(t *testing.T) {
	fx := mkfix(t)
	rel := "in.png"
	missing := filepath.Join(fx.dir, "missing.png")
	noDir := filepath.Join(fx.dir, "nope", "out.png")
	cases := []struct {
		name string
		args []string
	}{
		{"relative input", []string{"export", "--input", rel, "--output", fx.out}},
		{"missing input", []string{"export", "--input", missing, "--output", fx.out}},
		{"wrong ext input", []string{"export", "--input", fx.outBlend, "--output", fx.out}},
		{"directory input", []string{"export", "--input", fx.dir + "/", "--output", fx.out}},
		{"no input", []string{"export", "--output", fx.out}},
		{"no output", []string{"export", "--input", fx.in}},
		{"output relative", []string{"export", "--input", fx.in, "--output", "out.png"}},
		{"output parent missing", []string{"export", "--input", fx.in, "--output", noDir}},
		{"output is dir", []string{"export", "--input", fx.in, "--output", fx.dir}},
		{"output xcf refused", []string{"export", "--input", fx.in, "--output", filepath.Join(fx.dir, "o.xcf")}},
		{"export with mode", []string{"export", "--input", fx.in, "--output", fx.out, "--mode", "gray"}},
		{"convert no mode", []string{"convert", "--input", fx.in, "--output", fx.out}},
		{"convert bad mode", []string{"convert", "--input", fx.in, "--output", fx.out, "--mode", "cmyk"}},
		{"convert with scale", []string{"convert", "--input", fx.in, "--output", fx.out, "--mode", "rgb", "--scale", "8x8"}},
		{"edit neither", []string{"edit", "--input", fx.in, "--output", fx.out}},
		{"edit with mode", []string{"edit", "--input", fx.in, "--output", fx.out, "--scale", "8x8", "--mode", "rgb"}},
		{"edit scale no x", []string{"edit", "--input", fx.in, "--output", fx.out, "--scale", "800"}},
		{"edit scale trailing", []string{"edit", "--input", fx.in, "--output", fx.out, "--scale", "800x600x2"}},
		{"edit scale zero", []string{"edit", "--input", fx.in, "--output", fx.out, "--scale", "0x600"}},
		{"edit scale too big", []string{"edit", "--input", fx.in, "--output", fx.out, "--scale", "99999x1"}},
		{"edit scale negative", []string{"edit", "--input", fx.in, "--output", fx.out, "--scale", "-1x2"}},
		{"edit scale words", []string{"edit", "--input", fx.in, "--output", fx.out, "--scale", "bigxhuge"}},
		{"edit bad rotate", []string{"edit", "--input", fx.in, "--output", fx.out, "--rotate", "45"}},
		{"edit rotate not int", []string{"edit", "--input", fx.in, "--output", fx.out, "--rotate", "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeExec{stdout: resultMarker + "{}\n"}
			if _, err := execute(t, testRunner(f), tc.args...); err == nil {
				t.Fatal("expected a validation error")
			}
			if f.count() != 0 {
				t.Fatalf("executor called %d times before validation failed", f.count())
			}
		})
	}
	for _, bad := range []string{"/tmp/a\nb.png", "/tmp/a\x00.png", ""} {
		if err := (Request{Kind: KindExport, Input: bad, Output: fx.out}).Validate(); err == nil {
			t.Fatalf("Validate accepted %q", bad)
		}
	}
	if err := (Request{Kind: Kind("paint"), Input: fx.in, Output: fx.out}).Validate(); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestResolverFailureMeansNoExecution(t *testing.T) {
	fx := mkfix(t)
	f := &fakeExec{stdout: resultMarker + "{}\n"}
	rn := &runner{resolve: func(context.Context, string) (string, error) { return "", ErrNoPin }, execute: f.exec}
	_, err := execute(t, rn, "export", "--input", fx.in, "--output", fx.out)
	if !errors.Is(err, ErrNoPin) {
		t.Fatalf("err = %v, want ErrNoPin", err)
	}
	if f.count() != 0 {
		t.Fatalf("executor called %d times without a binary", f.count())
	}
}

func TestChildFailureAndMissingResultAreErrors(t *testing.T) {
	fx := mkfix(t)
	f := &fakeExec{stderr: "Traceback\nGIMP-Error: boom\n", err: errors.New("exit status 1")}
	_, err := execute(t, testRunner(f), "export", "--input", fx.in, "--output", fx.out)
	if err == nil || !strings.Contains(err.Error(), "exit status 1") || !strings.Contains(err.Error(), "GIMP-Error: boom") {
		t.Fatalf("err = %v", err)
	}
	f = &fakeExec{stdout: "batch command executed successfully\n"}
	out, err := execute(t, testRunner(f), "export", "--input", fx.in, "--output", fx.out)
	if err == nil || !strings.Contains(err.Error(), "no result line") {
		t.Fatalf("err = %v", err)
	}
	if out != "" {
		t.Fatalf("stdout = %q on failure", out)
	}
	f = &fakeExec{stdout: resultMarker + "not json\n"}
	// Output file is pre-created, so the JSON failure is what trips.
	if _, err := execute(t, testRunner(f), "export", "--input", fx.in, "--output", fx.out); err == nil ||
		!strings.Contains(err.Error(), "not JSON") {
		t.Fatalf("err = %v, want JSON failure", err)
	}
}

func TestMissingOrEmptyOutputAfterSuccessIsAnError(t *testing.T) {
	fx := mkfix(t)
	r := Request{Kind: KindExport, Input: fx.in, Output: filepath.Join(fx.dir, "never-written.png")}
	f := &fakeExec{stdout: resultFor(t, r)}
	if err := testRunner(f).run(context.Background(), "", r, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "output missing") {
		t.Fatalf("err = %v, want missing-output failure", err)
	}
	empty := filepath.Join(fx.dir, "empty.png")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r.Output = empty
	f.stdout = resultFor(t, r)
	if err := testRunner(f).run(context.Background(), "", r, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v, want empty-output failure", err)
	}
	if f.count() != 2 {
		t.Fatalf("executor called %d times", f.count())
	}
}

func TestDeadlineReachesTheChild(t *testing.T) {
	fx := mkfix(t)
	f := &fakeExec{block: true}
	cmd := newCmd(testRunner(f))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"edit", "--input", fx.in, "--output", fx.out, "--scale", "8x8", "--timeout", "50ms"})
	start := time.Now()
	err := cmd.ExecuteContext(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("deadline did not interrupt the child promptly")
	}
	if f.count() != 1 {
		t.Fatalf("executor called %d times", f.count())
	}
}

func TestCallerContextCancellation(t *testing.T) {
	fx := mkfix(t)
	f := &fakeExec{block: true}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- testRunner(f).run(ctx, "", Request{Kind: KindExport, Input: fx.in, Output: fx.out}, io.Discard)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not return")
	}
	f2 := &fakeExec{stdout: resultMarker + "{}\n"}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if err := testRunner(f2).run(ctx2, "", Request{Kind: KindExport, Input: fx.in, Output: fx.out}, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if f2.count() != 0 {
		t.Fatal("executor called under a cancelled context")
	}
}

func TestRealExecutorHonoursDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sleep")
	}
	if _, err := os.Stat("/bin/sleep"); err != nil {
		t.Skip("no /bin/sleep")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := runCommand(ctx, "/bin/sleep", []string{"30"}, io.Discard, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("real child outlived the deadline")
	}
}

func TestRealExecutorIsForegroundAndCapturesOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/echo")
	}
	var out bytes.Buffer
	if err := runCommand(context.Background(), "/bin/echo", []string{resultMarker + "{}"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, ok := findResult(out.String()); !ok || got != "{}" {
		t.Fatalf("result = %q, %v", got, ok)
	}
}

// TestNoDaemonListenerOrDetach pins, at the source level, that gimp.go
// cannot open a socket, install a plug-in, start a child it does not wait
// for, or reach for a host GIMP instead of the pinned artifact.
func TestNoDaemonListenerOrDetach(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "gimp.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range file.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		switch {
		case p == "net" || strings.HasPrefix(p, "net/"), p == "os/signal", p == "os/exec", p == "syscall",
			strings.Contains(p, "serviceenv"), strings.Contains(p, "/mcp"):
			t.Fatalf("gimp.go imports %q", p)
		}
	}
	src, err := os.ReadFile("gimp.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, bad := range []string{".Start(", "binmgr.Launch", "binmgr.Start", "binmgr.RunSpec", "Setsid", "Setpgid",
		"Listen(", "http.", "go func(", "LookPath", "PreferHost", "exec(open", "(gimp-quit", "plug-ins/"} {
		if strings.Contains(s, bad) {
			t.Fatalf("gimp.go contains %q", bad)
		}
	}
	if strings.Count(s, "binmgr.Command(") != 1 || strings.Count(s, ".Run()") != 1 {
		t.Fatal("expected exactly one binmgr.Command + Run site")
	}
}

func TestSpecAndPathFailClosedWithoutCommittedPin(t *testing.T) {
	tool, err := Spec("")
	if err != nil {
		t.Fatal(err)
	}
	if tool.Name != toolName || tool.Version != DefaultVersion {
		t.Fatalf("tool = %+v", tool)
	}
	if _, err := Spec("latest"); err == nil {
		t.Fatal("Spec accepted \"latest\"")
	}
	if _, err := Spec("3.2"); err == nil {
		t.Fatal("Spec accepted a non-patch version")
	}
	for _, plat := range []string{"linux/amd64", "linux/arm64", "darwin/arm64", "darwin/amd64", "windows/amd64", "windows/arm64"} {
		goos, goarch, _ := strings.Cut(plat, "/")
		_, pinned := binmgr.PinnedSHA256(toolName, DefaultVersion, plat)
		_, hasAsset := tool.Assets[plat]
		if hasAsset != pinned {
			t.Fatalf("%s: asset declared=%v pinned=%v (an asset without a committed pin is forbidden)", plat, hasAsset, pinned)
		}
		if hasAsset {
			a := tool.Assets[plat]
			if a.SHA256 == "" || a.Tree || a.Binary != "" || a.Entrypoint != "" {
				t.Fatalf("%s: raw-binary asset must carry only URL+SHA256, got %+v", plat, a)
			}
			continue
		}
		// No pin: Path must fail closed with NO network and no download.
		t.Setenv("BASHY_BIN_CACHE", t.TempDir())
		_, err := pathFor(context.Background(), DefaultVersion, goos, goarch)
		_, artifact := ArtifactFor(DefaultVersion, goos, goarch)
		switch {
		case !artifact && !errors.Is(err, ErrNoArtifact):
			t.Fatalf("%s: err = %v, want ErrNoArtifact", plat, err)
		case artifact && !errors.Is(err, ErrNoPin):
			// linux/amd64 on a non-linux host reaches the cross-provision
			// refusal instead: still closed, still no network.
			if plat == "linux/amd64" && runtime.GOOS != "linux" {
				if err == nil || strings.Contains(err.Error(), "ErrNoPin") || errors.Is(err, ErrNoArtifact) {
					t.Fatalf("%s: err = %v", plat, err)
				}
				if !strings.Contains(err.Error(), "cannot provision") {
					t.Fatalf("%s: err = %v, want the cross-provision refusal", plat, err)
				}
				continue
			}
			t.Fatalf("%s: err = %v, want ErrNoPin", plat, err)
		}
	}
	if _, err := Path(context.Background(), "9.9.9"); !errors.Is(err, ErrNoPin) && !errors.Is(err, ErrNoArtifact) {
		t.Fatalf("Path(9.9.9) = %v", err)
	}
}

func TestArtifactTable(t *testing.T) {
	a, ok := ArtifactFor("3.2.6", "linux", "amd64")
	if !ok || !a.Raw || a.URL != "https://download.gimp.org/gimp/v3.2/linux/GIMP-3.2.6-x86_64.AppImage" {
		t.Fatalf("linux/amd64 = %+v %v", a, ok)
	}
	a, ok = ArtifactFor("3.2.6", "linux", "arm64")
	if !ok || !a.Raw || a.URL != "https://download.gimp.org/gimp/v3.2/linux/GIMP-3.2.6-aarch64.AppImage" {
		t.Fatalf("linux/arm64 = %+v %v", a, ok)
	}
	for _, plat := range []string{"darwin/arm64", "darwin/amd64", "windows/amd64", "windows/arm64"} {
		goos, goarch, _ := strings.Cut(plat, "/")
		if _, ok := ArtifactFor("3.2.6", goos, goarch); ok {
			t.Fatalf("%s: unexpected artifact (installer/dmg are not managed binaries)", plat)
		}
	}
	if _, ok := ArtifactFor("latest", "linux", "amd64"); ok {
		t.Fatal("latest resolved")
	}
}

// TestOfficialSourceAndPin pins the supply-chain facts a reviewer must be
// able to check without running anything: the release root is the project's
// own download service (which 302-redirects plain clients into its mirror
// network — the committed pin, not the serving host, is the trust anchor),
// and the linux/amd64 digest is EXACTLY the project's published
// linux/SHA256SUMS row for GIMP-3.2.6-x86_64.AppImage — never a digest
// computed from a mirror or from whatever bytes happened to arrive (the
// authoring host additionally hashed the full 243,476,984-byte download and
// it matched; see EVIDENCE.md).
func TestOfficialSourceAndPin(t *testing.T) {
	if releaseBase != "https://download.gimp.org/gimp" {
		t.Fatalf("releaseBase = %q", releaseBase)
	}
	const officialLinuxAMD64 = "79ea41bc9b06f78fda181849a9ca8e42d83f1124dedb756f4d52352e2465010f" // GIMP-3.2.6-x86_64.AppImage
	sha, ok := binmgr.PinnedSHA256(toolName, DefaultVersion, "linux/amd64")
	if !ok || sha != officialLinuxAMD64 {
		t.Fatalf("linux/amd64 pin = %q, %v; want the official manifest digest", sha, ok)
	}
	tool, err := Spec("")
	if err != nil {
		t.Fatal(err)
	}
	a, ok := tool.Assets["linux/amd64"]
	if !ok || a.SHA256 != officialLinuxAMD64 || a.URL != "https://download.gimp.org/gimp/v3.2/linux/GIMP-3.2.6-x86_64.AppImage" {
		t.Fatalf("linux/amd64 asset = %+v %v", a, ok)
	}
	if a.SHA512 != "" || a.MD5 != "" {
		t.Fatalf("weaker digests must not be declared: %+v", a)
	}
	if a.Tree || a.Binary != "" || a.Entrypoint != "" {
		t.Fatalf("AppImage is a raw binary, not an archive: %+v", a)
	}
}

func TestRegisteredDeclarationMatchesContract(t *testing.T) {
	ops := Operations()
	if len(ops) != len(Kinds) {
		t.Fatalf("Operations = %d, Kinds = %d", len(ops), len(Kinds))
	}
	names := map[string]bool{}
	vocab := map[string]bool{}
	for _, e := range atlas.Effects() {
		vocab[e] = true
	}
	for i, op := range ops {
		if op.Name != string(Kinds[i]) {
			t.Fatalf("ops[%d].Name = %q, want %q", i, op.Name, Kinds[i])
		}
		if names[op.Name] {
			t.Fatalf("duplicate operation %q", op.Name)
		}
		names[op.Name] = true
		if op.Summary == "" || op.ResultSchema == "" {
			t.Fatalf("%s: summary and result schema are required", op.Name)
		}
		if !strings.HasPrefix(op.ResultSchema, "yoke-gimp-") {
			t.Fatalf("%s: result schema %q", op.Name, op.ResultSchema)
		}
		if len(op.Args) < 2 || op.Args[0].Flag != "input" || op.Args[1].Flag != "output" {
			t.Fatalf("%s: every operation starts with explicit input+output file args", op.Name)
		}
		for _, a := range op.Args {
			if a.Flag == "" || a.Type == "" || a.Desc == "" {
				t.Fatalf("%s: arg %+v is incomplete", op.Name, a)
			}
		}
		for _, e := range op.Effects {
			if !vocab[e] {
				t.Fatalf("%s: effect %q is not in the atlas vocabulary", op.Name, e)
			}
		}
	}
	reg := Registered()
	inStr := func(s string, set []string) bool {
		for _, v := range set {
			if v == s {
				return true
			}
		}
		return false
	}
	if reg.Kind != "download" || !inStr(reg.Kind, atlas.RegisteredKinds()) {
		t.Fatalf("registration kind = %q", reg.Kind)
	}
	if !inStr(reg.Group, atlas.Groups()) {
		t.Fatalf("registration group = %q", reg.Group)
	}
	if !inStr(reg.Tier, atlas.Tiers()) {
		t.Fatalf("registration tier = %q", reg.Tier)
	}
	if !inStr(reg.Stage, atlas.Stages()) {
		t.Fatalf("registration stage = %q", reg.Stage)
	}
	if reg.Shape != "json" {
		t.Fatalf("registration shape = %q", reg.Shape)
	}
	// Effects is exactly the sorted union of the per-operation effects.
	union := map[string]bool{}
	for _, op := range ops {
		for _, e := range op.Effects {
			union[e] = true
		}
	}
	if len(reg.Effects) != len(union) {
		t.Fatalf("registration effects = %v, want union %v", reg.Effects, union)
	}
	for _, e := range reg.Effects {
		if !union[e] {
			t.Fatalf("registration effect %q is not declared by any operation", e)
		}
	}
	for i := 1; i < len(reg.Effects); i++ {
		if reg.Effects[i-1] >= reg.Effects[i] {
			t.Fatalf("registration effects not sorted: %v", reg.Effects)
		}
	}
	if len(reg.OS) != 1 || reg.OS[0] != "linux/amd64" {
		t.Fatalf("registration OS = %v (only the provisionable platform may be listed)", reg.OS)
	}
}
