package gomod

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

const (
	headB   = "bbbbbbbbbbbb0000000000000000000000000000"
	headC   = "cccccccccccc0000000000000000000000000000"
	headF   = "ffffffffffff0000000000000000000000000000"
	oldRevB = "000000000001"
)

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture: a pins b (stale pseudo-version), b/nested (tag at HEAD),
// c (local replace) and the fork f (versioned replace at HEAD).
func fixture(t *testing.T) (root string, ws *Workspace) {
	t.Helper()
	t.Setenv("GOWORK", "")
	root = t.TempDir()
	write(t, filepath.Join(root, "go.work"), "go 1.24\n\nuse (\n\t./a\n\t./b\n\t./b/nested\n\t./c\n\t./f\n)\n")
	write(t, filepath.Join(root, "a/go.mod"), `module example.com/a

go 1.24

require (
	example.com/b v0.0.0-20260101000000-`+oldRevB+`
	example.com/b/nested v1.2.0
	example.com/c v0.0.0
	upstream.example/f/v3 v3.1.0
)

replace example.com/c => ../c

replace upstream.example/f/v3 => example.com/f/v3 v3.0.0-20260102000000-ffffffffffff
`)
	write(t, filepath.Join(root, "b/go.mod"), "module example.com/b\n\ngo 1.24\n\nrequire example.com/c v0.0.0-20260101000000-cccccccccccc\n")
	write(t, filepath.Join(root, "b/nested/go.mod"), "module example.com/b/nested\n\ngo 1.24\n")
	write(t, filepath.Join(root, "c/go.mod"), "module example.com/c\n\ngo 1.24\n")
	write(t, filepath.Join(root, "f/go.mod"), "module upstream.example/f/v3\n\ngo 1.24\n")
	var err error
	ws, err = Load(filepath.Join(root, "a"))
	if err != nil || ws == nil {
		t.Fatalf("Load: %v %v", ws, err)
	}
	return root, ws
}

func fakeGit(root string) ResolveFunc {
	heads := map[string]string{"b": headB, "c": headC, "f": headF}
	return func(dir, rev string) (string, error) {
		name := filepath.Base(dir)
		if rev == "HEAD" {
			if h, ok := heads[name]; ok {
				return h, nil
			}
			return "", errors.New("no repo")
		}
		if name == "b" && rev == "refs/tags/nested/v1.2.0" {
			return headB, nil
		}
		return "", errors.New("unknown revision")
	}
}

func TestFindWorkspaceHonoursGOWORK(t *testing.T) {
	root, _ := fixture(t)
	if wf, ok := FindWorkspace(filepath.Join(root, "b", "nested")); !ok || wf != filepath.Join(root, "go.work") {
		t.Fatalf("walk up: %q %v", wf, ok)
	}
	t.Setenv("GOWORK", "off")
	if _, ok := FindWorkspace(root); ok {
		t.Fatal("GOWORK=off still found a workspace")
	}
	if ws, err := Load(root); ws != nil || err != nil {
		t.Fatalf("GOWORK=off Load = %v, %v; want nil, nil", ws, err)
	}
}

func TestSiblingRequiresAndDirs(t *testing.T) {
	root, ws := fixture(t)
	a := ws.Module(filepath.Join(root, "a"))
	var got []string
	for _, r := range ws.SiblingRequires(a) {
		got = append(got, r.Path+"@"+r.Version)
	}
	want := []string{
		"example.com/b@v0.0.0-20260101000000-" + oldRevB,
		"example.com/b/nested@v1.2.0",
		"example.com/c@",
		"upstream.example/f/v3@v3.0.0-20260102000000-ffffffffffff",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requires\n got %v\nwant %v", got, want)
	}
	// b/nested collapses to b; c is reached directly and through b.
	dirs := ws.SiblingDirs(a)
	wantDirs := []string{filepath.Join(root, "b"), filepath.Join(root, "c"), filepath.Join(root, "f")}
	if !reflect.DeepEqual(dirs, wantDirs) {
		t.Fatalf("dirs\n got %v\nwant %v", dirs, wantDirs)
	}
	// A nested module that is its own git repo (a submodule) is its own repo.
	write(t, filepath.Join(root, "b/nested/.git"), "gitdir: ../../.git/modules/nested\n")
	if got := ws.RepoDir(filepath.Join(root, "b", "nested")); got != filepath.Join(root, "b", "nested") {
		t.Fatalf("submodule RepoDir = %s", got)
	}
	// The umbrella's own .git never makes the root a sibling repo.
	write(t, filepath.Join(root, ".git/HEAD"), "ref: refs/heads/dev\n")
	if got := ws.RepoDir(filepath.Join(root, "b")); got != filepath.Join(root, "b") {
		t.Fatalf("RepoDir under an umbrella = %s", got)
	}
}

func TestDrift(t *testing.T) {
	root, ws := fixture(t)
	got := map[string]State{}
	for _, d := range ws.Drift(ws.Module(filepath.Join(root, "a")), fakeGit(root)) {
		got[d.Name] = d.State
	}
	want := map[string]State{"b": Stale, "b/nested": InSync, "c": Unknown, "f": InSync}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("drift\n got %v\nwant %v", got, want)
	}
	// b pins c at c's HEAD.
	for _, d := range ws.Drift(ws.Module(filepath.Join(root, "b")), fakeGit(root)) {
		if d.State != InSync {
			t.Fatalf("b -> %s: %s %s", d.Name, d.State, d.Reason)
		}
	}
}

func TestCompareRules(t *testing.T) {
	head := "0123456789ab" + strings.Repeat("0", 28)
	noTag := func(string) (string, error) { return "", errors.New("none") }
	cases := []struct {
		r    Require
		want State
	}{
		{Require{Version: "v0.0.0-20260101000000-0123456789ab"}, InSync},
		{Require{Version: "v0.0.0-20260101000000-ffffffffffff"}, Stale},
		{Require{Version: "v0.0.0-00010101000000-000000000000"}, Unknown},
		{Require{Version: "v0.0.0"}, Unknown},
		{Require{Version: "v1.0.0"}, Unknown}, // tag missing
		{Require{Local: true, Via: "../x"}, Unknown},
	}
	for _, c := range cases {
		if got, why := compare(c.r, head, noTag); got != c.want {
			t.Errorf("%+v: %s (%s), want %s", c.r, got, why, c.want)
		}
	}
	if got, _ := compare(Require{Version: "v1.0.0"}, head, func(string) (string, error) { return "f" + head[1:], nil }); got != Stale {
		t.Errorf("tag at another commit: %s, want stale", got)
	}
}

func TestSyncRunsGoGetAndReplaceEdit(t *testing.T) {
	root, ws := fixture(t)
	var calls []string
	run := func(_ context.Context, dir string, env []string, args ...string) ([]byte, error) {
		if dir != filepath.Join(root, "a") || !reflect.DeepEqual(env, pinnedEnv) {
			t.Errorf("run in %s env %v", dir, env)
		}
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	}
	// Make f stale too so the fork-replace path runs.
	git := fakeGit(root)
	stale := func(dir, rev string) (string, error) {
		if filepath.Base(dir) == "f" && rev == "HEAD" {
			return "999999999999" + strings.Repeat("0", 28), nil
		}
		return git(dir, rev)
	}
	ch, err := ws.Sync(context.Background(), ws.Module(filepath.Join(root, "a")), nil, stale, run)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"get example.com/b@" + headB,
		"mod edit -replace=upstream.example/f/v3=example.com/f/v3@999999999999" + strings.Repeat("0", 28),
		"mod tidy",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls\n got %q\nwant %q", calls, want)
	}
	if len(ch) != 2 {
		t.Fatalf("changes = %+v", ch)
	}
	// --only limits; nothing stale means no tidy.
	calls = nil
	if ch, err := ws.Sync(context.Background(), ws.Module(filepath.Join(root, "a")), []string{"c"}, git, run); err != nil || ch != nil || calls != nil {
		t.Fatalf("only c (local replace): %v %v %v", ch, err, calls)
	}
}

func TestModuleDir(t *testing.T) {
	for _, bad := range []string{"example.com/x", "example.com/x@latest", "example.com/x@main", "Bad Path@v1.0.0"} {
		if _, err := ModuleDir(context.Background(), bad, false, nil); ExitCodeOf(err) != weavecli.ExitInvalidArg {
			t.Errorf("%s: %v, want invalid arg", bad, err)
		}
	}
	var gotEnv []string
	run := func(_ context.Context, _ string, env []string, args ...string) ([]byte, error) {
		gotEnv = env
		if strings.Join(args, " ") != "mod download -json example.com/x@v1.0.0" {
			t.Errorf("args %v", args)
		}
		return []byte(`{"Path":"example.com/x","Version":"v1.0.0","Dir":"/cache/example.com/x@v1.0.0","Sum":"h1:x"}`), nil
	}
	info, err := ModuleDir(context.Background(), "example.com/x@v1.0.0", true, run)
	if err != nil || info.Dir != "/cache/example.com/x@v1.0.0" {
		t.Fatalf("info %+v %v", info, err)
	}
	if !strings.Contains(strings.Join(gotEnv, " "), "GOPROXY=off") {
		t.Fatalf("offline env %v", gotEnv)
	}
	fail := func(context.Context, string, []string, ...string) ([]byte, error) {
		return []byte(`{"Path":"example.com/x","Version":"v1.0.0","Error":"not in cache"}`), errors.New("exit 1")
	}
	if _, err := ModuleDir(context.Background(), "example.com/x@v1.0.0", true, fail); ExitCodeOf(err) != weavecli.ExitDepUnhealthy {
		t.Fatalf("download error: %v", err)
	}
}

func TestTools(t *testing.T) {
	tools, err := Tools([]byte(`module example.com/bashy

go 1.24

tool example.com/outpost/cmd/outpost

require (
	example.com/outpost v0.0.0-20261001000000-abcdefabcdef
	example.com/outpost/cmd v1.0.0
)
`))
	if err != nil || len(tools) != 1 {
		t.Fatalf("%v %v", tools, err)
	}
	if tl := tools[0]; tl.Module != "example.com/outpost/cmd" || tl.Version != "v1.0.0" {
		t.Fatalf("longest prefix: %+v", tl)
	}
	tools, _ = Tools([]byte("module m\n\ngo 1.24\n\ntool example.com/o/cmd/o\n\nrequire example.com/o v0.0.0-20261001000000-abcdefabcdef\n"))
	if tools[0].Commit != "abcdefabcdef" {
		t.Fatalf("commit %+v", tools[0])
	}
}

func TestInit(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(dir, "", ""); ExitCodeOf(err) != weavecli.ExitInputRequired {
		t.Fatalf("no remote: %v", err)
	}
	res, err := Init(dir, "github.com/owner/bashsharp-tests", "1.24")
	if err != nil || !reflect.DeepEqual(res.Created, []string{"go.mod", "pkg.go"}) || len(res.Warnings) != 1 {
		t.Fatalf("first: %+v %v", res, err)
	}
	pkg, _ := os.ReadFile(filepath.Join(dir, "pkg.go"))
	if !strings.Contains(string(pkg), "package bashsharptests") {
		t.Fatalf("pkg.go:\n%s", pkg)
	}
	// Idempotent; the module path is read back from go.mod.
	res, err = Init(dir, "", "")
	if err != nil || len(res.Created) != 0 || !reflect.DeepEqual(res.Unchanged, []string{"go.mod", "pkg.go"}) {
		t.Fatalf("second: %+v %v", res, err)
	}
	if _, err := Init(dir, "github.com/owner/other", ""); ExitCodeOf(err) != weavecli.ExitStateConflict {
		t.Fatalf("path mismatch: %v", err)
	}
	// A Go project gets no stub.
	goDir := t.TempDir()
	write(t, filepath.Join(goDir, "main.go"), "package main\n")
	write(t, filepath.Join(goDir, "LICENSE"), "MIT\n")
	if res, err := Init(goDir, "github.com/owner/tool", "1.24"); err != nil || !reflect.DeepEqual(res.Created, []string{"go.mod"}) || len(res.Warnings) != 0 {
		t.Fatalf("go project: %+v %v", res, err)
	}
}

func TestModulePathFromRemote(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:qiangli/agent-bench.git":  "github.com/qiangli/agent-bench",
		"https://github.com/dhnt/appstore.git":    "github.com/dhnt/appstore",
		"https://github.com/bashsharp/tour":       "github.com/bashsharp/tour",
		"ssh://git@github.com/qiangli/yoke.git":   "github.com/qiangli/yoke",
		"https://user@example.com/owner/repo.git": "example.com/owner/repo",
	} {
		if got, err := ModulePathFromRemote(in); err != nil || got != want {
			t.Errorf("%s = %q %v, want %s", in, got, err, want)
		}
	}
	if got := PackageName("github.com/qiangli/sh/v3"); got != "sh" {
		t.Errorf("PackageName v3 = %s", got)
	}
	if got := PackageName("example.com/2fa"); got != "x2fa" {
		t.Errorf("PackageName digit = %s", got)
	}
}

func TestRunDriftExitCodes(t *testing.T) {
	t.Setenv("GOWORK", "off")
	var out, errb strings.Builder
	if code := Run([]string{"drift", t.TempDir()}, &out, &errb); code != 0 || !strings.Contains(out.String(), "no go.work") {
		t.Fatalf("standalone: code %d out %q err %q", code, out.String(), errb.String())
	}
	if code := Run([]string{"dir", "example.com/x@latest"}, &out, &errb); code != weavecli.ExitInvalidArg {
		t.Fatalf("dir latest: %d", code)
	}
}
