package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func hostGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, errb.String())
	}
	return out.String()
}

func needHostGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no host git on PATH")
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stagedRepo: HEAD has a.txt, sub/b.txt, gone.txt, nonl.txt; the index
// then holds a modified file, a nested modified file, a deletion, a
// no-trailing-newline edit, and new files (top-level and nested).
func stagedRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	hostGit(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "a.txt", "one\ntwo\nthree\n")
	write(t, dir, "sub/b.txt", "x\ny\n")
	write(t, dir, "gone.txt", "bye\n")
	write(t, dir, "nonl.txt", "no newline")
	hostGit(t, dir, "add", "-A")
	hostGit(t, dir, "commit", "-q", "-m", "init")
	write(t, dir, "a.txt", "one\nTWO\nthree\nfour\n")
	write(t, dir, "sub/b.txt", "x\ny\nz\n")
	write(t, dir, "nonl.txt", "no newline either")
	write(t, dir, "new.txt", "brand new\n")
	write(t, dir, "sub/deep/new2.txt", "deep new\n")
	if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The sprint-412 repro: `git add -A && git diff --cached --binary HEAD`
// printed "Added all changes" and an empty diff.
func TestAddAllThenDiffCachedBinaryMatchesHostGit(t *testing.T) {
	needHostGit(t)
	dir := stagedRepo(t)
	ctx := context.Background()

	res, err := Exec(ctx, dir, []string{"add", "-A"})
	if err != nil {
		t.Fatalf("add -A: %v", err)
	}
	if res.Stdout != "" || res.Stderr != "" || res.ExitCode != 0 {
		t.Fatalf("add -A must be silent, got %+v", res)
	}

	got, err := Exec(ctx, dir, []string{"diff", "--cached", "--binary", "HEAD"})
	if err != nil {
		t.Fatalf("diff --cached --binary HEAD: %v", err)
	}
	if got.Stdout == "" {
		t.Fatal("empty diff: the staged changes were lost")
	}
	want := hostGit(t, dir, "diff", "--cached", "--binary", "--no-renames", "HEAD")
	for _, marker := range []string{"new file mode", "deleted file mode", `\ No newline at end of file`} {
		if !strings.Contains(want, marker) {
			t.Fatalf("fixture lost coverage of %q", marker)
		}
	}
	if got.Stdout != want {
		t.Fatalf("diff --cached --binary HEAD differs from host git\n--- native ---\n%s\n--- host ---\n%s", got.Stdout, want)
	}

	// The same patch with no REV, via --staged, and a path filter.
	for _, args := range [][]string{
		{"diff", "--cached"},
		{"diff", "--staged"},
		{"diff", "--staged", "--binary"},
		{"diff", "--cached", "--", "sub"},
	} {
		g, err := Exec(ctx, dir, args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		w := hostGit(t, dir, append(append([]string{}, args[:1]...), append([]string{"--no-renames"}, args[1:]...)...)...)
		if g.Stdout != w {
			t.Fatalf("%v differs from host git\n--- native ---\n%s\n--- host ---\n%s", args, g.Stdout, w)
		}
	}
}

func TestDiffCachedPatchApplies(t *testing.T) {
	needHostGit(t)
	dir := stagedRepo(t)
	ctx := context.Background()
	if _, err := Exec(ctx, dir, []string{"add", "-A"}); err != nil {
		t.Fatal(err)
	}
	got, err := Exec(ctx, dir, []string{"diff", "--cached", "--binary", "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	clean := t.TempDir()
	hostGit(t, clean, "clone", "-q", dir, ".")
	patch := filepath.Join(t.TempDir(), "p.diff")
	if err := os.WriteFile(patch, []byte(got.Stdout), 0o644); err != nil {
		t.Fatal(err)
	}
	hostGit(t, clean, "apply", patch)
	for _, f := range []string{"a.txt", "sub/b.txt", "nonl.txt", "new.txt", "sub/deep/new2.txt"} {
		a, _ := os.ReadFile(filepath.Join(dir, f))
		b, err := os.ReadFile(filepath.Join(clean, f))
		if err != nil || string(a) != string(b) {
			t.Fatalf("%s after apply = %q (%v), want %q", f, b, err, a)
		}
	}
	if _, err := os.Stat(filepath.Join(clean, "gone.txt")); err == nil {
		t.Fatal("deletion not applied")
	}
}

func TestDiffCachedEmptyWhenNothingStaged(t *testing.T) {
	needHostGit(t)
	dir := stagedRepo(t)
	res, err := Exec(context.Background(), dir, []string{"diff", "--cached", "--binary", "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "" {
		t.Fatalf("nothing staged but got %q", res.Stdout)
	}
}

func TestDiffCachedBinaryFileFallsBackToHost(t *testing.T) {
	needHostGit(t)
	dir := stagedRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "img.bin"), []byte{0, 1, 2, 0, 255, 254, 0}, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := Exec(ctx, dir, []string{"add", "-A"}); err != nil {
		t.Fatal(err)
	}
	args := []string{"diff", "--cached", "--binary", "HEAD"}
	if _, err := Exec(ctx, dir, args); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("native binary diff must be refused, not stubbed: %v", err)
	}
	if _, err := ExecOrExternal(ctx, dir, args, false); err == nil || !strings.Contains(err.Error(), "--external=true") {
		t.Fatalf("refusal must name --external=true, got %v", err)
	}
	res, err := ExecOrExternal(ctx, dir, args, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "GIT binary patch") {
		t.Fatalf("host fallback should emit a binary patch:\n%s", res.Stdout)
	}
	if want := hostGit(t, dir, args...); res.Stdout != want {
		t.Fatal("external result differs from host git")
	}
}

func TestUnknownFlagUsesHostOnlyWhenExternal(t *testing.T) {
	needHostGit(t)
	dir := stagedRepo(t)
	ctx := context.Background()
	args := []string{"diff", "--cached", "--word-diff"}
	if _, err := ExecOrExternal(ctx, dir, args, false); err == nil || !strings.Contains(err.Error(), "--external=true") {
		t.Fatalf("want error naming --external=true, got %v", err)
	}
	if _, err := ExecOrExternal(ctx, dir, args, true); err != nil {
		t.Fatalf("external run: %v", err)
	}
}
