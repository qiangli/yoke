package mplfree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	securejoin "github.com/cyphar/filepath-securejoin"
)

// TestEngineSecureJoinIsPermissive pins the S488.3d contract: the
// cyphar/filepath-securejoin package in the engine import graph must
// resolve to yoke's clean-room external/securejoin replacement, never
// to the MPL-2.0 upstream module cache. A package that dropped out of
// the graph entirely also satisfies the contract.
func TestEngineSecureJoinIsPermissive(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	deps := engineDeps(t)
	wantDir := filepath.Join(root, "external", "securejoin")
	dir, inGraph := deps["github.com/cyphar/filepath-securejoin"]
	if !inGraph {
		t.Log("cyphar/filepath-securejoin dropped out of the engine graph: contract satisfied")
		return
	}
	if dir != wantDir {
		t.Errorf("github.com/cyphar/filepath-securejoin resolves to %s, want the clean-room replacement at %s", dir, wantDir)
	}
}

// TestSecureJoinContainmentContract pins the containment behavior
// every shipped engine caller relies on: joins stay inside the root,
// lexical escapes are an error, symlinks that would leave the root
// never yield a usable outside path, and loops are an error. It runs
// against the effective module for the import path, so it guards the
// replacement without changing callers.
func TestSecureJoinContainmentContract(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("TOP-SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "file.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "evil-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub", filepath.Join(root, "good-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop2", filepath.Join(root, "loop1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop1", filepath.Join(root, "loop2")); err != nil {
		t.Fatal(err)
	}

	// Plain joins stay inside the root, including the absolute
	// "/etc/passwd" shape the lookup callers use.
	for _, unsafe := range []string{"a/b", ".containerignore", "/etc/passwd", "sub/../inside.txt"} {
		got, err := securejoin.SecureJoin(root, unsafe)
		if err != nil {
			t.Errorf("SecureJoin(%q): unexpected error: %v", unsafe, err)
			continue
		}
		if !withinSecureRoot(root, got) {
			t.Errorf("SecureJoin(%q) = %q escapes %q", unsafe, got, root)
		}
	}

	// Lexical escapes above the root never yield an outside path: the
	// join stays inside the root (clamped), whether or not an error is
	// also reported.
	for _, unsafe := range []string{"..", "../escape", "../../escape", "/../escape"} {
		got, err := securejoin.SecureJoin(root, unsafe)
		if err != nil && got != "" && !withinSecureRoot(root, got) {
			t.Errorf("SecureJoin(%q) error still yielded outside path %q", unsafe, got)
			continue
		}
		if err == nil && !withinSecureRoot(root, got) {
			t.Errorf("SecureJoin(%q) = %q escapes %q", unsafe, got, root)
		}
	}

	// A symlink pointing outside never yields usable outside content:
	// either an error, or a path that stays inside the root and does
	// not expose the outside file.
	for _, unsafe := range []string{"evil-link/secret.txt", "sub/../evil-link/secret.txt"} {
		got, err := securejoin.SecureJoin(root, unsafe)
		if err != nil {
			if got != "" && !withinSecureRoot(root, got) {
				t.Errorf("SecureJoin(%q) error still yielded outside path %q", unsafe, got)
			}
			continue
		}
		if !withinSecureRoot(root, got) {
			t.Errorf("SecureJoin(%q) = %q escapes %q", unsafe, got, root)
			continue
		}
		if data, readErr := os.ReadFile(got); readErr == nil && string(data) == "TOP-SECRET" {
			t.Errorf("SecureJoin(%q) = %q exposes outside content", unsafe, got)
		}
	}

	// Symlink loops are an error, not a hang or an escape.
	if got, err := securejoin.SecureJoin(root, "loop1/x"); err == nil {
		t.Errorf("SecureJoin(loop1/x) = %q, want a loop error", got)
	} else if got != "" && !withinSecureRoot(root, got) {
		t.Errorf("SecureJoin(loop1/x) error still yielded outside path %q", got)
	}

	// A symlink staying inside resolves without an error.
	if got, err := securejoin.SecureJoin(root, "good-link/file.txt"); err != nil {
		t.Errorf("SecureJoin(good-link/file.txt): unexpected error: %v", err)
	} else if !withinSecureRoot(root, got) {
		t.Errorf("SecureJoin(good-link/file.txt) = %q escapes %q", got, root)
	}
}

func withinSecureRoot(root, p string) bool {
	root = filepath.Clean(root)
	p = filepath.Clean(p)
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+string(filepath.Separator))
}
