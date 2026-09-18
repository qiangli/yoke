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

// setupTestRepo creates a temp directory with an initialized git repo and one commit.
func setupTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}

	// Create a file and commit it
	testFile := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(testFile, []byte("hello world\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	if _, err := wt.Add("hello.txt"); err != nil {
		t.Fatalf("add: %v", err)
	}

	_, err = wt.Commit("initial commit", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	return dir
}

func TestNativeRevParse_IsInsideWorkTree(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	result, err := nativeRevParse(context.Background(), dir, []string{"--is-inside-work-tree"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Stdout != "true\n" {
		t.Errorf("got %q, want %q", result.Stdout, "true\n")
	}
}

func TestNativeRevParse_ShowToplevel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	result, err := nativeRevParse(context.Background(), dir, []string{"--show-toplevel"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := dir
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		want = r // go-git reports the physical root (macOS /var → /private/var)
	}
	if got := strings.TrimSpace(result.Stdout); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNativeRevParse_AbbrevRefHEAD(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	result, err := nativeRevParse(context.Background(), dir, []string{"--abbrev-ref", "HEAD"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := strings.TrimSpace(result.Stdout)
	if got != "master" {
		t.Errorf("got %q, want %q", got, "master")
	}
}

func TestNativeRevParse_ShortHEAD(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	result, err := nativeRevParse(context.Background(), dir, []string{"--short", "HEAD"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := strings.TrimSpace(result.Stdout)
	if len(got) != 7 {
		t.Errorf("expected 7-char short hash, got %q (len=%d)", got, len(got))
	}
}

func TestNativeStatus_Clean(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	result, err := nativeStatus(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Stdout != "" {
		t.Errorf("expected empty status for clean repo, got %q", result.Stdout)
	}
}

func TestNativeStatus_Modified(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	// Modify the file
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("modified\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := nativeStatus(context.Background(), dir, []string{"--short"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Stdout, "hello.txt") {
		t.Errorf("expected hello.txt in status output, got %q", result.Stdout)
	}
}

func TestNativeLog_Oneline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	result, err := nativeLog(context.Background(), dir, []string{"--oneline", "-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Stdout, "initial commit") {
		t.Errorf("expected 'initial commit' in output, got %q", result.Stdout)
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 line, got %d", len(lines))
	}
}

func TestNativeLog_Default(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	result, err := nativeLog(context.Background(), dir, []string{"-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Stdout, "commit ") {
		t.Errorf("expected 'commit ' in output, got %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "Author: Test User") {
		t.Errorf("expected author in output, got %q", result.Stdout)
	}
}

func TestNativeAdd_And_Commit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	// Create a new file
	newFile := filepath.Join(dir, "new.txt")
	if err := os.WriteFile(newFile, []byte("new content\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Add it
	result, err := nativeAdd(context.Background(), dir, []string{"new.txt"})
	if err != nil {
		t.Fatalf("add error: %v", err)
	}

	// Commit it
	result, err = nativeCommit(context.Background(), dir, []string{"-m", "add new file"})
	if err != nil {
		t.Fatalf("commit error: %v", err)
	}
	if !strings.Contains(result.Stdout, "add new file") {
		t.Errorf("expected commit message in output, got %q", result.Stdout)
	}
}

func TestNativeBranch_List(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	result, err := nativeBranch(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("branch error: %v", err)
	}
	if !strings.Contains(result.Stdout, "master") {
		t.Errorf("expected 'master' in branch list, got %q", result.Stdout)
	}
}

func TestNativeBranch_CreateAndCheckout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	// Create branch
	result, err := nativeBranch(context.Background(), dir, []string{"feature"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	// Checkout
	result, err = nativeCheckout(context.Background(), dir, []string{"feature"})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if !strings.Contains(result.Stdout, "feature") {
		t.Errorf("expected 'feature' in output, got %q", result.Stdout)
	}

	// Verify HEAD
	result, err = nativeRevParse(context.Background(), dir, []string{"--abbrev-ref", "HEAD"})
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != "feature" {
		t.Errorf("expected HEAD on 'feature', got %q", result.Stdout)
	}
}

func TestNativeCheckout_CreateBranch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	result, err := nativeCheckout(context.Background(), dir, []string{"-b", "new-branch"})
	if err != nil {
		t.Fatalf("checkout -b: %v", err)
	}
	if !strings.Contains(result.Stdout, "new-branch") {
		t.Errorf("expected 'new-branch' in output, got %q", result.Stdout)
	}
}

func TestNativeRemote_GetURL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	// Add a remote
	repo, _ := gogit.PlainOpen(dir)
	_, err := repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{"https://github.com/test/repo.git"},
	})
	if err != nil {
		t.Fatalf("create remote: %v", err)
	}

	result, err := nativeRemote(context.Background(), dir, []string{"get-url", "origin"})
	if err != nil {
		t.Fatalf("remote get-url: %v", err)
	}
	expected := "https://github.com/test/repo.git\n"
	if result.Stdout != expected {
		t.Errorf("got %q, want %q", result.Stdout, expected)
	}
}

func TestNativeStash_ReturnsNotImplemented(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	_, err := nativeStash(context.Background(), "", nil)
	if err != ErrUnsupported {
		t.Errorf("expected ErrUnsupported, got %v", err)
	}
}

func TestNativeMerge_FastForward(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	// Create a feature branch and add a commit
	repo, _ := gogit.PlainOpen(dir)
	wt, _ := repo.Worktree()

	err := wt.Checkout(&gogit.CheckoutOptions{
		Branch: "refs/heads/feature",
		Create: true,
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	wt.Add("feature.txt")
	wt.Commit("feature commit", &gogit.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "t@t.com", When: time.Now()},
	})

	// Switch back to master
	err = wt.Checkout(&gogit.CheckoutOptions{
		Branch: "refs/heads/master",
	})
	if err != nil {
		t.Fatalf("checkout master: %v", err)
	}

	// Merge feature into master (fast-forward)
	result, err := nativeMerge(context.Background(), dir, []string{"feature"})
	if err != nil {
		t.Fatalf("merge error: %v", err)
	}
	if !strings.Contains(result.Stdout, "Fast-forward") {
		t.Errorf("expected fast-forward in output, got %q", result.Stdout)
	}

	// Verify feature.txt exists after merge
	if _, err := os.Stat(filepath.Join(dir, "feature.txt")); os.IsNotExist(err) {
		t.Error("feature.txt should exist after merge")
	}
}

func TestNativeReset(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	// Create and stage a new file
	newFile := filepath.Join(dir, "staged.txt")
	if err := os.WriteFile(newFile, []byte("staged content\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	repo, _ := gogit.PlainOpen(dir)
	wt, _ := repo.Worktree()
	if _, err := wt.Add("staged.txt"); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Verify it's staged (Added = 65 = 'A')
	status, _ := wt.Status()
	if status.File("staged.txt").Staging != gogit.Added {
		t.Fatalf("file should be staged (Added) before reset, got %v", status.File("staged.txt").Staging)
	}

	// Reset (unstage all)
	if _, err := nativeReset(context.Background(), dir, []string{"HEAD"}); err != nil {
		t.Fatalf("reset error: %v", err)
	}

	// Verify file is no longer staged as Added.
	// After reset, a newly added file (not in HEAD) becomes untracked ('?').
	status, _ = wt.Status()
	fs := status.File("staged.txt")
	if fs.Staging == gogit.Added {
		t.Errorf("file should not be staged as Added after reset, got staging=%v", fs.Staging)
	}
}

func TestNativeShow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	// Get HEAD hash
	repo, _ := gogit.PlainOpen(dir)
	head, _ := repo.Head()
	hash := head.Hash().String()

	result, err := nativeShow(context.Background(), dir, []string{hash})
	if err != nil {
		t.Fatalf("show error: %v", err)
	}
	if !strings.Contains(result.Stdout, "commit "+hash) {
		t.Errorf("expected commit hash in output, got %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "Author: Test User") {
		t.Errorf("expected author in output, got %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "initial commit") {
		t.Errorf("expected commit message in output, got %q", result.Stdout)
	}
	// Should include the patch for the initial commit
	if !strings.Contains(result.Stdout, "hello") {
		t.Errorf("expected patch content in output, got %q", result.Stdout)
	}
}

func TestNativeLogSince(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := t.TempDir()

	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}

	wt, _ := repo.Worktree()

	// Create an old commit (2 days ago)
	oldFile := filepath.Join(dir, "old.txt")
	if err := os.WriteFile(oldFile, []byte("old\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	wt.Add("old.txt")
	oldTime := time.Now().Add(-48 * time.Hour)
	wt.Commit("old commit", &gogit.CommitOptions{
		Author:    &object.Signature{Name: "Test", Email: "t@t.com", When: oldTime},
		Committer: &object.Signature{Name: "Test", Email: "t@t.com", When: oldTime},
	})

	// Create a recent commit (now)
	newFile := filepath.Join(dir, "new.txt")
	if err := os.WriteFile(newFile, []byte("new\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	wt.Add("new.txt")
	now := time.Now()
	wt.Commit("recent commit", &gogit.CommitOptions{
		Author:    &object.Signature{Name: "Test", Email: "t@t.com", When: now},
		Committer: &object.Signature{Name: "Test", Email: "t@t.com", When: now},
	})

	// Log with --since=1 day ago (should only show recent commit)
	sinceDate := time.Now().Add(-24 * time.Hour).Format("2006-01-02")
	result, err := nativeLog(context.Background(), dir, []string{"--oneline", "--since=" + sinceDate})
	if err != nil {
		t.Fatalf("log error: %v", err)
	}
	if !strings.Contains(result.Stdout, "recent commit") {
		t.Errorf("expected 'recent commit' in output, got %q", result.Stdout)
	}
	if strings.Contains(result.Stdout, "old commit") {
		t.Errorf("should not contain 'old commit', got %q", result.Stdout)
	}
}

func TestNativeBranchListAll(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	// Create a second branch
	repo, _ := gogit.PlainOpen(dir)
	head, _ := repo.Head()
	ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature"), head.Hash())
	_ = repo.Storer.SetReference(ref)

	// List with -a (all branches)
	result, err := nativeBranch(context.Background(), dir, []string{"-a"})
	if err != nil {
		t.Fatalf("branch -a error: %v", err)
	}
	if !strings.Contains(result.Stdout, "master") {
		t.Errorf("expected 'master' in output, got %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "feature") {
		t.Errorf("expected 'feature' in output, got %q", result.Stdout)
	}
}

func TestNativeBranchContains(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	// Get HEAD hash (the initial commit)
	repo, _ := gogit.PlainOpen(dir)
	head, _ := repo.Head()
	commitHash := head.Hash().String()[:7]

	// Create a second branch at HEAD
	ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature"), head.Hash())
	_ = repo.Storer.SetReference(ref)

	// --contains should show both branches
	result, err := nativeBranch(context.Background(), dir, []string{"--contains", commitHash})
	if err != nil {
		t.Fatalf("branch --contains error: %v", err)
	}
	if !strings.Contains(result.Stdout, "master") {
		t.Errorf("expected 'master' in output, got %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "feature") {
		t.Errorf("expected 'feature' in output, got %q", result.Stdout)
	}
}

func TestNativeMergeBase(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	// Create a second commit on a branch
	repo, _ := gogit.PlainOpen(dir)
	wt, _ := repo.Worktree()

	// Get current HEAD hash (will be the merge base)
	head, _ := repo.Head()
	baseHash := head.Hash().String()

	// Create branch and add a commit
	err := wt.Checkout(&gogit.CheckoutOptions{
		Branch: "refs/heads/feature",
		Create: true,
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	wt.Add("feature.txt")
	wt.Commit("feature commit", &gogit.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "t@t.com", When: time.Now()},
	})

	// merge-base between master and feature should be the initial commit
	result, err := nativeMergeBase(context.Background(), dir, []string{"master", "feature"})
	if err != nil {
		t.Fatalf("merge-base: %v", err)
	}
	got := strings.TrimSpace(result.Stdout)
	if got != baseHash {
		t.Errorf("got %q, want %q", got, baseHash)
	}
}

func TestNativeTag(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	// Create a lightweight tag
	result, err := nativeTag(context.Background(), dir, []string{"v1.0"})
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}

	// List tags
	result, err = nativeTag(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	if !strings.Contains(result.Stdout, "v1.0") {
		t.Errorf("expected 'v1.0' in tag list, got %q", result.Stdout)
	}

	// Create annotated tag
	result, err = nativeTag(context.Background(), dir, []string{"-a", "v2.0", "-m", "release 2.0"})
	if err != nil {
		t.Fatalf("create annotated tag: %v", err)
	}

	// List all tags
	result, err = nativeTag(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	if !strings.Contains(result.Stdout, "v2.0") {
		t.Errorf("expected 'v2.0' in tag list, got %q", result.Stdout)
	}

	// Delete tag
	result, err = nativeTag(context.Background(), dir, []string{"-d", "v1.0"})
	if err != nil {
		t.Fatalf("delete tag: %v", err)
	}
	if !strings.Contains(result.Stdout, "Deleted") {
		t.Errorf("expected 'Deleted' in output, got %q", result.Stdout)
	}

	// Verify deleted
	result, err = nativeTag(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	if strings.Contains(result.Stdout, "v1.0") {
		t.Errorf("tag 'v1.0' should have been deleted, got %q", result.Stdout)
	}
}

func TestNativeFetch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	// Add a remote
	repo, _ := gogit.PlainOpen(dir)
	_, err := repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{dir}, // point to self for testing
	})
	if err != nil {
		t.Fatalf("create remote: %v", err)
	}

	// Fetch from origin — should succeed (already up to date)
	if _, err := nativeFetch(context.Background(), dir, nil); err != nil {
		t.Fatalf("fetch error: %v", err)
	}
}

func TestNativeGrep(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	// Search for "hello" in tracked files
	result, err := nativeGrep(context.Background(), dir, []string{"hello"})
	if err != nil {
		t.Fatalf("grep error: %v", err)
	}
	if !strings.Contains(result.Stdout, "hello.txt:1:hello world") {
		t.Errorf("expected match in output, got %q", result.Stdout)
	}

	// Case insensitive search
	result, err = nativeGrep(context.Background(), dir, []string{"-i", "HELLO"})
	if err != nil {
		t.Fatalf("grep -i error: %v", err)
	}
	if !strings.Contains(result.Stdout, "hello.txt") {
		t.Errorf("expected match for case insensitive, got %q", result.Stdout)
	}

	// Files only
	result, err = nativeGrep(context.Background(), dir, []string{"-l", "hello"})
	if err != nil {
		t.Fatalf("grep -l error: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != "hello.txt" {
		t.Errorf("expected just filename, got %q", result.Stdout)
	}

	// No match
	result, err = nativeGrep(context.Background(), dir, []string{"nonexistent_pattern_xyz"})
	if err != nil {
		t.Fatalf("grep no match error: %v", err)
	}
	if result.ExitCode != 1 {
		t.Errorf("expected exit code 1 for no match, got %d", result.ExitCode)
	}
}

func TestNativeLsFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	// List tracked files
	result, err := nativeLsFiles(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("ls-files error: %v", err)
	}
	if !strings.Contains(result.Stdout, "hello.txt") {
		t.Errorf("expected 'hello.txt' in output, got %q", result.Stdout)
	}

	// Create an untracked file
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// --others should show untracked
	result, err = nativeLsFiles(context.Background(), dir, []string{"--others"})
	if err != nil {
		t.Fatalf("ls-files --others error: %v", err)
	}
	if !strings.Contains(result.Stdout, "untracked.txt") {
		t.Errorf("expected 'untracked.txt' in --others output, got %q", result.Stdout)
	}

	// Modify tracked file and check --modified
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("modified\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	result, err = nativeLsFiles(context.Background(), dir, []string{"--modified"})
	if err != nil {
		t.Fatalf("ls-files --modified error: %v", err)
	}
	if !strings.Contains(result.Stdout, "hello.txt") {
		t.Errorf("expected 'hello.txt' in --modified output, got %q", result.Stdout)
	}
}

func TestNativeBlame(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	dir := setupTestRepo(t)

	result, err := nativeBlame(context.Background(), dir, []string{"hello.txt"})
	if err != nil {
		t.Fatalf("blame error: %v", err)
	}
	// Should contain author email (from Line.Author field)
	if !strings.Contains(result.Stdout, "test@example.com") {
		t.Errorf("expected author in blame output, got %q", result.Stdout)
	}
	// Should contain line content
	if !strings.Contains(result.Stdout, "hello world") {
		t.Errorf("expected file content in blame output, got %q", result.Stdout)
	}
	// Should contain line number
	if !strings.Contains(result.Stdout, "1)") {
		t.Errorf("expected line number in blame output, got %q", result.Stdout)
	}
}
