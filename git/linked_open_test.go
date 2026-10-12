package git

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Opening a linked worktree must not leak the commondir file: go-git's
// EnableDotGitCommonDir does, and on Windows the open handle stops
// `worktree remove` from deleting the admin directory.
func TestOpenLinkedWorktreeLeaksNoFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /dev/fd; TestNativeWorktree_AddRemoveList covers Windows")
	}
	if _, err := os.ReadDir("/dev/fd"); err != nil {
		t.Skip("no /dev/fd")
	}
	ctx := context.Background()
	dir := makeTwoCommitRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	if _, err := Exec(ctx, dir, []string{"worktree", "add", wt}); err != nil {
		t.Fatalf("worktree add: %v", err)
	}
	if _, err := openRepo(wt); err != nil {
		t.Fatalf("open linked worktree: %v", err)
	}
	fds := func() int {
		ents, _ := os.ReadDir("/dev/fd")
		return len(ents)
	}
	before := fds()
	for i := 0; i < 50; i++ {
		r, err := openRepo(wt)
		if err != nil {
			t.Fatalf("open linked worktree: %v", err)
		}
		if _, err := r.Head(); err != nil {
			t.Fatalf("head through commondir: %v", err)
		}
	}
	if after := fds(); after > before+5 {
		t.Errorf("open file descriptors grew %d -> %d over 50 opens of a linked worktree", before, after)
	}
}
