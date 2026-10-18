package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func revPathRepo(t *testing.T) (string, *gogit.Repository) {
	t.Helper()
	dir := setupTestRepo(t)
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "sub", "x.go"), []byte("package sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	if _, err := wt.Add("."); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "T", Email: "t@example.com", When: time.Now()}
	h, err := wt.Commit("second", &gogit.CommitOptions{Author: sig})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateTag("v1", h, &gogit.CreateTagOptions{Tagger: sig, Message: "v1"}); err != nil {
		t.Fatal(err)
	}
	return dir, repo
}

func TestResolveRevPath(t *testing.T) {
	_, repo := revPathRepo(t)
	for _, tc := range []struct {
		rev, path, want string
		mode            filemode.FileMode
	}{
		{"HEAD", "", "", filemode.Dir},
		{"HEAD", "pkg/sub", "pkg/sub", filemode.Dir},
		{"HEAD", "pkg/sub/", "pkg/sub", filemode.Dir},
		{"HEAD", "pkg/sub/x.go", "pkg/sub/x.go", filemode.Regular},
		{"v1", "hello.txt", "hello.txt", filemode.Regular}, // annotated tag peels
		{"HEAD~1", "hello.txt", "hello.txt", filemode.Regular},
	} {
		rp, err := ResolveRevPath(repo, tc.rev, tc.path)
		if err != nil {
			t.Fatalf("%s:%s: %v", tc.rev, tc.path, err)
		}
		if rp.Path != tc.want || rp.Mode != tc.mode || rp.Commit == nil {
			t.Fatalf("%s:%s: %+v", tc.rev, tc.path, rp)
		}
	}
	old, _ := ResolveRevPath(repo, "HEAD~1", "hello.txt")
	cur, _ := ResolveRevPath(repo, "HEAD", "hello.txt")
	if old.Hash == cur.Hash {
		t.Fatal("HEAD~1 and HEAD resolved the same blob")
	}
	if _, err := ResolveRevPath(repo, "HEAD~1", "pkg"); !errors.Is(err, ErrPathNotInRev) {
		t.Fatalf("missing path: %v", err)
	}
	for _, p := range []string{"./hello.txt", "../x", ".", "a/../../x"} {
		if _, err := ResolveRevPath(repo, "HEAD", p); !errors.Is(err, ErrRevPathForm) {
			t.Fatalf("%q: %v", p, err)
		}
	}
	if _, err := ResolveRevPath(repo, "", "hello.txt"); !errors.Is(err, ErrRevPathForm) {
		t.Fatalf("index form: %v", err)
	}
	// A tree id is a tree-ish too.
	rp, _ := ResolveRevPath(repo, "HEAD", "pkg")
	sub, err := ResolveRevPath(repo, rp.Hash.String(), "sub/x.go")
	if err != nil || sub.Commit != nil || sub.Mode != filemode.Regular {
		t.Fatalf("tree rev: %+v %v", sub, err)
	}
}

func TestCatFileRevPathNative(t *testing.T) {
	dir, _ := revPathRepo(t)
	ctx := context.Background()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"cat-file", "-p", "HEAD:hello.txt"}, "hello again\n"},
		{[]string{"cat-file", "-p", "HEAD~1:hello.txt"}, "hello world\n"},
		{[]string{"cat-file", "-t", "HEAD:pkg"}, "tree\n"},
		{[]string{"cat-file", "-t", "v1:pkg/sub/x.go"}, "blob\n"},
		{[]string{"cat-file", "-s", "HEAD:pkg/sub/x.go"}, "12\n"},
	} {
		res, err := Exec(ctx, dir, tc.args)
		if err != nil || res.ExitCode != 0 || res.Stdout != tc.want {
			t.Fatalf("%v: %+v %v", tc.args, res, err)
		}
	}
	res, err := Exec(ctx, dir, []string{"cat-file", "-p", "HEAD:nope"})
	if err != nil || res.ExitCode != 128 || res.Stderr != "fatal: path 'nope' does not exist in 'HEAD'\n" {
		t.Fatalf("missing: %+v %v", res, err)
	}
	// Index and cwd-relative forms stay host git's.
	for _, spec := range []string{":hello.txt", "HEAD:./hello.txt"} {
		if _, err := Exec(ctx, dir, []string{"cat-file", "-p", spec}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%s: %v", spec, err)
		}
	}
	if rev, p, ok := SplitRevPath("origin/main:pkg/a:b.go"); !ok || rev != "origin/main" || p != "pkg/a:b.go" {
		t.Fatalf("split: %q %q %v", rev, p, ok)
	}
}
