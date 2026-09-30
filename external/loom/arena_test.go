package loom

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

func TestArenaPushBaseAndBundleLocalRepo(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	repo, err := gogit.PlainInit(source, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "readme.txt"), []byte("arena test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("readme.txt"); err != nil {
		t.Fatal(err)
	}
	want, err := wt.Commit("initial", &gogit.CommitOptions{Author: &object.Signature{Name: "agent-a", Email: "agent-a@example.test", When: time.Unix(1, 0)}})
	if err != nil {
		t.Fatal(err)
	}
	barePath := filepath.Join(root, "sprint-1", "example.git")
	if err := os.MkdirAll(filepath.Dir(barePath), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := gogit.PlainInit(barePath, true); err != nil {
		t.Fatal(err)
	}
	client := ArenaClient{URL: "file://" + root}
	if err := client.PushBase("sprint-1", "example", source, want.String()); err != nil {
		t.Fatalf("PushBase: %v", err)
	}
	// Re-pinning the same base (arena up after a partial pin) is a no-op, not an error.
	if err := client.PushBase("sprint-1", "example", source, want.String()); err != nil {
		t.Fatalf("PushBase again: %v", err)
	}
	bare, err := gogit.PlainOpen(barePath)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := bare.Reference(plumbing.NewBranchReferenceName("base"), true)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Hash() != want {
		t.Fatalf("base = %s, want %s", ref.Hash(), want)
	}
	bundlePath := filepath.Join(root, "archive", "example.bundle")
	if err := client.Bundle("sprint-1", "example", bundlePath); err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	wantHeader := "# v2 git bundle\n" + want.String() + " refs/heads/base\n\n"
	if !strings.HasPrefix(string(bundle), wantHeader) {
		t.Fatalf("bundle header = %q, want prefix %q", bundle[:min(len(bundle), len(wantHeader))], wantHeader)
	}
	if len(bundle) < len(wantHeader)+4 || string(bundle[len(wantHeader):len(wantHeader)+4]) != "PACK" {
		t.Fatal("bundle has no packfile")
	}
	objects := memory.NewStorage()
	if err := packfile.UpdateObjectStorage(objects, bytes.NewReader(bundle[len(wantHeader):])); err != nil {
		t.Fatalf("decode bundle packfile: %v", err)
	}
	if _, err := objects.EncodedObject(plumbing.CommitObject, want); err != nil {
		t.Fatalf("bundle missing base commit: %v", err)
	}
}
