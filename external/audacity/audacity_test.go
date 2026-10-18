package audacity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// fakePipe is the dependency-injected pipe transport: it records invocations
// and never touches a real named pipe.
type fakePipe struct {
	mu       sync.Mutex
	calls    [][]string
	response string
	err      error
	block    bool
}

func (f *fakePipe) pipe(ctx context.Context, cmds []string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), cmds...))
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if f.err != nil {
		return "", f.err
	}
	return f.response, nil
}

func (f *fakePipe) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

const fakeBin = "/fake/cache/audacity/3.7.3/audacity"

func fakeResolve(ctx context.Context, version string) (string, error) {
	if version != DefaultVersion {
		return "", errors.New("unexpected version " + version)
	}
	return fakeBin, nil
}

func testRunner(f *fakePipe) *runner {
	return &runner{resolve: fakeResolve, pipe: f.pipe}
}

type fixtures struct {
	dir, wav, mp3, aiff, aup3, outWav, outMp3 string
}

func mkfix(t *testing.T) fixtures {
	t.Helper()
	dir := t.TempDir()
	fx := fixtures{
		dir:    dir,
		wav:    filepath.Join(dir, "in.wav"),
		mp3:    filepath.Join(dir, "in.mp3"),
		aiff:   filepath.Join(dir, "in.aiff"),
		aup3:   filepath.Join(dir, "proj.aup3"),
		outWav: filepath.Join(dir, "out.wav"),
		outMp3: filepath.Join(dir, "out.mp3"),
	}
	for _, p := range []string{fx.wav, fx.mp3, fx.aiff, fx.aup3} {
		if err := os.WriteFile(p, []byte("RIFF"), 0o644); err != nil {
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

func TestSubcommandsAreExactlyTheFourTypedOperations(t *testing.T) {
	cmd := NewCmd()
	var names []string
	for _, c := range cmd.Commands() {
		names = append(names, c.Name())
	}
	// alphabetical due to cobra sorting.
	sortStrings(names)
	want := []string{"export", "info", "normalize", "trim"}
	sortStrings(want)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("subcommands = %v, want %v", names, want)
	}
	for _, forbidden := range []string{"run", "exec", "python", "serve", "daemon", "mcp", "gui", "shell", "repl", "bridge", "listen"} {
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

func sortStrings(s []string) {
	sort.Strings(s)
}

func TestNoFreeFormEscapeFlags(t *testing.T) {
	forbidden := map[string]bool{"python": true, "python-expr": true, "expr": true, "script": true, "args": true,
		"arg": true, "raw": true, "passthrough": true, "extra": true, "command": true, "exec": true, "addon": true, "pipe": true}
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
	fx := mkfix(t)
	f := &fakePipe{response: `{"ok":true}`}
	if _, err := execute(t, testRunner(f), "info", "--input", fx.wav, "--pipe", "evil"); err == nil {
		t.Fatal("unknown flag --pipe accepted")
	}
	if _, err := execute(t, testRunner(f), "info", "--input", fx.wav, "extra.wav"); err == nil {
		t.Fatal("positional argument accepted")
	}
	if f.count() != 0 {
		t.Fatalf("pipe called %d times on rejected input", f.count())
	}
}

func TestInfoPipeCommands(t *testing.T) {
	fx := mkfix(t)
	f := &fakePipe{response: "BatchCommand finished: OK"}
	out, err := execute(t, testRunner(f), "info", "--input", fx.wav)
	if err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 {
		t.Fatalf("pipe called %d times, want 1", f.count())
	}
	cmds := f.calls[0]
	if len(cmds) != 2 {
		t.Fatalf("info commands = %v", cmds)
	}
	if !strings.HasPrefix(cmds[0], "Import2: Filename=") {
		t.Fatalf("cmd0 = %q", cmds[0])
	}
	if cmds[1] != "GetInfo: Type=Tracks Format=JSON" {
		t.Fatalf("cmd1 = %q", cmds[1])
	}
	// Output is a single yoke-audacity-v1 JSON line.
	if !strings.HasPrefix(out, resultMarker) {
		t.Fatalf("output = %q", out)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(out), resultMarker)), &m); err != nil {
		t.Fatal(err)
	}
	if m["schema_version"] != "yoke-audacity-v1" || m["op"] != "info" {
		t.Fatalf("envelope = %v", m)
	}
}

func TestTrimPipeCommands(t *testing.T) {
	fx := mkfix(t)
	f := &fakePipe{response: "BatchCommand finished: OK"}
	if _, err := execute(t, testRunner(f), "trim", "--input", fx.wav, "--output", fx.outWav, "--start", "1.0", "--end", "5.0"); err != nil {
		t.Fatal(err)
	}
	cmds := f.calls[0]
	if len(cmds) != 4 {
		t.Fatalf("trim commands = %v", cmds)
	}
	if !strings.Contains(cmds[1], "Select: Start=") || !strings.Contains(cmds[2], "Trim:") {
		t.Fatalf("trim cmds = %v", cmds)
	}
	if !strings.HasPrefix(cmds[3], "Export2: Filename=") {
		t.Fatalf("cmd3 = %q", cmds[3])
	}
}

func TestNormalizePipeCommands(t *testing.T) {
	fx := mkfix(t)
	f := &fakePipe{response: "BatchCommand finished: OK"}
	if _, err := execute(t, testRunner(f), "normalize", "--input", fx.wav, "--output", fx.outWav, "--level", "-6"); err != nil {
		t.Fatal(err)
	}
	cmds := f.calls[0]
	if len(cmds) != 4 {
		t.Fatalf("normalize commands = %v", cmds)
	}
	if !strings.Contains(cmds[1], "SelectAll:") || !strings.Contains(cmds[2], "LoudnessNormalization:") {
		t.Fatalf("normalize cmds = %v", cmds)
	}
	// default level
	f = &fakePipe{response: "BatchCommand finished: OK"}
	if _, err := execute(t, testRunner(f), "normalize", "--input", fx.wav, "--output", fx.outWav); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.calls[0][2], "Level=-3.00") {
		t.Fatalf("default level not -3.00: %v", f.calls[0])
	}
}

func TestExportPipeCommands(t *testing.T) {
	fx := mkfix(t)
	f := &fakePipe{response: "BatchCommand finished: OK"}
	if _, err := execute(t, testRunner(f), "export", "--input", fx.aup3, "--output", fx.outMp3, "--format", "mp3"); err != nil {
		t.Fatal(err)
	}
	cmds := f.calls[0]
	if len(cmds) != 2 {
		t.Fatalf("export commands = %v", cmds)
	}
	if !strings.HasPrefix(cmds[0], "Import2:") || !strings.HasPrefix(cmds[1], "Export2:") {
		t.Fatalf("export cmds = %v", cmds)
	}
}

func TestPipeCommandsAreFixedAndQuoted(t *testing.T) {
	fx := mkfix(t)
	reqs := []Request{
		{Kind: KindInfo, Input: fx.wav},
		{Kind: KindTrim, Input: fx.wav, Output: fx.outWav, Start: 0, End: 2},
		{Kind: KindNormalize, Input: fx.wav, Output: fx.outWav, Level: -3, LevelSet: true},
		{Kind: KindExport, Input: fx.aup3, Output: fx.outMp3, Format: "mp3"},
	}
	for _, r := range reqs {
		cmds, err := r.PipeCommands()
		if err != nil {
			t.Fatalf("%s: %v", r.Kind, err)
		}
		for _, c := range cmds {
			if strings.Contains(c, "exec(") || strings.Contains(c, "eval(") || strings.Contains(c, "socket") || strings.Contains(c, "subprocess") {
				t.Fatalf("%s: command contains forbidden token %q", r.Kind, c)
			}
			// No raw user input beyond quoted Filename.
			if strings.Contains(c, "; rm ") {
				t.Fatalf("%s: injection in command %q", r.Kind, c)
			}
		}
	}
}

func TestValidationRunsBeforeExecution(t *testing.T) {
	fx := mkfix(t)
	rel := "in.wav"
	missing := filepath.Join(fx.dir, "missing.wav")
	noDir := filepath.Join(fx.dir, "nope", "out.wav")
	cases := []struct {
		name string
		args []string
	}{
		{"relative input", []string{"info", "--input", rel}},
		{"missing input", []string{"info", "--input", missing}},
		{"wrong ext info", []string{"info", "--input", filepath.Join(fx.dir, "bad.txt")}},
		{"no input", []string{"info"}},
		{"trim output same as input", []string{"trim", "--input", fx.wav, "--output", fx.wav, "--start", "0", "--end", "1"}},
		{"trim start negative", []string{"trim", "--input", fx.wav, "--output", fx.outWav, "--start", "-1", "--end", "2"}},
		{"trim end <= start", []string{"trim", "--input", fx.wav, "--output", fx.outWav, "--start", "2", "--end", "2"}},
		{"trim output parent missing", []string{"trim", "--input", fx.wav, "--output", noDir, "--start", "0", "--end", "1"}},
		{"normalize level too high", []string{"normalize", "--input", fx.wav, "--output", fx.outWav, "--level", "5"}},
		{"normalize level too low", []string{"normalize", "--input", fx.wav, "--output", fx.outWav, "--level", "-70"}},
		{"export format mismatch", []string{"export", "--input", fx.wav, "--output", fx.outMp3, "--format", "wav"}},
		{"export unknown format", []string{"export", "--input", fx.wav, "--output", fx.outWav, "--format", "xyz"}},
		{"export output same as input", []string{"export", "--input", fx.wav, "--output", fx.wav}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakePipe{response: "{}"}
			if _, err := execute(t, testRunner(f), tc.args...); err == nil {
				t.Fatal("expected a validation error")
			}
			if f.count() != 0 {
				t.Fatalf("pipe called %d times before validation failed", f.count())
			}
		})
	}
	for _, bad := range []string{"/tmp/a\nb.wav", "/tmp/a\x00.wav", ""} {
		if err := (Request{Kind: KindInfo, Input: bad}).Validate(); err == nil {
			t.Fatalf("Validate accepted %q", bad)
		}
	}
	if err := (Request{Kind: Kind("eval"), Input: fx.wav}).Validate(); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestResolverFailureMeansNoPipe(t *testing.T) {
	fx := mkfix(t)
	f := &fakePipe{response: "{}"}
	rn := &runner{resolve: func(context.Context, string) (string, error) { return "", ErrNoPin }, pipe: f.pipe}
	_, err := execute(t, rn, "info", "--input", fx.wav)
	if !errors.Is(err, ErrNoPin) {
		t.Fatalf("err = %v, want ErrNoPin", err)
	}
	if f.count() != 0 {
		t.Fatalf("pipe called %d times without a binary", f.count())
	}
}

func TestPipeFailureIsError(t *testing.T) {
	fx := mkfix(t)
	f := &fakePipe{err: errors.New("pipe write failed")}
	_, err := execute(t, testRunner(f), "info", "--input", fx.wav)
	if err == nil || !strings.Contains(err.Error(), "pipe write failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestUnsupportedVersionRejected(t *testing.T) {
	fx := mkfix(t)
	f := &fakePipe{response: "{}"}
	rn := testRunner(f)
	// version 4.0.0 must be rejected before pipe.
	err := rn.run(context.Background(), "4.0.0", Request{Kind: KindInfo, Input: fx.wav}, io.Discard)
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("err = %v, want ErrUnsupportedVersion", err)
	}
	if f.count() != 0 {
		t.Fatal("pipe called for unsupported version")
	}
	// 3.7.3 should succeed
	f2 := &fakePipe{response: "BatchCommand finished: OK"}
	rn2 := testRunner(f2)
	if err := rn2.run(context.Background(), "3.7.3", Request{Kind: KindInfo, Input: fx.wav}, io.Discard); err != nil {
		t.Fatalf("3.7.3 err = %v", err)
	}
}

func TestDeadlineReachesPipe(t *testing.T) {
	fx := mkfix(t)
	f := &fakePipe{block: true}
	cmd := newCmd(testRunner(f))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"info", "--input", fx.wav, "--timeout", "50ms"})
	start := time.Now()
	err := cmd.ExecuteContext(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("deadline did not interrupt pipe promptly")
	}
	if f.count() != 1 {
		t.Fatalf("pipe called %d times", f.count())
	}
}

func TestCallerContextCancellation(t *testing.T) {
	fx := mkfix(t)
	f := &fakePipe{block: true}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- testRunner(f).run(ctx, "", Request{Kind: KindInfo, Input: fx.wav}, io.Discard)
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
	f2 := &fakePipe{response: "{}"}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if err := testRunner(f2).run(ctx2, "", Request{Kind: KindInfo, Input: fx.wav}, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if f2.count() != 0 {
		t.Fatal("pipe called under a cancelled context")
	}
}

func TestNoDaemonListenerOrNetwork(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "audacity.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range file.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		switch {
		case p == "net" || strings.HasPrefix(p, "net/"), p == "os/signal", p == "os/exec",
			strings.Contains(p, "serviceenv"), strings.Contains(p, "/mcp"):
			t.Fatalf("audacity.go imports %q", p)
		}
	}
	src, err := os.ReadFile("audacity.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, bad := range []string{".Start(", "binmgr.Launch", "binmgr.Start", "binmgr.RunSpec", "Setsid", "Setpgid", "Listen(", "http."} {
		if strings.Contains(s, bad) {
			t.Fatalf("audacity.go contains %q", bad)
		}
	}
	// Must not use net.Listen or TCP dial.
	if strings.Contains(s, "net.Dial") || strings.Contains(s, "TCPAddr") {
		t.Fatal("audacity.go must not contain network dial")
	}
	// The bounded pipe transaction may use short-lived goroutines for
	// context-cancellation of blocking FIFO opens; a daemon would use
	// persistent listeners, not a per-operation goroutine pair.
	if strings.Contains(s, "net.Listen") || strings.Contains(s, "Serve(") {
		t.Fatal("audacity.go must not contain network serve")
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
	if _, err := Spec("4.0.0"); err == nil {
		t.Fatal("Spec accepted Audacity 4")
	}
	if _, err := Spec("3.7"); err == nil {
		t.Fatal("Spec accepted non-patch version")
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
			if a.SHA256 == "" {
				t.Fatalf("%s: asset %+v missing SHA256", plat, a)
			}
			// Windows is Tree with entrypoint; Linux is raw binary.
			if plat == "windows/amd64" && (!a.Tree || a.Entrypoint == "") {
				t.Fatalf("%s: windows asset must be Tree with Entrypoint: %+v", plat, a)
			}
			if plat == "linux/amd64" && a.Tree {
				t.Fatalf("%s: linux asset must be raw binary (not Tree): %+v", plat, a)
			}
			continue
		}
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
	if _, err := Path(context.Background(), "9.9.9"); !errors.Is(err, ErrUnsupportedVersion) && !errors.Is(err, ErrNoPin) && !errors.Is(err, ErrNoArtifact) {
		t.Fatalf("Path(9.9.9) = %v", err)
	}
}

func TestArtifactTable(t *testing.T) {
	a, ok := ArtifactFor("3.7.3", "linux", "amd64")
	if !ok || a.URL != "https://github.com/audacity/audacity/releases/download/Audacity-3.7.3/audacity-linux-3.7.3-x64-22.04.AppImage" || a.IsTree {
		t.Fatalf("linux/amd64 = %+v %v", a, ok)
	}
	a, ok = ArtifactFor("3.7.3", "windows", "amd64")
	if !ok || a.URL != "https://github.com/audacity/audacity/releases/download/Audacity-3.7.3/audacity-win-3.7.3-64bit.zip" || !a.IsTree || a.Entrypoint != "audacity-win-3.7.3-64bit/Audacity.exe" {
		t.Fatalf("windows/amd64 = %+v %v", a, ok)
	}
	for _, plat := range []string{"darwin/arm64", "darwin/amd64", "linux/arm64", "windows/arm64"} {
		goos, goarch, _ := strings.Cut(plat, "/")
		if _, ok := ArtifactFor("3.7.3", goos, goarch); ok {
			t.Fatalf("%s: unexpected artifact", plat)
		}
	}
	if _, ok := ArtifactFor("4.0.0", "linux", "amd64"); ok {
		t.Fatal("4.0.0 must not resolve")
	}
	if _, ok := ArtifactFor("latest", "linux", "amd64"); ok {
		t.Fatal("latest resolved")
	}
}

func TestOfficialSourceAndPin(t *testing.T) {
	if releaseBase != "https://github.com/audacity/audacity/releases/download" {
		t.Fatalf("releaseBase = %q", releaseBase)
	}
	const officialWindowsAMD64 = "1d4c91d0bc1b5376f4c8768d48a6e79ab9dcb4600c1162053f61556becb45ce5" // audacity-win-3.7.3-64bit.zip
	sha, ok := binmgr.PinnedSHA256(toolName, DefaultVersion, "windows/amd64")
	if !ok || sha != officialWindowsAMD64 {
		t.Fatalf("windows/amd64 pin = %q, %v; want the official CHECKSUMS.txt digest", sha, ok)
	}
	tool, err := Spec("")
	if err != nil {
		t.Fatal(err)
	}
	a, ok := tool.Assets["windows/amd64"]
	if !ok || a.SHA256 != officialWindowsAMD64 || a.URL != "https://github.com/audacity/audacity/releases/download/Audacity-3.7.3/audacity-win-3.7.3-64bit.zip" || !a.Tree || a.Entrypoint != "audacity-win-3.7.3-64bit/Audacity.exe" {
		t.Fatalf("windows/amd64 asset = %+v %v", a, ok)
	}
	if a.SHA512 != "" || a.MD5 != "" {
		t.Fatalf("weaker digests must not be declared: %+v", a)
	}
	// Linux pin (when present) must match CHECKSUMS.txt as well.
	const officialLinuxAMD64 = "4e7600c3d9f09dca52c7171cc0bfe76abb168436143cd02ecfa3052ab9f7aaa1"
	if sha, ok := binmgr.PinnedSHA256(toolName, DefaultVersion, "linux/amd64"); ok {
		if sha != officialLinuxAMD64 {
			t.Fatalf("linux/amd64 pin = %q, want %q", sha, officialLinuxAMD64)
		}
	}
}

func TestEffectsAreDeclared(t *testing.T) {
	if got := EffectsFor(KindInfo); strings.Join(got, ",") != "read,process" {
		t.Fatalf("info effects = %v", got)
	}
	for _, k := range []Kind{KindTrim, KindNormalize, KindExport} {
		eff := EffectsFor(k)
		if !contains(eff, "read") || !contains(eff, "write") {
			t.Fatalf("%s effects = %v, want read+write", k, eff)
		}
	}
}

func contains(s []string, w string) bool {
	for _, v := range s {
		if v == w {
			return true
		}
	}
	return false
}

func TestPipeCommandsAreFixedSet(t *testing.T) {
	// Ensure every PipeCommands output is drawn from the fixed vocabulary.
	fx := mkfix(t)
	cases := []Request{
		{Kind: KindInfo, Input: fx.wav},
		{Kind: KindTrim, Input: fx.wav, Output: fx.outWav, Start: 0.5, End: 2.5},
		{Kind: KindNormalize, Input: fx.wav, Output: fx.outWav, Level: -3, LevelSet: true},
		{Kind: KindExport, Input: fx.aup3, Output: fx.outMp3, Format: "mp3"},
	}
	allowedPrefixes := []string{"Import2:", "GetInfo:", "Select:", "Trim:", "SelectAll:", "LoudnessNormalization:", "Export2:"}
	for _, r := range cases {
		cmds, err := r.PipeCommands()
		if err != nil {
			t.Fatalf("%s: %v", r.Kind, err)
		}
		for _, c := range cmds {
			ok := false
			for _, pre := range allowedPrefixes {
				if strings.HasPrefix(c, pre) {
					ok = true
					break
				}
			}
			if !ok {
				t.Fatalf("%s: command %q not in fixed vocabulary", r.Kind, c)
			}
		}
	}
	// Unknown operation rejected.
	if _, err := (Request{Kind: "unknown", Input: fx.wav}).PipeCommands(); err == nil {
		t.Fatal("unknown kind pipe commands accepted")
	}
}

func TestVersionGating(t *testing.T) {
	if _, err := Spec("4.0.1"); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("4.0.1 err = %v", err)
	}
	if _, ok := ArtifactFor("4.0.1", "windows", "amd64"); ok {
		t.Fatal("4.0.1 artifact exists")
	}
}

func TestResultEnvelopeIsVersionedJSON(t *testing.T) {
	fx := mkfix(t)
	f := &fakePipe{response: "BatchCommand finished: OK"}
	out, err := execute(t, testRunner(f), "info", "--input", fx.wav)
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.TrimPrefix(strings.TrimSpace(out), resultMarker)
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if m["schema_version"] != "yoke-audacity-v1" {
		t.Fatalf("schema_version = %v", m["schema_version"])
	}
	if m["op"] != "info" {
		t.Fatalf("op = %v", m["op"])
	}
}

func TestLinuxArtifactIsRawBinary(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("darwin has no artifact by design")
	}
	// If linux/amd64 is pinned, its asset must be raw binary.
	if _, ok := binmgr.PinnedSHA256(toolName, DefaultVersion, "linux/amd64"); ok {
		tool, err := Spec("")
		if err != nil {
			t.Fatal(err)
		}
		a := tool.Assets["linux/amd64"]
		if a.Tree || a.Entrypoint != "" {
			t.Fatalf("linux asset must be raw binary: %+v", a)
		}
	}
}

func TestVerifyPipeRejectsNonFIFOAndWrongOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO checks are Unix-only")
	}
	dir := t.TempDir()
	uid := os.Getuid()
	toPipe := filepath.Join(dir, fmt.Sprintf("audacity_script_pipe.to.%d", uid))
	fromPipe := filepath.Join(dir, fmt.Sprintf("audacity_script_pipe.from.%d", uid))
	// Missing pipe should fail closed.
	if err := verifyPipe(toPipe, uid); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing pipe err = %v, want not found", err)
	}
	// Regular file must be rejected as not a FIFO.
	if err := os.WriteFile(toPipe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyPipe(toPipe, uid); err == nil || !strings.Contains(err.Error(), "not a FIFO") {
		t.Fatalf("regular file err = %v, want not a FIFO", err)
	}
	os.Remove(toPipe)
	// FIFO owned by same user should pass.
	if err := mkFifo(toPipe, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	if err := verifyPipe(toPipe, uid); err != nil {
		t.Fatalf("same-user FIFO should pass: %v", err)
	}
	// FIFO with wrong UID must be rejected (same-user check via syscall.Stat_t).
	wrongUid := uid + 1
	// Handle overflow: if uid is max int, use uid-1 (still wrong).
	if wrongUid < 0 {
		wrongUid = uid - 1
	}
	if err := verifyPipe(toPipe, wrongUid); err == nil || !strings.Contains(err.Error(), "same-user") {
		t.Fatalf("wrong-owner err = %v, want same-user", err)
	}
	os.Remove(toPipe)
	// Repeat for from pipe.
	if err := mkFifo(fromPipe, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyPipe(fromPipe, uid); err != nil {
		t.Fatalf("from pipe same-user: %v", err)
	}
	os.Remove(fromPipe)
}

func TestPipeTransactionBoundedCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO checks are Unix-only")
	}
	dir := t.TempDir()
	orig := pipeDirOverride
	pipeDirOverride = dir
	t.Cleanup(func() { pipeDirOverride = orig })

	toPipe, fromPipe := pipePaths()
	if err := mkFifo(toPipe, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mkFifo(fromPipe, 0o600); err != nil {
		t.Fatal(err)
	}
	// No fake Audacity counterpart: open will block until context deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := defaultPipeClient(ctx, []string{`Import2: Filename="/tmp/x.wav"`, "GetInfo: Type=Tracks Format=JSON"})
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want DeadlineExceeded or Canceled", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("bounded cancellation took %v, want <2s", time.Since(start))
	}
	// Also test that an already-cancelled context returns immediately without touching pipe.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := defaultPipeClient(ctx2, []string{"GetInfo: Type=Tracks Format=JSON"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("already-cancelled err = %v, want Canceled", err)
	}
}

// fakeAudacity is a fixture mod-script-pipe server with the REAL server's
// shape: it reads ONE line from the to-FIFO, replies on the from-FIFO, and
// only then reads the next line. Replies are indexed by command position;
// a nil reply withholds the response forever (the adversarial case). Each
// command's arrival and each reply's send are timestamped so tests can
// prove strict request/reply ordering, and whatever reaches the to-FIFO
// after the client gives up is drained into got (up to EOF) so a test can
// prove nothing more was written.
type fakeAudacity struct {
	mu      sync.Mutex
	got     []string
	gotAt   []time.Time
	sentAt  []time.Time
	replies []*string
	done    chan struct{}
}

func reply(s string) *string { return &s }

func startFakeAudacity(t *testing.T, toPipe, fromPipe string, replies ...*string) *fakeAudacity {
	t.Helper()
	f := &fakeAudacity{replies: replies, done: make(chan struct{})}
	go func() {
		defer close(f.done)
		rf, err := os.OpenFile(toPipe, os.O_RDONLY, 0)
		if err != nil {
			return
		}
		defer rf.Close()
		var wf *os.File
		defer func() {
			if wf != nil {
				wf.Close()
			}
		}()
		rd := bufio.NewReader(rf)
		for i := 0; ; i++ {
			line, err := rd.ReadString('\n')
			if line != "" {
				f.mu.Lock()
				f.got = append(f.got, line)
				f.gotAt = append(f.gotAt, time.Now())
				f.mu.Unlock()
			}
			if err != nil {
				return // client closed its write end (EOF)
			}
			if i >= len(f.replies) || f.replies[i] == nil {
				continue // withhold: never reply, keep draining
			}
			if wf == nil {
				wf, err = os.OpenFile(fromPipe, os.O_WRONLY, 0)
				if err != nil {
					return
				}
			}
			f.mu.Lock()
			f.sentAt = append(f.sentAt, time.Now())
			f.mu.Unlock()
			_, _ = io.WriteString(wf, *f.replies[i])
		}
	}()
	return f
}

func (f *fakeAudacity) wait(t *testing.T) {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake Audacity did not see EOF on the to-FIFO (client left it open?)")
	}
}

func (f *fakeAudacity) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

const okReply = "{\"tracks\":[]}\nBatchCommand finished: OK\n\n"

func TestPipeWireTransaction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO checks are Unix-only")
	}
	dir := t.TempDir()
	orig := pipeDirOverride
	pipeDirOverride = dir
	t.Cleanup(func() { pipeDirOverride = orig })

	toPipe, fromPipe := pipePaths()
	if err := mkFifo(toPipe, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mkFifo(fromPipe, 0o600); err != nil {
		t.Fatal(err)
	}
	commands := []string{`Import2: Filename="/tmp/in.wav"`, "GetInfo: Type=Tracks Format=JSON"}
	fake := startFakeAudacity(t, toPipe, fromPipe,
		reply("\nBatchCommand finished: OK\n\n"), reply(okReply))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := defaultPipeClient(ctx, commands)
	if err != nil {
		t.Fatalf("pipe client: %v", err)
	}
	fake.wait(t)
	// Each command is one newline-terminated line, in order, nothing else.
	got := fake.lines()
	if len(got) != len(commands) {
		t.Fatalf("lines written = %q, want %d commands", got, len(commands))
	}
	for i, c := range commands {
		if got[i] != c+"\n" {
			t.Fatalf("line %d = %q, want %q", i, got[i], c+"\n")
		}
	}
	// The aggregate carries BOTH replies (one marker per command), not
	// just the first one the old bulk protocol stopped at.
	if n := strings.Count(resp, "BatchCommand finished: OK"); n != 2 {
		t.Fatalf("aggregate = %q, want 2 finished markers", resp)
	}
	if !strings.Contains(resp, "{\"tracks\":[]}") || !containsBatchFinished(resp) {
		t.Fatalf("response = %q, want GetInfo payload + marker", resp)
	}
}

// TestPipeStrictRequestReplyOrdering pins the pipe_test.py protocol: command
// N+1 is written only AFTER reply N has been read. The fake delays reply 1
// and records when each command arrived and each reply was sent; the second
// command must arrive strictly after the first reply left.
func TestPipeStrictRequestReplyOrdering(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO checks are Unix-only")
	}
	dir := t.TempDir()
	orig := pipeDirOverride
	pipeDirOverride = dir
	t.Cleanup(func() { pipeDirOverride = orig })

	toPipe, fromPipe := pipePaths()
	if err := mkFifo(toPipe, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mkFifo(fromPipe, 0o600); err != nil {
		t.Fatal(err)
	}
	commands := []string{`Import2: Filename="/tmp/in.wav"`, "SelectAll:", "GetInfo: Type=Tracks Format=JSON"}
	f := &fakeAudacity{done: make(chan struct{})}
	const hold = 150 * time.Millisecond
	go func() {
		defer close(f.done)
		rf, err := os.OpenFile(toPipe, os.O_RDONLY, 0)
		if err != nil {
			return
		}
		defer rf.Close()
		wf, err := os.OpenFile(fromPipe, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer wf.Close()
		rd := bufio.NewReader(rf)
		for i := 0; i < len(commands); i++ {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			f.mu.Lock()
			f.got = append(f.got, line)
			f.gotAt = append(f.gotAt, time.Now())
			f.mu.Unlock()
			time.Sleep(hold)
			f.mu.Lock()
			f.sentAt = append(f.sentAt, time.Now())
			f.mu.Unlock()
			_, _ = io.WriteString(wf, "\nBatchCommand finished: OK\n\n")
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := defaultPipeClient(ctx, commands)
	if err != nil {
		t.Fatalf("pipe client: %v", err)
	}
	<-f.done
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) != 3 || len(f.sentAt) != 3 {
		t.Fatalf("got %d commands / %d replies, want 3/3", len(f.got), len(f.sentAt))
	}
	for i := 1; i < len(commands); i++ {
		if !f.gotAt[i].After(f.sentAt[i-1]) {
			t.Fatalf("command %d arrived at %v, before reply %d was sent at %v: bulk write, not request/reply",
				i+1, f.gotAt[i], i, f.sentAt[i-1])
		}
	}
	if n := strings.Count(resp, "BatchCommand finished: OK"); n != 3 {
		t.Fatalf("aggregate = %q, want 3 markers", resp)
	}
}

// TestPipeWithheldReplyRejectsSecondCommand is the adversarial case: the
// fake reads command 1 and NEVER replies. The client must time out on
// reply 1 and must not have written command 2 — a bulk writer would have.
func TestPipeWithheldReplyRejectsSecondCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO checks are Unix-only")
	}
	dir := t.TempDir()
	orig := pipeDirOverride
	pipeDirOverride = dir
	t.Cleanup(func() { pipeDirOverride = orig })

	toPipe, fromPipe := pipePaths()
	if err := mkFifo(toPipe, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mkFifo(fromPipe, 0o600); err != nil {
		t.Fatal(err)
	}
	commands := []string{`Import2: Filename="/tmp/in.wav"`, "GetInfo: Type=Tracks Format=JSON"}
	fake := startFakeAudacity(t, toPipe, fromPipe, nil, reply(okReply)) // reply 1 withheld

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	resp, err := defaultPipeClient(ctx, commands)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded while waiting for reply 1", err)
	}
	if resp != "" {
		t.Fatalf("resp = %q, want nothing on failure", resp)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("withheld reply took %v to bound, want <2s", time.Since(start))
	}
	fake.wait(t) // client closed the to-FIFO; fake drained everything to EOF
	got := fake.lines()
	if len(got) != 1 || got[0] != commands[0]+"\n" {
		t.Fatalf("to-FIFO received %q; command 2 must never be written before reply 1 is read", got)
	}
	for _, l := range got {
		if strings.Contains(l, "GetInfo") {
			t.Fatalf("command 2 reached the pipe before reply 1: %q", got)
		}
	}
}

// TestPipeFailedReplyAbortsBeforeNextCommand: a "Failed" reply to command 1
// (e.g. Import2 of an unreadable file) must fail the operation immediately;
// command 2 (which would act on a missing track) is never written.
func TestPipeFailedReplyAbortsBeforeNextCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO checks are Unix-only")
	}
	dir := t.TempDir()
	orig := pipeDirOverride
	pipeDirOverride = dir
	t.Cleanup(func() { pipeDirOverride = orig })

	toPipe, fromPipe := pipePaths()
	if err := mkFifo(toPipe, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mkFifo(fromPipe, 0o600); err != nil {
		t.Fatal(err)
	}
	commands := []string{`Import2: Filename="/tmp/in.wav"`, "SelectAll:", `Export2: Filename="/tmp/out.wav" NumChannels=1`}
	fake := startFakeAudacity(t, toPipe, fromPipe,
		reply("\nBatchCommand finished: Failed!\n\n"), reply(okReply), reply(okReply))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := defaultPipeClient(ctx, commands)
	if err == nil {
		t.Fatal("Failed reply must be an error")
	}
	if !strings.Contains(err.Error(), "command 1/3 (Import2) failed") || !strings.Contains(err.Error(), "Failed!") {
		t.Fatalf("err = %v, want command 1/3 (Import2) failed … Failed!", err)
	}
	if strings.Contains(err.Error(), "/tmp/in.wav") {
		t.Fatalf("error must name the command ID, not echo the quoted path: %v", err)
	}
	fake.wait(t)
	if got := fake.lines(); len(got) != 1 {
		t.Fatalf("to-FIFO received %q; nothing may follow a Failed reply", got)
	}
}

// TestPipeReplyBoundIsEnforced: a reply that never reaches its marker is
// cut off at maxReplyBytes rather than buffered without bound.
func TestPipeReplyBoundIsEnforced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO checks are Unix-only")
	}
	dir := t.TempDir()
	orig := pipeDirOverride
	pipeDirOverride = dir
	t.Cleanup(func() { pipeDirOverride = orig })

	toPipe, fromPipe := pipePaths()
	if err := mkFifo(toPipe, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mkFifo(fromPipe, 0o600); err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat(strings.Repeat("x", 1023)+"\n", maxReplyBytes/1024+2) // > bound, no marker
	fake := startFakeAudacity(t, toPipe, fromPipe, reply(huge))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := defaultPipeClient(ctx, []string{"GetInfo: Type=Tracks Format=JSON"})
	if !errors.Is(err, errReplyTooLarge) {
		t.Fatalf("err = %v, want errReplyTooLarge", err)
	}
	fake.wait(t)
}

func TestDefaultPipeClientRejectsRegularFileAsPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO checks are Unix-only")
	}
	dir := t.TempDir()
	orig := pipeDirOverride
	pipeDirOverride = dir
	t.Cleanup(func() { pipeDirOverride = orig })

	toPipe, fromPipe := pipePaths()
	// Create regular files instead of FIFOs.
	if err := os.WriteFile(toPipe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := mkFifo(fromPipe, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := defaultPipeClient(context.Background(), []string{"GetInfo: Type=Tracks Format=JSON"})
	if err == nil || !strings.Contains(err.Error(), "not a FIFO") {
		t.Fatalf("err = %v, want not a FIFO", err)
	}
}

func TestOpenFileWithContextNoLeakedGoroutine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO checks are Unix-only")
	}
	dir := t.TempDir()
	orig := pipeDirOverride
	pipeDirOverride = dir
	t.Cleanup(func() { pipeDirOverride = orig })

	toPipe, fromPipe := pipePaths()
	if err := mkFifo(toPipe, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mkFifo(fromPipe, 0o600); err != nil {
		t.Fatal(err)
	}
	// Repeated cancellations must remain bounded: the old implementation
	// left a blocked os.OpenFile goroutine per call, so N cancellations
	// leaked N goroutines and the (N+1)th call still waited for the first's
	// goroutine's 80ms timeout * N. With O_NONBLOCK+retry, each cancellation
	// returns promptly and leaves no residue.
	start := time.Now()
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
		_, err := defaultPipeClient(ctx, []string{`Import2: Filename="/tmp/x.wav"`})
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Fatalf("iter %d: err = %v, want DeadlineExceeded/Canceled", i, err)
		}
	}
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("50 bounded cancellations took %v, want <5s (leaked goroutines would cause ~>10s or timeout leaks)", elapsed)
	}
	// After the storm, a real wire session must still succeed promptly,
	// proving no leaked goroutines are still holding the FIFO.
	commands := []string{`Import2: Filename="/tmp/in.wav"`, "GetInfo: Type=Tracks Format=JSON"}
	fake := startFakeAudacity(t, toPipe, fromPipe,
		reply("\nBatchCommand finished: OK\n\n"), reply(okReply))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := defaultPipeClient(ctx, commands)
	if err != nil {
		t.Fatalf("wire after storm: %v", err)
	}
	fake.wait(t)
	got := fake.lines()
	if len(got) != 2 || got[0] != commands[0]+"\n" || got[1] != commands[1]+"\n" {
		t.Fatalf("lines after storm = %q, want %q", got, commands)
	}
	if !containsBatchFinished(resp) {
		t.Fatalf("response after storm = %q, want BatchCommand finished", resp)
	}
}

func TestPipeWireBlankLineFraming(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO checks are Unix-only")
	}
	dir := t.TempDir()
	orig := pipeDirOverride
	pipeDirOverride = dir
	t.Cleanup(func() { pipeDirOverride = orig })

	toPipe, fromPipe := pipePaths()
	if err := mkFifo(toPipe, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mkFifo(fromPipe, 0o600); err != nil {
		t.Fatal(err)
	}
	commands := []string{`Import2: Filename="/tmp/in.wav"`, "GetInfo: Type=Tracks Format=JSON"}
	// Real Audacity replies with a blank line before the marker; also test
	// that a split delivery (marker halves across reads) is still recognized.
	// The fake answers each command in turn and writes the LAST marker in
	// two chunks to exercise the line-accumulation logic.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rf, err := os.OpenFile(toPipe, os.O_RDONLY, 0)
		if err != nil {
			return
		}
		defer rf.Close()
		wf, err := os.OpenFile(fromPipe, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer wf.Close()
		rd := bufio.NewReader(rf)
		if _, err := rd.ReadString('\n'); err != nil {
			return
		}
		_, _ = io.WriteString(wf, "\nBatchCommand finished: OK\n\n")
		if _, err := rd.ReadString('\n'); err != nil {
			return
		}
		// JSON + blank line, flush, then marker halves.
		_, _ = io.WriteString(wf, "{\"tracks\":[]}\n\nBatchCommand")
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(wf, " finished: OK\n\n")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := defaultPipeClient(ctx, commands)
	if err != nil {
		t.Fatalf("pipe client blank-line framing: %v", err)
	}
	wg.Wait()
	if !strings.Contains(resp, "BatchCommand finished: OK") {
		t.Fatalf("resp = %q, want BatchCommand finished: OK", resp)
	}
	if !containsBatchFinished(resp) {
		t.Fatal("containsBatchFinished failed on blank-line framed response")
	}
	// Ensure requiresBlankLine: a marker mid-payload without newline should NOT count.
	// containsBatchFinished must still return true for the real response but false for embedded substring.
	if !containsBatchFinished("{\"note\":\"BatchCommand finished: trick\"}\n\nBatchCommand finished: OK\n") {
		t.Fatal("should detect real marker after blank line")
	}
}

func TestBatchFinishedLineAware(t *testing.T) {
	// Marker must be at line start (after TrimSpace), not a substring of JSON.
	if containsBatchFinished("{\"msg\":\"BatchCommand finished: fake\"}") {
		t.Fatal("substring inside JSON should not be detected as batch finished")
	}
	if !containsBatchFinished("foo\nBatchCommand finished: OK") {
		t.Fatal("line-start marker should be detected")
	}
	if !containsBatchFinished("foo\n\nBatchCommand finished: Failed\n") {
		t.Fatal("blank-line + marker should be detected")
	}
	if !containsBatchFinished("BatchCommand finished: OK") {
		t.Fatal("marker alone should be detected")
	}
	if containsBatchFinished("BatchCommand finished") {
		t.Fatal("marker without colon should not be detected")
	}
}
