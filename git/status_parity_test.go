package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func commitAll(t *testing.T, repo *gogit.Repository, msg string) plumbing.Hash {
	t.Helper()
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	h, err := wt.Commit(msg, &gogit.CommitOptions{
		All:    true,
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	return h
}

// setupSuperproject builds top/ with a gitlink "sub" recorded at the
// submodule's first commit, then advances the submodule checkout one
// commit. .git/config carries no submodule.sub entry — the shape of a
// superproject whose submodules were cloned without `submodule init`.
func setupSuperproject(t *testing.T) string {
	t.Helper()
	top := setupTestRepo(t)
	subDir := filepath.Join(top, "sub")
	sub, err := gogit.PlainInit(subDir, false)
	if err != nil {
		t.Fatalf("init sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "a"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	swt, _ := sub.Worktree()
	if _, err := swt.Add("a"); err != nil {
		t.Fatal(err)
	}
	first := commitAll(t, sub, "a")

	if err := os.WriteFile(filepath.Join(top, ".gitmodules"),
		[]byte("[submodule \"sub\"]\n\tpath = sub\n\turl = https://example.com/sub.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := gogit.PlainOpen(top)
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	if _, err := wt.Add(".gitmodules"); err != nil {
		t.Fatal(err)
	}
	idx, err := repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	idx.Entries = append(idx.Entries, &index.Entry{Name: "sub", Mode: filemode.Submodule, Hash: first})
	if err := repo.Storer.SetIndex(idx); err != nil {
		t.Fatal(err)
	}
	commitAll(t, repo, "add sub")

	if err := os.WriteFile(filepath.Join(subDir, "b"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := swt.Add("b"); err != nil {
		t.Fatal(err)
	}
	commitAll(t, sub, "b")
	return top
}

func TestNativeStatus_MovedSubmoduleWithoutConfigEntry(t *testing.T) {
	top := setupSuperproject(t)
	res, err := nativeStatus(context.Background(), top, []string{"--short"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if res.Stdout != " M sub\n" {
		t.Errorf("status = %q, want %q (real git reports the moved gitlink)", res.Stdout, " M sub\n")
	}
	_, entries, err := Status(top)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(entries) != 1 || entries[0].File != "sub" || entries[0].Staged {
		t.Errorf("Status entries = %+v, want one unstaged sub", entries)
	}
}

func TestNativeStatus_HonorsInfoExclude(t *testing.T) {
	dir := setupTestRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "exclude"), []byte("# local\nsecret.json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := nativeStatus(context.Background(), dir, []string{"--short"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if strings.Contains(res.Stdout, "secret.json") {
		t.Errorf("status = %q; secret.json is excluded by .git/info/exclude", res.Stdout)
	}
}

func TestNativeClean_KeepsInfoExcludedFiles(t *testing.T) {
	dir := setupTestRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "exclude"), []byte("secret.json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"secret.json", "junk.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := nativeClean(context.Background(), dir, []string{"-f"}); err != nil {
		t.Fatalf("clean: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "secret.json")); err != nil {
		t.Errorf("clean -f removed an info/exclude'd file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "junk.txt")); !os.IsNotExist(err) {
		t.Errorf("clean -f kept untracked junk.txt (err=%v)", err)
	}
}
