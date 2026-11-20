package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression for story 4c60ebe8: the engine parsed --allow-empty as an
// accepted no-op but never set go-git's AllowEmptyCommits, so the worker
// form `git -c user.email=.. -c user.name=.. commit -q --allow-empty -m x`
// failed on a clean tree. The flag must be honored, and a clean-tree
// commit WITHOUT it must still refuse (host git behavior).
func TestNativeCommitAllowEmpty(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	if _, err := Exec(ctx, dir, []string{"init", "-q", "."}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Exec(ctx, dir, []string{"add", "."}); err != nil {
		t.Fatalf("add: %v", err)
	}
	id := []string{"-c", "user.email=w@example.invalid", "-c", "user.name=worker"}
	seed := append(append([]string{}, id...), "commit", "-qm", "seed")
	if _, err := Exec(ctx, dir, seed); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	// Clean tree, no --allow-empty: refuse, like host git.
	noFlag := append(append([]string{}, id...), "commit", "-qm", "must refuse")
	if _, err := Exec(ctx, dir, noFlag); err == nil {
		t.Fatal("clean-tree commit without --allow-empty succeeded, want error")
	}

	// The full worker form must create the empty commit, quietly.
	emptyOK := append(append([]string{}, id...), "commit", "-q", "--allow-empty", "-m", "empty one")
	res, err := Exec(ctx, dir, emptyOK)
	if err != nil {
		t.Fatalf("commit -q --allow-empty: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("commit -q --allow-empty exit = %d, stderr = %s", res.ExitCode, res.Stderr)
	}
	if res.Stdout != "" {
		t.Errorf("-q stdout = %q, want empty", res.Stdout)
	}

	log, err := Exec(ctx, dir, []string{"log", "--oneline"})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(log.Stdout), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "empty one") {
		t.Errorf("log after --allow-empty = %q, want newest commit 'empty one' over 'seed'", log.Stdout)
	}
}

func TestTypedCommitAllowEmpty(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(InitOptions{Path: dir, Quiet: true}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(AddOptions{RepoPath: dir, All: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	base := CommitOptions{
		RepoPath:    dir,
		AuthorName:  "worker",
		AuthorEmail: "w@example.invalid",
	}
	seed := base
	seed.Message = "seed"
	if _, err := Commit(seed); err != nil {
		t.Fatalf("seed Commit: %v", err)
	}

	refuse := base
	refuse.Message = "must refuse"
	if _, err := Commit(refuse); err == nil {
		t.Fatal("typed clean-tree Commit without AllowEmpty succeeded, want error")
	}

	ok := base
	ok.Message = "empty one"
	ok.AllowEmpty = true
	if _, err := Commit(ok); err != nil {
		t.Fatalf("typed Commit with AllowEmpty: %v", err)
	}
}
