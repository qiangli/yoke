package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestNativePush(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	// Create a bare remote repo
	remoteDir := t.TempDir()
	_, err := gogit.PlainInit(remoteDir, true)
	if err != nil {
		t.Fatalf("init bare: %v", err)
	}

	// Create local repo with a commit
	localDir := setupTestRepo(t)
	repo, err := gogit.PlainOpen(localDir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Add remote
	_, err = repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{remoteDir},
	})
	if err != nil {
		t.Fatalf("create remote: %v", err)
	}

	// Push current branch (no args)
	result, err := nativePush(context.Background(), localDir, nil)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("push exit code: %d, stderr: %s", result.ExitCode, result.Stderr)
	}

	// Push again should say up-to-date
	result, err = nativePush(context.Background(), localDir, nil)
	if err != nil {
		t.Fatalf("push again: %v", err)
	}
	if !strings.Contains(result.Stdout, "up-to-date") {
		t.Errorf("expected up-to-date, got %q", result.Stdout)
	}
}

func TestNativeCatFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	dir := setupTestRepo(t)
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}

	commitHash := head.Hash().String()

	// Test -t (type)
	result, err := nativeCatFile(context.Background(), dir, []string{"-t", commitHash})
	if err != nil {
		t.Fatalf("cat-file -t: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != "commit" {
		t.Errorf("expected type 'commit', got %q", result.Stdout)
	}

	// Test -s (size)
	result, err = nativeCatFile(context.Background(), dir, []string{"-s", commitHash})
	if err != nil {
		t.Fatalf("cat-file -s: %v", err)
	}
	size := strings.TrimSpace(result.Stdout)
	if size == "" || size == "0" {
		t.Errorf("expected non-zero size, got %q", size)
	}

	// Test -p (pretty-print commit)
	result, err = nativeCatFile(context.Background(), dir, []string{"-p", commitHash})
	if err != nil {
		t.Fatalf("cat-file -p: %v", err)
	}
	if !strings.Contains(result.Stdout, "tree ") {
		t.Errorf("expected 'tree ' in output, got %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "initial commit") {
		t.Errorf("expected commit message in output, got %q", result.Stdout)
	}

	// Test -p on a blob (find the blob hash from the tree)
	commit, _ := repo.CommitObject(head.Hash())
	tree, _ := commit.Tree()
	blobHash := ""
	for _, entry := range tree.Entries {
		if entry.Name == "hello.txt" {
			blobHash = entry.Hash.String()
			break
		}
	}
	if blobHash != "" {
		result, err = nativeCatFile(context.Background(), dir, []string{"-p", blobHash})
		if err != nil {
			t.Fatalf("cat-file -p blob: %v", err)
		}
		if result.Stdout != "hello world\n" {
			t.Errorf("expected blob content 'hello world\\n', got %q", result.Stdout)
		}

		result, err = nativeCatFile(context.Background(), dir, []string{"-t", blobHash})
		if err != nil {
			t.Fatalf("cat-file -t blob: %v", err)
		}
		if strings.TrimSpace(result.Stdout) != "blob" {
			t.Errorf("expected type 'blob', got %q", result.Stdout)
		}
	}
}

func TestNativeLsTree(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	dir := setupTestRepo(t)

	result, err := nativeLsTree(context.Background(), dir, []string{"HEAD"})
	if err != nil {
		t.Fatalf("ls-tree: %v", err)
	}
	if !strings.Contains(result.Stdout, "hello.txt") {
		t.Errorf("expected hello.txt in output, got %q", result.Stdout)
	}
	// Check format: mode type hash\tname
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) == 0 {
		t.Fatal("no output from ls-tree")
	}
	parts := strings.SplitN(lines[0], "\t", 2)
	if len(parts) != 2 {
		t.Errorf("expected tab-separated output, got %q", lines[0])
	}
	fields := strings.Fields(parts[0])
	if len(fields) != 3 {
		t.Errorf("expected 3 fields before tab (mode type hash), got %d: %q", len(fields), parts[0])
	}
}

func TestNativeLsTree_Recursive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Create nested files
	subDir := filepath.Join(dir, "sub")
	os.MkdirAll(subDir, 0755)
	os.WriteFile(filepath.Join(dir, "root.txt"), []byte("root\n"), 0644)
	os.WriteFile(filepath.Join(subDir, "nested.txt"), []byte("nested\n"), 0644)

	wt, _ := repo.Worktree()
	wt.Add("root.txt")
	wt.Add("sub/nested.txt")
	wt.Commit("add files", &gogit.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "t@t.com", When: time.Now()},
	})

	result, err := nativeLsTree(context.Background(), dir, []string{"-r", "HEAD"})
	if err != nil {
		t.Fatalf("ls-tree -r: %v", err)
	}
	if !strings.Contains(result.Stdout, "root.txt") {
		t.Errorf("expected root.txt, got %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "sub/nested.txt") {
		t.Errorf("expected sub/nested.txt, got %q", result.Stdout)
	}
}

func TestNativeShowRef(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	dir := setupTestRepo(t)
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Create a tag
	head, _ := repo.Head()
	tagRef := plumbing.NewHashReference(plumbing.NewTagReferenceName("v1.0"), head.Hash())
	repo.Storer.SetReference(tagRef)

	// List all refs
	result, err := nativeShowRef(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("show-ref: %v", err)
	}
	if result.Stdout == "" {
		t.Error("expected non-empty output")
	}
	if !strings.Contains(result.Stdout, "refs/tags/v1.0") {
		t.Errorf("expected tag ref in output, got %q", result.Stdout)
	}

	// --heads only
	result, err = nativeShowRef(context.Background(), dir, []string{"--heads"})
	if err != nil {
		t.Fatalf("show-ref --heads: %v", err)
	}
	if strings.Contains(result.Stdout, "refs/tags") {
		t.Errorf("--heads should not show tags, got %q", result.Stdout)
	}

	// --tags only
	result, err = nativeShowRef(context.Background(), dir, []string{"--tags"})
	if err != nil {
		t.Fatalf("show-ref --tags: %v", err)
	}
	if !strings.Contains(result.Stdout, "refs/tags/v1.0") {
		t.Errorf("--tags should show tags, got %q", result.Stdout)
	}
}

func TestNativeRm(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	dir := setupTestRepo(t)

	// Remove hello.txt
	result, err := nativeRm(context.Background(), dir, []string{"hello.txt"})
	if err != nil {
		t.Fatalf("rm: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("rm exit code: %d", result.ExitCode)
	}

	// File should be gone from working tree
	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); !os.IsNotExist(err) {
		t.Error("expected file to be removed from working tree")
	}

	// Status should show deletion
	result, err = nativeStatus(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(result.Stdout, "hello.txt") {
		t.Errorf("expected hello.txt in status, got %q", result.Stdout)
	}
}

func TestNativeRm_Cached(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	dir := setupTestRepo(t)

	// Remove hello.txt --cached (keep in working tree)
	result, err := nativeRm(context.Background(), dir, []string{"--cached", "hello.txt"})
	if err != nil {
		t.Fatalf("rm --cached: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("rm exit code: %d", result.ExitCode)
	}

	// File should still exist in working tree
	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); os.IsNotExist(err) {
		t.Error("expected file to remain in working tree with --cached")
	}
}

func TestNativeSymbolicRef(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	dir := setupTestRepo(t)

	// Read HEAD
	result, err := nativeSymbolicRef(context.Background(), dir, []string{"HEAD"})
	if err != nil {
		t.Fatalf("symbolic-ref HEAD: %v", err)
	}
	if !strings.Contains(result.Stdout, "refs/heads/") {
		t.Errorf("expected refs/heads/ in output, got %q", result.Stdout)
	}
}

func TestNativeUpdateRef(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	dir := setupTestRepo(t)
	repo, _ := gogit.PlainOpen(dir)
	head, _ := repo.Head()

	// Create a new ref
	refName := "refs/heads/test-branch"
	result, err := nativeUpdateRef(context.Background(), dir, []string{refName, head.Hash().String()})
	if err != nil {
		t.Fatalf("update-ref: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code: %d", result.ExitCode)
	}

	// Verify it exists
	ref, err := repo.Storer.Reference(plumbing.ReferenceName(refName))
	if err != nil {
		t.Fatalf("reference not found: %v", err)
	}
	if ref.Hash() != head.Hash() {
		t.Errorf("hash mismatch: %s != %s", ref.Hash(), head.Hash())
	}

	// Delete the ref
	result, err = nativeUpdateRef(context.Background(), dir, []string{"-d", refName})
	if err != nil {
		t.Fatalf("update-ref -d: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("delete exit code: %d", result.ExitCode)
	}
}

func TestNativeDiffTree(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	// Create a repo with two commits
	dir := t.TempDir()
	repo, _ := gogit.PlainInit(dir, false)
	wt, _ := repo.Worktree()

	// First commit
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("aaa\n"), 0644)
	wt.Add("a.txt")
	hash1, _ := wt.Commit("first", &gogit.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@t.com", When: time.Now()},
	})

	// Second commit: modify a.txt and add b.txt
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("modified\n"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("bbb\n"), 0644)
	wt.Add("a.txt")
	wt.Add("b.txt")
	hash2, _ := wt.Commit("second", &gogit.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@t.com", When: time.Now()},
	})

	result, err := nativeDiffTree(context.Background(), dir, []string{hash1.String(), hash2.String()})
	if err != nil {
		t.Fatalf("diff-tree: %v", err)
	}
	if !strings.Contains(result.Stdout, "a.txt") {
		t.Errorf("expected a.txt in diff, got %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "b.txt") {
		t.Errorf("expected b.txt in diff, got %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "M") {
		t.Errorf("expected M status, got %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "A") {
		t.Errorf("expected A status, got %q", result.Stdout)
	}
}

func TestNativeHashObject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	dir := setupTestRepo(t)

	// Create a file to hash
	testFile := filepath.Join(dir, "hashme.txt")
	os.WriteFile(testFile, []byte("hash this content\n"), 0644)

	result, err := nativeHashObject(context.Background(), dir, []string{"-w", "hashme.txt"})
	if err != nil {
		t.Fatalf("hash-object: %v", err)
	}
	hash := strings.TrimSpace(result.Stdout)
	if len(hash) != 40 {
		t.Errorf("expected 40-char hash, got %q (len=%d)", hash, len(hash))
	}

	// Verify the object exists via cat-file
	result, err = nativeCatFile(context.Background(), dir, []string{"-t", hash})
	if err != nil {
		t.Fatalf("cat-file verify: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != "blob" {
		t.Errorf("expected blob type, got %q", result.Stdout)
	}
}

func TestNativeFormatPatch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	dir := setupTestRepo(t)

	result, err := nativeFormatPatch(context.Background(), dir, []string{"-1", "HEAD"})
	if err != nil {
		t.Fatalf("format-patch: %v", err)
	}
	if !strings.Contains(result.Stdout, "From ") {
		t.Errorf("expected 'From ' header, got %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "Subject: [PATCH]") {
		t.Errorf("expected Subject header, got %q", result.Stdout)
	}
}

func TestNativeStash_NotImplemented(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	dir := setupTestRepo(t)

	// All stash operations should return ErrUnsupported
	_, err := nativeStash(context.Background(), dir, nil)
	if err != ErrUnsupported {
		t.Errorf("expected ErrUnsupported for stash, got %v", err)
	}

	_, err = nativeStash(context.Background(), dir, []string{"list"})
	if err != ErrUnsupported {
		t.Errorf("expected ErrUnsupported for stash list, got %v", err)
	}
}

func TestNativeRebase_NotImplemented_Interactive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	dir := setupTestRepo(t)

	_, err := nativeRebase(context.Background(), dir, []string{"-i", "HEAD~1"})
	if err != ErrUnsupported {
		t.Errorf("expected ErrUnsupported for interactive rebase, got %v", err)
	}
}
