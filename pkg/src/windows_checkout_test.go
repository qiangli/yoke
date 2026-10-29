package src_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/yoke/pkg/src"
)

// executableSeed records an executable bit in Git even on a Windows filesystem,
// where the checkout cannot represent that bit. This is the shape of upstream
// scripts in a native Windows clone with core.filemode=false.
func executableSeed(t *testing.T, home string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "script.sh"), []byte("echo ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := gogit.PlainInit(work, false)
	if err != nil {
		t.Fatal(err)
	}
	w, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Add("script.sh"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	idx, err := r.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range idx.Entries {
		if e.Name == "script.sh" {
			e.Mode = filemode.Executable
		}
	}
	if err := r.Storer.SetIndex(idx); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit("base", &gogit.CommitOptions{Author: &object.Signature{Name: "src-test", Email: "src-test@example.com", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	head, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(home, "seed", "yoke.git")
	if err := os.MkdirAll(filepath.Dir(seed), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := gogit.PlainClone(seed, true, &gogit.CloneOptions{URL: work}); err != nil {
		t.Fatal(err)
	}
	return head.Hash().String()
}

func TestWindowsFetchExecutableSeedAndRepeat(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows file mode regression")
	}
	home := t.TempDir()
	sha := executableSeed(t, home)
	t.Setenv("BASHY_SRC_COMMIT", sha)
	if _, err := src.Fetch(home, "yoke"); err != nil {
		t.Fatalf("fetch published commit %s: %v", sha, err)
	}
	if _, err := src.Fetch(home, "yoke"); err != nil {
		t.Fatalf("repeat fetch of clean checkout: %v", err)
	}
}

func TestWindowsFetchExistingNativeClone(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows file mode regression")
	}
	home := t.TempDir()
	sha := executableSeed(t, home)
	t.Setenv("BASHY_SRC_COMMIT", sha)
	cloneWindowsFixture(t, home)
	if _, err := src.Fetch(home, "yoke"); err != nil {
		t.Fatalf("fetch existing clean Windows clone: %v", err)
	}
}

func cloneWindowsFixture(t *testing.T, home string) (*gogit.Repository, string) {
	t.Helper()
	dest := destOf(home)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := gogit.PlainClone(dest, false, &gogit.CloneOptions{URL: filepath.Join(home, "seed", "yoke.git")}); err != nil {
		t.Fatal(err)
	}
	r, err := gogit.PlainOpen(dest)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := r.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Raw.Section("core").SetOption("filemode", "false")
	if err := r.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return r, dest
}

func TestWindowsPRMergeExecutableCheckout(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows file mode regression")
	}
	home := t.TempDir()
	executableSeed(t, home)
	r, dir := cloneWindowsFixture(t, home)
	head, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("local"), head.Hash())); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	tip, err := w.Commit("change README", &gogit.CommitOptions{Author: &object.Signature{Name: "src-test", Email: "src-test@example.com", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("topic"), tip)); err != nil {
		t.Fatal(err)
	}
	reviewWaiver(t, home)
	pr, err := src.OpenPR(home, "yoke", "topic", "change README", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.MergePR(home, pr.ID); err != nil {
		t.Fatalf("merge PR in clean Windows clone: %v", err)
	}
	if got := resolve(t, dir, "local"); got != pr.HeadSHA {
		t.Fatalf("local = %s, want %s", got, pr.HeadSHA)
	}
}
