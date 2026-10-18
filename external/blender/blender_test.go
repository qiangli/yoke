package blender

import (
	"bytes"
	"context"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

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

const fakeBin = "/fake/cache/blender/4.2.3/blender-4.2.3-linux-x64/blender"

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
	dir, blend, obj, png, outBlend, outPNG string
}

func mkfix(t *testing.T) fixtures {
	t.Helper()
	dir := t.TempDir()
	fx := fixtures{
		dir:      dir,
		blend:    filepath.Join(dir, "scene.blend"),
		obj:      filepath.Join(dir, "cube.obj"),
		png:      filepath.Join(dir, "frame.png"),
		outBlend: filepath.Join(dir, "out.blend"),
		outPNG:   filepath.Join(dir, "render.png"),
	}
	for _, p := range []string{fx.blend, fx.obj, fx.png} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return fx
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

func equalArgv(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("argv length %d, want %d\n got: %q\nwant: %q", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSubcommandsAreExactlyTheFourTypedOperations(t *testing.T) {
	cmd := NewCmd()
	var names []string
	for _, c := range cmd.Commands() {
		names = append(names, c.Name())
	}
	want := []string{"asset-import", "output-inspect", "render", "scene-inspect"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("subcommands = %v, want %v", names, want)
	}
	for _, forbidden := range []string{"run", "exec", "python", "serve", "daemon", "mcp", "gui", "shell", "repl"} {
		for _, n := range names {
			if n == forbidden {
				t.Fatalf("forbidden subcommand %q registered", n)
			}
		}
	}
	if len(Kinds) != 4 {
		t.Fatalf("Kinds = %v", Kinds)
	}
}

func TestNoFreeFormEscapeFlags(t *testing.T) {
	forbidden := map[string]bool{"python": true, "python-expr": true, "expr": true, "script": true, "args": true,
		"arg": true, "raw": true, "passthrough": true, "extra": true, "command": true, "exec": true, "addon": true}
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
	// Blender argument to sneak in.
	fx := mkfix(t)
	f := &fakeExec{}
	if _, err := execute(t, testRunner(f), "scene-inspect", "--input", fx.blend, "--python-expr", "print(1)"); err == nil {
		t.Fatal("unknown flag --python-expr accepted")
	}
	if _, err := execute(t, testRunner(f), "scene-inspect", "--input", fx.blend, "extra.blend"); err == nil {
		t.Fatal("positional argument accepted")
	}
	if _, err := execute(t, testRunner(f), "render", "--input", fx.blend, "--output", fx.outPNG, "--", "--python-console"); err == nil {
		t.Fatal("trailing pass-through accepted")
	}
	if f.count() != 0 {
		t.Fatalf("executor called %d times on rejected input", f.count())
	}
}

func TestSceneInspectArgv(t *testing.T) {
	fx := mkfix(t)
	f := &fakeExec{stdout: "Blender 4.2.3\nRead blend: " + fx.blend + "\n" + resultMarker + `{"scene":"Scene"}` + "\n\nBlender quit\n"}
	out, err := execute(t, testRunner(f), "scene-inspect", "--input", fx.blend)
	if err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 {
		t.Fatalf("executor called %d times, want exactly 1", f.count())
	}
	if f.calls[0].bin != fakeBin {
		t.Fatalf("bin = %q", f.calls[0].bin)
	}
	equalArgv(t, f.calls[0].args, []string{
		"--background", "--factory-startup", fx.blend,
		"--python-exit-code", "3", "--python-expr", sceneInspectScript, "--",
	})
	if out != `{"scene":"Scene"}`+"\n" {
		t.Fatalf("stdout = %q (noise must be filtered)", out)
	}
}

func TestAssetImportArgv(t *testing.T) {
	fx := mkfix(t)
	f := &fakeExec{stdout: resultMarker + "{}\n"}
	if _, err := execute(t, testRunner(f), "asset-import", "--asset", fx.obj, "--output", fx.outBlend); err != nil {
		t.Fatal(err)
	}
	equalArgv(t, f.calls[0].args, []string{
		"--background", "--factory-startup",
		"--python-exit-code", "3", "--python-expr", assetImportScript, "--",
		fx.obj, fx.outBlend, "1",
	})
	f = &fakeExec{stdout: resultMarker + "{}\n"}
	if _, err := execute(t, testRunner(f), "asset-import", "--asset", fx.obj, "--output", fx.outBlend, "--input", fx.blend); err != nil {
		t.Fatal(err)
	}
	equalArgv(t, f.calls[0].args, []string{
		"--background", "--factory-startup", fx.blend,
		"--python-exit-code", "3", "--python-expr", assetImportScript, "--",
		fx.obj, fx.outBlend, "0",
	})
	if f.count() != 1 {
		t.Fatalf("executor called %d times", f.count())
	}
}

func TestRenderArgv(t *testing.T) {
	fx := mkfix(t)
	f := &fakeExec{stdout: resultMarker + "{}\n"}
	if _, err := execute(t, testRunner(f), "render", "--input", fx.blend, "--output", fx.outPNG, "--frame", "12"); err != nil {
		t.Fatal(err)
	}
	equalArgv(t, f.calls[0].args, []string{
		"--background", "--factory-startup", fx.blend,
		"--python-exit-code", "3", "--python-expr", renderScript, "--",
		fx.outPNG, "12",
	})
	f = &fakeExec{stdout: resultMarker + "{}\n"}
	if _, err := execute(t, testRunner(f), "render", "--input", fx.blend, "--output", fx.outPNG); err != nil {
		t.Fatal(err)
	}
	if got := f.calls[0].args[len(f.calls[0].args)-1]; got != "1" {
		t.Fatalf("default frame = %q, want 1", got)
	}
}

func TestOutputInspectArgv(t *testing.T) {
	fx := mkfix(t)
	f := &fakeExec{stdout: resultMarker + `{"width":1920}` + "\n"}
	out, err := execute(t, testRunner(f), "output-inspect", "--input", fx.png)
	if err != nil {
		t.Fatal(err)
	}
	equalArgv(t, f.calls[0].args, []string{
		"--background", "--factory-startup",
		"--python-exit-code", "3", "--python-expr", outputInspectScript, "--",
		fx.png,
	})
	if out != `{"width":1920}`+"\n" {
		t.Fatalf("stdout = %q", out)
	}
}

func TestEveryArgvIsBackgroundWithAFixedScript(t *testing.T) {
	fx := mkfix(t)
	reqs := []Request{
		{Kind: KindSceneInspect, Input: fx.blend},
		{Kind: KindAssetImport, Asset: fx.obj, Output: fx.outBlend},
		{Kind: KindAssetImport, Asset: fx.obj, Output: fx.outBlend, Input: fx.blend},
		{Kind: KindRender, Input: fx.blend, Output: fx.outPNG, Frame: 3},
		{Kind: KindOutputInspect, Input: fx.png},
	}
	scripts := Scripts()
	for _, r := range reqs {
		argv, err := r.Argv()
		if err != nil {
			t.Fatalf("%s: %v", r.Kind, err)
		}
		if argv[0] != "--background" {
			t.Fatalf("%s: argv[0] = %q", r.Kind, argv[0])
		}
		n := 0
		for i, a := range argv {
			if a == "--python-expr" {
				n++
				if argv[i+1] != scripts[r.Kind] {
					t.Fatalf("%s: --python-expr carries a non-fixed script", r.Kind)
				}
			}
			for _, bad := range []string{"--python", "--python-console", "--python-use-system-env", "--addons", "--command", "--env-system-scripts", "--window-geometry"} {
				if a == bad {
					t.Fatalf("%s: forbidden argument %q", r.Kind, a)
				}
			}
		}
		if n != 1 {
			t.Fatalf("%s: %d --python-expr occurrences, want 1", r.Kind, n)
		}
		// Paths never appear before "--" except the single .blend positional.
		sep := -1
		for i, a := range argv {
			if a == "--" {
				sep = i
			}
		}
		if sep < 0 {
			t.Fatalf("%s: no -- separator", r.Kind)
		}
	}
}

func TestValidationRunsBeforeExecution(t *testing.T) {
	fx := mkfix(t)
	rel := "scene.blend"
	missing := filepath.Join(fx.dir, "missing.blend")
	noDir := filepath.Join(fx.dir, "nope", "out.png")
	cases := []struct {
		name string
		args []string
	}{
		{"relative input", []string{"scene-inspect", "--input", rel}},
		{"missing input", []string{"scene-inspect", "--input", missing}},
		{"wrong ext input", []string{"scene-inspect", "--input", fx.obj}},
		{"directory input", []string{"scene-inspect", "--input", fx.dir + "/"}},
		{"no input", []string{"scene-inspect"}},
		{"asset wrong ext", []string{"asset-import", "--asset", fx.blend, "--output", fx.outBlend}},
		{"asset relative", []string{"asset-import", "--asset", "cube.obj", "--output", fx.outBlend}},
		{"asset output wrong ext", []string{"asset-import", "--asset", fx.obj, "--output", fx.outPNG}},
		{"asset output parent missing", []string{"asset-import", "--asset", fx.obj, "--output", filepath.Join(fx.dir, "nope", "o.blend")}},
		{"asset output equals input", []string{"asset-import", "--asset", fx.obj, "--output", fx.blend, "--input", fx.blend}},
		{"render output relative", []string{"render", "--input", fx.blend, "--output", "render.png"}},
		{"render output not png", []string{"render", "--input", fx.blend, "--output", filepath.Join(fx.dir, "r.exr")}},
		{"render output parent missing", []string{"render", "--input", fx.blend, "--output", noDir}},
		{"render negative frame", []string{"render", "--input", fx.blend, "--output", fx.outPNG, "--frame", "-1"}},
		{"render frame not int", []string{"render", "--input", fx.blend, "--output", fx.outPNG, "--frame", "x"}},
		{"output-inspect wrong ext", []string{"output-inspect", "--input", fx.blend}},
		{"output-inspect missing", []string{"output-inspect", "--input", filepath.Join(fx.dir, "no.png")}},
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
	// Control characters and option-looking values are refused at the
	// Request layer even though they are absolute.
	for _, bad := range []string{"/tmp/a\nb.blend", "/tmp/a\x00.blend", ""} {
		if err := (Request{Kind: KindSceneInspect, Input: bad}).Validate(); err == nil {
			t.Fatalf("Validate accepted %q", bad)
		}
	}
	if err := (Request{Kind: Kind("eval"), Input: fx.blend}).Validate(); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestResolverFailureMeansNoExecution(t *testing.T) {
	fx := mkfix(t)
	f := &fakeExec{stdout: resultMarker + "{}\n"}
	rn := &runner{resolve: func(context.Context, string) (string, error) { return "", ErrNoPin }, execute: f.exec}
	_, err := execute(t, rn, "scene-inspect", "--input", fx.blend)
	if !errors.Is(err, ErrNoPin) {
		t.Fatalf("err = %v, want ErrNoPin", err)
	}
	if f.count() != 0 {
		t.Fatalf("executor called %d times without a binary", f.count())
	}
}

func TestChildFailureAndMissingResultAreErrors(t *testing.T) {
	fx := mkfix(t)
	f := &fakeExec{stderr: "Traceback\nRuntimeError: boom\n", err: errors.New("exit status 3")}
	_, err := execute(t, testRunner(f), "scene-inspect", "--input", fx.blend)
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "RuntimeError: boom") {
		t.Fatalf("err = %v", err)
	}
	f = &fakeExec{stdout: "Blender quit\n"}
	out, err := execute(t, testRunner(f), "scene-inspect", "--input", fx.blend)
	if err == nil || !strings.Contains(err.Error(), "no result line") {
		t.Fatalf("err = %v", err)
	}
	if out != "" {
		t.Fatalf("stdout = %q on failure", out)
	}
}

func TestDeadlineReachesTheChild(t *testing.T) {
	fx := mkfix(t)
	f := &fakeExec{block: true}
	cmd := newCmd(testRunner(f))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"render", "--input", fx.blend, "--output", fx.outPNG, "--timeout", "50ms"})
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
		done <- testRunner(f).run(ctx, "", Request{Kind: KindSceneInspect, Input: fx.blend}, io.Discard)
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
	// An already-cancelled context never reaches the executor.
	f2 := &fakeExec{stdout: resultMarker + "{}\n"}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if err := testRunner(f2).run(ctx2, "", Request{Kind: KindSceneInspect, Input: fx.blend}, io.Discard); !errors.Is(err, context.Canceled) {
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

// TestNoDaemonListenerOrDetach pins, at the source level, that blender.go
// cannot open a socket, install signal handlers, or start a child it does
// not wait for.
func TestNoDaemonListenerOrDetach(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "blender.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range file.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		switch {
		case p == "net" || strings.HasPrefix(p, "net/"), p == "os/signal", p == "os/exec", p == "syscall",
			strings.Contains(p, "serviceenv"), strings.Contains(p, "/mcp"):
			t.Fatalf("blender.go imports %q", p)
		}
	}
	src, err := os.ReadFile("blender.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, bad := range []string{".Start(", "binmgr.Launch", "binmgr.Start", "binmgr.RunSpec", "Setsid", "Setpgid", "Listen(", "http.", "go func("} {
		if strings.Contains(s, bad) {
			t.Fatalf("blender.go contains %q", bad)
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
	if _, err := Spec("4.2"); err == nil {
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
			if a.SHA256 == "" || !a.Tree || a.Entrypoint == "" {
				t.Fatalf("%s: asset %+v", plat, a)
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
			t.Fatalf("%s: err = %v, want ErrNoPin", plat, err)
		}
	}
	if _, err := Path(context.Background(), "9.9.9"); !errors.Is(err, ErrNoPin) && !errors.Is(err, ErrNoArtifact) {
		t.Fatalf("Path(9.9.9) = %v", err)
	}
}

func TestArtifactTable(t *testing.T) {
	a, ok := ArtifactFor("4.2.3", "linux", "amd64")
	if !ok || a.URL != "https://download.blender.org/release/Blender4.2/blender-4.2.3-linux-x64.tar.xz" || a.Entrypoint != "blender-4.2.3-linux-x64/blender" {
		t.Fatalf("linux/amd64 = %+v %v", a, ok)
	}
	a, ok = ArtifactFor("4.2.3", "windows", "amd64")
	if !ok || a.URL != "https://download.blender.org/release/Blender4.2/blender-4.2.3-windows-x64.zip" || a.Entrypoint != "blender-4.2.3-windows-x64/blender.exe" {
		t.Fatalf("windows/amd64 = %+v %v", a, ok)
	}
	for _, plat := range []string{"darwin/arm64", "darwin/amd64", "linux/arm64", "windows/arm64"} {
		goos, goarch, _ := strings.Cut(plat, "/")
		if _, ok := ArtifactFor("4.2.3", goos, goarch); ok {
			t.Fatalf("%s: unexpected artifact", plat)
		}
	}
	if _, ok := ArtifactFor("latest", "linux", "amd64"); ok {
		t.Fatal("latest resolved")
	}
}

func TestFixedScriptsPrintOneMarkedResult(t *testing.T) {
	for kind, script := range Scripts() {
		if strings.Count(script, `print("`+resultMarker) != 1 {
			t.Fatalf("%s: script must print exactly one marked result line", kind)
		}
		for _, bad := range []string{"exec(", "eval(", "subprocess", "socket", "os.system", "importlib", "__import__"} {
			if strings.Contains(script, bad) {
				t.Fatalf("%s: script contains %q", kind, bad)
			}
		}
	}
}
