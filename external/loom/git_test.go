package loom

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedGitConfig(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "app.ini")
	original := "[storage]\nSERVE_DIRECT = true\n[git]\nGC_ARGS = --aggressive\nPATH = git\n"
	if err := os.WriteFile(cfg, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	const git = "C:\\cache with spaces\\mingit\\cmd\\git.exe"
	if err := writeGitConfig(cfg, git); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PATH = C:/cache with spaces/mingit/cmd/git.exe", "GC_ARGS = --aggressive", "SERVE_DIRECT = true"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %q in %s", want, b)
		}
	}
	if err := writeGitConfig(cfg, git); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(cfg)
	if string(again) != string(b) {
		t.Fatal("not idempotent")
	}
}

// ensureGitRuntime does BOTH halves of pointing Gitea at the managed git:
// app.ini's [git] PATH, and the bash name its hook scripts need (hooks.go).
func TestEnsureGitRuntime(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "mingit", "2.55.0.2")
	usrBin := filepath.Join(root, "usr", "bin")
	if err := os.MkdirAll(filepath.Join(root, "cmd"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitExe := filepath.Join(root, "cmd", hookShellName("git"))
	writeExe(t, gitExe, "git")
	writeExe(t, filepath.Join(usrBin, hookShellName("sh")), "shell")

	cfg := filepath.Join(dir, "app.ini")
	if err := os.WriteFile(cfg, []byte("APP_NAME = loom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ensureGitRuntime(context.Background(), cfg, func(context.Context) (string, error) { return gitExe, nil })
	if err != nil {
		t.Fatal(err)
	}
	if got != gitExe {
		t.Fatalf("git exe: got %q want %q", got, gitExe)
	}
	ini, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := "PATH = " + strings.ReplaceAll(gitExe, "\\", "/"); !strings.Contains(string(ini), want) {
		t.Fatalf("missing %q in %s", want, ini)
	}
	if bash := filepath.Join(usrBin, hookShellName("bash")); !usable(bash) {
		t.Fatalf("hook shell not provisioned at %s", bash)
	}
}

func TestEnsureGitRuntimeErrors(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "app.ini")
	if err := os.WriteFile(cfg, []byte("APP_NAME = loom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(cfg)

	sentinel := errors.New("provision failed")
	if _, err := ensureGitRuntime(context.Background(), cfg, func(context.Context) (string, error) { return "", sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("resolve error: got %v", err)
	}
	// An unexpected tree layout must fail loudly: a silently skipped hook shell
	// is exactly the failure mode this story fixes.
	if _, err := ensureGitRuntime(context.Background(), cfg, func(context.Context) (string, error) {
		return filepath.Join(t.TempDir(), "bin", "git"), nil
	}); err == nil || !strings.Contains(err.Error(), "hook shell") {
		t.Fatalf("unexpected layout: got %v", err)
	}
	// No shell to link in an otherwise well-formed tree: also loud.
	root := filepath.Join(t.TempDir(), "mingit")
	if err := os.MkdirAll(filepath.Join(root, "cmd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureGitRuntime(context.Background(), cfg, func(context.Context) (string, error) {
		return filepath.Join(root, "cmd", hookShellName("git")), nil
	}); err == nil || !strings.Contains(err.Error(), "hook shell") {
		t.Fatalf("missing shell: got %v", err)
	}
	after, _ := os.ReadFile(cfg)
	if string(after) != string(before) {
		t.Fatal("failed provisioning changed app.ini")
	}
}

func writeExe(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
