package git

import (
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
)

// A submodule checkout (and a linked worktree) has a .git FILE holding
// "gitdir: <path>", not a .git directory. The in-process file transport
// must follow it, or every local clone of a submodule checkout fails
// with "repository not found" (sprint arena up hit this on cloudbox).
func TestFileTransportFollowsGitdirFile(t *testing.T) {
	isolateGlobalConfig(t)
	src := makeTwoCommitRepo(t)
	root := t.TempDir()
	moved := filepath.Join(root, "modules", "sub")
	if err := os.MkdirAll(filepath.Dir(moved), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(src, ".git"), moved); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(src, moved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".git"), []byte("gitdir: "+rel+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := gogit.PlainClone(dst, true, &gogit.CloneOptions{URL: src, NoCheckout: true}); err != nil {
		t.Fatalf("clone of a gitdir-file checkout: %v", err)
	}
}
