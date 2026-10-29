package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	yokegit "github.com/qiangli/yoke/git"
)

// TestS525A7PreviewBase pins the preview seam: `weave start` offers --base,
// an explicit base commit must exist in the source repo, and resolving it
// never moves anything (the caller only reads).
func TestS525A7PreviewBase(t *testing.T) {
	if f := newWeaveStartCmd().Flags().Lookup("base"); f == nil {
		t.Fatal("weave start has no --base flag")
	}
	root := t.TempDir()
	if _, err := yokegit.Init(yokegit.InitOptions{Path: root}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := yokegit.Add(yokegit.AddOptions{RepoPath: root, All: true}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := yokegit.Commit(yokegit.CommitOptions{
		RepoPath: root, Message: "first",
		AuthorName: "s525a7", AuthorEmail: "s525a7@example.com",
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	head, err := yokegit.ResolveCommit(root, "HEAD")
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	if got, err := weaveBaseSHAForStart(root, head); err != nil || got != head {
		t.Fatalf("base HEAD = %q, %v; want %s", got, err, head)
	}
	short := head[:12]
	if got, err := weaveBaseSHAForStart(root, short); err != nil || got != head {
		t.Fatalf("base short = %q, %v; want %s", got, err, head)
	}
	if _, err := weaveBaseSHAForStart(root, strings.Repeat("0", 40)); err == nil {
		t.Fatal("unknown --base commit = nil error, want refusal")
	}
	if _, err := weaveBaseSHAForStart(root, "no-such-branch"); err == nil {
		t.Fatal("unknown --base name = nil error, want refusal")
	}
	// Resolving the base is read-only: nothing moved.
	if got, err := yokegit.ResolveCommit(root, "HEAD"); err != nil || got != head {
		t.Fatalf("HEAD after base resolution = %s, %v; want %s", got, err, head)
	}
}
