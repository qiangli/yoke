package lenscmd

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/qiangli/yoke/pkg/lens"
)

func s12Fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"docs/a.txt", "docs/b.txt", "top.txt"} {
		w, _ := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store, Modified: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
		w.Write([]byte(name))
	}
	zw.Close()
	if err := os.WriteFile(filepath.Join(dir, "rel.zip"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	if _, err := wt.Add("rel.zip"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("c", &gogit.CommitOptions{Author: &object.Signature{Name: "T", Email: "t@e", When: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestS12ProvidersThroughLens(t *testing.T) {
	dir := s12Fixture(t)
	for loc, want := range map[string]string{
		"zip:rel.zip":            "zip:rel.zip\nzip:rel.zip!/docs\nzip:rel.zip!/docs/a.txt\nzip:rel.zip!/docs/b.txt\nzip:rel.zip!/top.txt\n",
		"zip:rel.zip!/docs":      "zip:rel.zip!/docs\nzip:rel.zip!/docs/a.txt\nzip:rel.zip!/docs/b.txt\n",
		"git:HEAD:":              "git:HEAD:\ngit:HEAD:rel.zip\n",
		"git:HEAD:rel.zip!/docs": "git:HEAD:rel.zip!/docs\ngit:HEAD:rel.zip!/docs/a.txt\ngit:HEAD:rel.zip!/docs/b.txt\n",
	} {
		out, errb, code := runTool(t, dir, "", "-t", "paths", loc)
		if code != 0 || errb != "" {
			t.Fatalf("%s: %d %q", loc, code, errb)
		}
		if out != want {
			t.Errorf("%s paths:\n%s\nwant:\n%s", loc, out, want)
		}
	}
	// Pull and push are the same bytes for the new providers too.
	for _, loc := range []string{"zip:rel.zip", "git:HEAD:rel.zip!/."} {
		produced, errb, code := runTool(t, dir, "", "-t", "graph", loc)
		if code != 0 || errb != "" {
			t.Fatalf("%s produce: %d %q", loc, code, errb)
		}
		for _, view := range lens.GraphViews {
			pulled, _, c1 := runTool(t, dir, "", "-t", view, loc)
			pushed, _, c2 := runTool(t, dir, produced, "-t", view)
			if c1 != 0 || c2 != 0 || pulled != pushed {
				t.Errorf("%s %s: pull/push differ (%d/%d)", loc, view, c1, c2)
			}
		}
	}
	// Unknown descent fails loudly, never guesses a format.
	_, errb, code := runTool(t, dir, "", "-t", "paths", "zip:rel.zip!/top.txt!/x")
	if code == 0 || !strings.Contains(errb, "not a container") {
		t.Fatalf("bad descent: %d %q", code, errb)
	}
}
