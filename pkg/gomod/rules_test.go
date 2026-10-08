package gomod

import (
	"path/filepath"
	"reflect"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
)

func gitRepo(t *testing.T, dir, origin string) {
	t.Helper()
	r, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{origin}}); err != nil {
		t.Fatal(err)
	}
}

// In-repo local replaces wire a repo's own nested modules and are not
// sibling pins; a fork required without a replace is a pin on upstream.
func TestSiblingRequiresSkipsInRepoAndUpstreamPins(t *testing.T) {
	t.Setenv("GOWORK", "")
	root := t.TempDir()
	write(t, filepath.Join(root, "go.work"), "go 1.24\n\nuse (\n\t./y\n\t./y/pkg/oci\n\t./sh\n\t./g\n)\n")
	write(t, filepath.Join(root, "y/go.mod"), "module github.com/o/y\n\ngo 1.24\n\nrequire (\n\tgithub.com/o/y/pkg/oci v0.0.0\n\tmvdan.cc/sh/v3 v3.13.1\n)\n\nreplace github.com/o/y/pkg/oci => ./pkg/oci\n\nreplace mvdan.cc/sh/v3 => github.com/o/sh/v3 v3.0.0-20260101000000-aaaaaaaaaaaa\n")
	write(t, filepath.Join(root, "y/pkg/oci/go.mod"), "module github.com/o/y/pkg/oci\n\ngo 1.24\n")
	write(t, filepath.Join(root, "sh/go.mod"), "module mvdan.cc/sh/v3\n\ngo 1.24\n")
	write(t, filepath.Join(root, "g/go.mod"), "module github.com/o/g\n\ngo 1.24\n\nrequire mvdan.cc/sh/v3 v3.13.1\n")
	gitRepo(t, filepath.Join(root, "y"), "https://github.com/o/y.git")
	gitRepo(t, filepath.Join(root, "sh"), "git@github.com:o/sh.git")
	ws, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if !ws.IsFork(ws.ByPath("mvdan.cc/sh/v3")) || ws.IsFork(ws.ByPath("github.com/o/y")) {
		t.Fatal("fork detection")
	}
	var got []string
	for _, r := range ws.SiblingRequires(ws.ByPath("github.com/o/y")) {
		got = append(got, r.Path)
	}
	if !reflect.DeepEqual(got, []string{"mvdan.cc/sh/v3"}) {
		t.Fatalf("y pins %v, want only the fork replace", got)
	}
	if r := ws.SiblingRequires(ws.ByPath("github.com/o/g")); len(r) != 0 {
		t.Fatalf("g pins upstream sh, not the fork: %+v", r)
	}
}

// Two modules that pin each other can never both be in sync; the stale edge
// is reported as a cycle and does not count as stale.
func TestDriftReportsCycleEdges(t *testing.T) {
	t.Setenv("GOWORK", "")
	root := t.TempDir()
	write(t, filepath.Join(root, "go.work"), "go 1.24\n\nuse (\n\t./a\n\t./b\n)\n")
	write(t, filepath.Join(root, "a/go.mod"), "module example.com/a\n\ngo 1.24\n\nrequire example.com/b v0.0.0-20260101000000-bbbbbbbbbbbb\n")
	write(t, filepath.Join(root, "b/go.mod"), "module example.com/b\n\ngo 1.24\n\nrequire example.com/a v0.0.0-20260101000000-aaaaaaaaaaaa\n")
	ws, _ := Load(root)
	heads := func(dir, rev string) (string, error) {
		return "cccccccccccc0000000000000000000000000000", nil
	}
	for _, d := range ws.Drift(ws.ByPath("example.com/a"), heads) {
		if d.State != Cycle {
			t.Fatalf("a -> b: %s, want cycle", d.State)
		}
	}
}
