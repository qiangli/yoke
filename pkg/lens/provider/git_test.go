package provider

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func gitFixture(t *testing.T) (string, *gogit.Repository, func(msg string, files map[string]string) plumbing.Hash) {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	commit := func(msg string, files map[string]string) plumbing.Hash {
		for name, body := range files {
			full := filepath.Join(dir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := wt.Add("."); err != nil {
			t.Fatal(err)
		}
		h, err := wt.Commit(msg, &gogit.CommitOptions{Author: &object.Signature{Name: "T", Email: "t@example.com", When: fixedTime}})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	return dir, repo, commit
}

func TestGitProvider(t *testing.T) {
	ctx := context.Background()
	dir, repo, commit := gitFixture(t)
	first := commit("first\n\nbody", map[string]string{
		"a.txt":           "one",
		"pkg/lens/row.go": "package lens\n",
		"dist/app.zip":    string(zipBytes(t, member{name: "bin/app", body: "binary"})),
	})
	second := commit("second", map[string]string{"a.txt": "two"})
	if _, err := repo.CreateTag("v1", first, &gogit.CreateTagOptions{Tagger: &object.Signature{Name: "T", When: fixedTime}, Message: "v1"}); err != nil {
		t.Fatal(err)
	}
	g := NewGit(filepath.Join(dir, "pkg")) // discovered from a subdirectory

	root, err := g.Stat(ctx, loc(t, "git:HEAD:"))
	if err != nil || root.Kind != "directory" || root.Rev != second.String() || root.Attrs["subject"] != "second" || root.Children == nil || *root.Children != 3 {
		t.Fatalf("root %+v %v", root, err)
	}
	if p := root.Attrs["parents"].([]string); len(p) != 1 || p[0] != first.String() {
		t.Fatalf("parents %v", p)
	}
	rows, err := g.List(ctx, loc(t, "git:HEAD:"))
	if err != nil || len(rows) != 3 || rows[0].Ref != "git:HEAD:a.txt" || rows[0].Mode != "100644" || *rows[0].Size != 3 {
		t.Fatalf("list %+v %v", rows, err)
	}
	sub, err := g.List(ctx, loc(t, "git:HEAD:pkg"))
	if err != nil || len(sub) != 1 || sub[0].Ref != "git:HEAD:pkg/lens" || sub[0].Kind != "directory" || *sub[0].Children != 1 {
		t.Fatalf("sub %+v %v", sub, err)
	}
	if got := readAll(t, g, "git:HEAD:a.txt"); got != "two" {
		t.Fatalf("HEAD %q", got)
	}
	if got := readAll(t, g, "git:HEAD~1:a.txt"); got != "one" {
		t.Fatalf("HEAD~1 %q", got)
	}
	if got := readAll(t, g, "git:v1:pkg/lens/row.go"); got != "package lens\n" {
		t.Fatalf("tag %q", got)
	}
	// A container blob descends with !/ — no checkout, nothing extracted.
	if n, _ := g.Stat(ctx, loc(t, "git:HEAD:dist/app.zip")); n.Attrs["container"] != "zip" {
		t.Fatalf("container hint %+v", n)
	}
	if got := readAll(t, g, "git:HEAD:dist/app.zip!/bin/app"); got != "binary" {
		t.Fatalf("blob descent %q", got)
	}
	in, err := g.List(ctx, loc(t, "git:HEAD:dist/app.zip!/bin"))
	if err != nil || len(in) != 1 || in[0].Ref != "git:HEAD:dist/app.zip!/bin/app" {
		t.Fatalf("blob list %+v %v", in, err)
	}

	if _, err := g.Stat(ctx, loc(t, "git:HEAD:nope")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := g.Stat(ctx, loc(t, "git:no-such-branch:a.txt")); err == nil {
		t.Fatal("bad revision accepted")
	}
	if _, err := g.Open(ctx, loc(t, "git:HEAD:pkg")); err == nil {
		t.Fatal("opened a tree")
	}

	edges, err := g.Edges(ctx, loc(t, "git:HEAD:"))
	if err != nil {
		t.Fatal(err)
	}
	last := edges[len(edges)-1]
	if last.Rel != "parent" || last.Dst != "git:"+first.String()+":" {
		t.Fatalf("parent edge %+v", last)
	}

	// One provider = one snapshot: HEAD stays pinned while the branch moves.
	commit("third", map[string]string{"a.txt": "three"})
	if got := readAll(t, g, "git:HEAD:a.txt"); got != "two" {
		t.Fatalf("snapshot drifted: %q", got)
	}
	if got := readAll(t, NewGit(dir), "git:HEAD:a.txt"); got != "three" {
		t.Fatalf("fresh provider %q", got)
	}
}
