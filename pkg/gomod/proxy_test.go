package gomod

import (
	"archive/zip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/pkg/weavecli"

	coregit "github.com/qiangli/yoke/git"
)

func zipNames(t *testing.T, file string) []string {
	t.Helper()
	r, err := zip.OpenReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var names []string
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	return names
}

func TestPublishLayoutAndImmutability(t *testing.T) {
	src, proxy := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "go.mod"), "module example.com/lib\n\ngo 1.22\n")
	write(t, filepath.Join(src, "lib.go"), "package lib\n\nconst V = 1\n")
	write(t, filepath.Join(src, "inner", "go.mod"), "module example.com/lib/inner\n")

	pub, err := Publish(src, "v0.1.0", proxy)
	if err != nil || !pub.Published || pub.Module != "example.com/lib" || !strings.HasPrefix(pub.Hash, "h1:") {
		t.Fatalf("publish: %+v %v", pub, err)
	}
	vdir := filepath.Join(proxy, "example.com", "lib", "@v")
	for _, f := range []string{"list", "v0.1.0.info", "v0.1.0.mod", "v0.1.0.zip"} {
		if _, err := os.Stat(filepath.Join(vdir, f)); err != nil {
			t.Fatalf("layout: %v", err)
		}
	}
	if got := zipNames(t, filepath.Join(vdir, "v0.1.0.zip")); !reflect.DeepEqual(got, []string{"example.com/lib@v0.1.0/go.mod", "example.com/lib@v0.1.0/lib.go"}) {
		t.Fatalf("zip holds %v; the nested module must be omitted", got)
	}

	again, err := Publish(src, "v0.1.0", proxy)
	if err != nil || again.Published || again.Hash != pub.Hash {
		t.Fatalf("identical republish must be a no-op: %+v %v", again, err)
	}
	write(t, filepath.Join(src, "lib.go"), "package lib\n\nconst V = 2\n")
	if _, err := Publish(src, "v0.1.0", proxy); ExitCodeOf(err) != weavecli.ExitPrecondFail || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("overwrite must be refused: %v", err)
	}
	if _, err := Publish(src, "v0.2.0", proxy); err != nil {
		t.Fatal(err)
	}
	if list, _ := os.ReadFile(filepath.Join(vdir, "list")); string(list) != "v0.1.0\nv0.2.0\n" {
		t.Fatalf("list = %q", list)
	}
	mods, err := ProxyStatus(proxy)
	if err != nil || !reflect.DeepEqual(mods, []ProxyModule{{Module: "example.com/lib", Versions: []string{"v0.1.0", "v0.2.0"}}}) {
		t.Fatalf("status: %+v %v", mods, err)
	}
	if mods, err := ProxyStatus(filepath.Join(proxy, "missing")); err != nil || len(mods) != 0 {
		t.Fatalf("missing proxy: %+v %v", mods, err)
	}

	for _, bad := range []struct{ mod, ver string }{{"module desk-kit\n", "v0.1.0"}, {"module example.com/lib\n", "1.0"}, {"module example.com/lib\n", "v1.0"}} {
		write(t, filepath.Join(src, "go.mod"), bad.mod)
		if _, err := Publish(src, bad.ver, proxy); ExitCodeOf(err) != weavecli.ExitInvalidArg {
			t.Errorf("%q %s: %v, want invalid arg", bad.mod, bad.ver, err)
		}
	}
}

// A pseudo-version names a commit: its zip is that commit's tree, whatever
// the working copy holds. An explicit version takes the tracked working copy.
func TestPublishPseudoVersionIsTheCommitTree(t *testing.T) {
	repo, proxy := t.TempDir(), t.TempDir()
	if _, err := coregit.Init(coregit.InitOptions{Path: repo, Quiet: true}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(repo, "lib")
	write(t, filepath.Join(dir, "go.mod"), "module example.com/repo/lib\n\ngo 1.22\n")
	write(t, filepath.Join(dir, "lib.go"), "package lib\n")
	if _, err := coregit.Add(coregit.AddOptions{RepoPath: repo, All: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := coregit.Commit(coregit.CommitOptions{RepoPath: repo, Message: "lib", AuthorName: "t", AuthorEmail: "t@example.invalid"}); err != nil {
		t.Fatal(err)
	}
	head, err := coregit.ResolveCommit(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "lib.go"), "package lib\n\nconst Dirty = 1\n")
	write(t, filepath.Join(dir, "untracked.go"), "package lib\n")

	pub, err := Publish(dir, "", proxy)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^v0\.0\.0-\d{14}-`+head[:12]+`$`).MatchString(pub.Version) || pub.Commit != head {
		t.Fatalf("pseudo-version %+v, want one naming %s", pub, head)
	}
	zr, err := zip.OpenReader(filepath.Join(proxy, "example.com", "repo", "lib", "@v", pub.Version+".zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var names []string
	for _, f := range zr.File {
		names = append(names, filepath.Base(f.Name))
		if filepath.Base(f.Name) == "lib.go" && f.UncompressedSize64 != uint64(len("package lib\n")) {
			t.Fatalf("lib.go is the working copy, want the committed blob")
		}
	}
	if !reflect.DeepEqual(names, []string{"go.mod", "lib.go"}) {
		t.Fatalf("zip %v, want the commit's files only", names)
	}

	exp, err := Publish(dir, "v0.1.0", proxy)
	if err != nil {
		t.Fatal(err)
	}
	if got := zipNames(t, filepath.Join(proxy, "example.com", "repo", "lib", "@v", "v0.1.0.zip")); len(got) != 2 || exp.Hash == pub.Hash {
		t.Fatalf("explicit version = tracked working copy: %v (%s vs %s)", got, exp.Hash, pub.Hash)
	}
}

// The go command resolves and builds a published version through
// GOPROXY=file:// with no network and no sibling directory.
func TestPublishedVersionResolvesWithGo(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	src, proxy, use := t.TempDir(), t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "go.mod"), "module example.com/lib\n\ngo 1.22\n")
	write(t, filepath.Join(src, "lib.go"), "package lib\n\nconst V = 1\n")
	if _, err := Publish(src, "v0.1.0", proxy); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(use, "go.mod"), "module example.com/use\n\ngo 1.22\n\nrequire example.com/lib v0.1.0\n")
	write(t, filepath.Join(use, "main.go"), "package main\n\nimport \"example.com/lib\"\n\nfunc main() { println(lib.V) }\n")
	inherited := map[string]string{"GOPROXY": "off", "GONOSUMDB": "example.com"}
	env := append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod -modcacherw", "GOMODCACHE="+filepath.Join(t.TempDir(), "modcache"))
	env = append(env, BuildEnv(proxy, func(k string) string { return inherited[k] })...)
	for _, args := range [][]string{{"list", "-m", "example.com/lib"}, {"build", "-o", os.DevNull, "."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir, cmd.Env = use, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, out)
		}
		if args[0] == "list" && strings.TrimSpace(string(out)) != "example.com/lib v0.1.0" {
			t.Fatalf("go list -m = %q", out)
		}
	}
	if sum, err := os.ReadFile(filepath.Join(use, "go.sum")); err != nil || !strings.Contains(string(sum), "example.com/lib v0.1.0 h1:") {
		t.Fatalf("go.sum %q %v", sum, err)
	}
}

func TestBuildEnv(t *testing.T) {
	none := func(string) string { return "" }
	got := BuildEnv("/p", none)
	want := []string{"GOPROXY=file:///p,https://proxy.golang.org,direct", "GONOSUMDB=" + LocalModulePrefix}
	if filepath.Separator == '/' && !reflect.DeepEqual(got, want) {
		t.Fatalf("default env %v, want %v", got, want)
	}
	inherited := map[string]string{"GOPROXY": "https://corp,direct", "GONOSUMDB": "corp.example"}
	got = BuildEnv("/p", func(k string) string { return inherited[k] })
	if !strings.HasSuffix(got[0], ",https://corp,direct") || got[1] != "GONOSUMDB=corp.example,"+LocalModulePrefix || len(got) != 2 {
		t.Fatalf("inherited env %v", got)
	}
	t.Setenv(ProxyEnv, "/x/proxy")
	if ProxyDir() != "/x/proxy" {
		t.Fatal(ProxyDir())
	}
	t.Setenv(ProxyEnv, "")
	t.Setenv("BASHY_HOME", "/h")
	if ProxyDir() != filepath.Join("/h", "gomod-proxy") {
		t.Fatal(ProxyDir())
	}
}
