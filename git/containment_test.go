package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5/osfs"
)

// TestGitDepGraphExcludesSecureJoin is the S488.3a acceptance test: the
// shipped ./git dependency graph must not contain the MPL-2.0
// cyphar/filepath-securejoin module (reached today via
// go-billy/v5/osfs). It fails until the billy dependency is replaced
// with a securejoin-free one, and guards the swap afterwards.
func TestGitDepGraphExcludesSecureJoin(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	modRoot := thisFile
	for {
		dir := filepath.Dir(modRoot)
		if dir == modRoot {
			t.Fatal("module root not found")
		}
		modRoot = dir
		if _, err := os.Stat(filepath.Join(modRoot, "go.mod")); err == nil {
			break
		}
	}
	cmd := exec.Command(goBin, "list", "-deps", "./git/...")
	cmd.Dir = modRoot
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./git/...: %v", err)
	}
	var flagged []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "cyphar") || strings.Contains(line, "securejoin") {
			flagged = append(flagged, line)
		}
	}
	if len(flagged) > 0 {
		t.Fatalf("MPL path still in ./git dep graph:\n%s", strings.Join(flagged, "\n"))
	}
}

// TestBoundOSConfinesPathEscape pins the Git path containment the
// securejoin removal must preserve, through the public bound-OS
// surface: ".." and absolute paths fold back inside the base dir, and
// symlink resolutions that would leave the base dir never yield a
// usable outside path. Together with
// TestGitDepGraphExcludesSecureJoin it proves the MPL code was removed
// without weakening the boundary.
func TestBoundOSConfinesPathEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("TOP-SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "evil-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop2", filepath.Join(root, "loop1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop1", filepath.Join(root, "loop2")); err != nil {
		t.Fatal(err)
	}
	fs := osfs.New(root, osfs.WithBoundOS())

	// ".." above the base dir folds back inside instead of escaping.
	for _, p := range []string{"../escape", "../../escape", "sub/../../escape"} {
		sub, err := fs.Chroot(p)
		if err != nil {
			t.Errorf("Chroot(%q): %v", p, err)
			continue
		}
		if got, want := sub.Root(), filepath.Join(root, "escape"); got != want {
			t.Errorf("Chroot(%q).Root() = %q, want %q", p, got, want)
		}
	}
	if f, err := fs.Create("../created.txt"); err != nil {
		t.Errorf("Create(../created.txt): %v", err)
	} else {
		f.Close()
	}
	if _, err := os.Stat(filepath.Join(root, "created.txt")); err != nil {
		t.Errorf("created file did not land inside the base dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "created.txt")); err == nil {
		t.Error("created file escaped above the base dir")
	}

	// A symlink pointing outside never yields usable outside content.
	for _, p := range []string{"evil-link/secret.txt", "sub/../evil-link/secret.txt"} {
		f, err := fs.Open(p)
		if err == nil {
			f.Close()
			t.Errorf("Open(%q): reached outside content, want an error", p)
			continue
		}
		if _, err := os.Stat(filepath.Join(outside, "secret.txt")); err != nil {
			t.Fatalf("outside fixture vanished: %v", err)
		}
	}
	sub, err := fs.Chroot("evil-link")
	if err != nil {
		t.Fatalf("Chroot(evil-link): %v", err)
	}
	if !withinRoot(root, sub.Root()) {
		t.Errorf("Chroot(evil-link).Root() = %q escapes %q", sub.Root(), root)
	}

	// Symlink loops are an error, not a hang or an escape.
	if _, err := fs.Chroot("loop1"); err == nil {
		t.Error("Chroot(loop1): expected an error, got nil")
	}

	// Sanity: confined paths still work through the same surface.
	if _, err := fs.Open("inside.txt"); err != nil {
		t.Errorf("Open(inside.txt): %v", err)
	}
	sub, err = fs.Chroot("sub")
	if err != nil {
		t.Fatalf("Chroot(sub): %v", err)
	}
	if got := sub.Root(); got != filepath.Join(root, "sub") {
		t.Fatalf("Chroot(sub).Root() = %q", got)
	}
}

// withinRoot reports whether p is root or descends from it, lexically.
func withinRoot(root, p string) bool {
	root = filepath.Clean(root)
	p = filepath.Clean(p)
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+string(filepath.Separator))
}

// TestLinkedWorktreeOutsideAdminOpens pins the git-faithful half of the
// story: a linked worktree whose admin directory lives outside the
// worktree root (the normal layout) must keep opening. The MPL removal
// must not "fix" path following by confining gitdir/commondir
// pointers, which would break every real linked worktree.
func TestLinkedWorktreeOutsideAdminOpens(t *testing.T) {
	isolateGlobalConfig(t)
	main := makeTwoCommitRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	admin := filepath.Join(main, ".git", "worktrees", "manual")
	if err := os.MkdirAll(admin, 0o755); err != nil {
		t.Fatal(err)
	}
	// Minimal linked-worktree admin dir: commondir points at the main
	// .git dir (outside the worktree root, as real git writes it), and
	// HEAD mirrors the main repo so refs resolve through commondir.
	mainGit := filepath.Join(main, ".git")
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte(mainGit+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	head, err := os.ReadFile(filepath.Join(mainGit, "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(admin, "HEAD"), head, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+admin+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := openLinked(wt, false)
	if err != nil {
		t.Fatalf("openLinked with outside admin dir: %v", err)
	}
	got, err := r.Head()
	if err != nil {
		t.Fatalf("Head through commondir: %v", err)
	}
	want, err := RevParse(RevParseOptions{RepoPath: main})
	if err != nil {
		t.Fatalf("rev-parse main: %v", err)
	}
	if got.Hash().String() != want.Hash {
		t.Fatalf("worktree HEAD %s != main HEAD %s", got.Hash(), want.Hash)
	}
}
