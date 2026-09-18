// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package weave

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitIdent(t *testing.T, dir string) {
	gitRun(t, dir, "config", "user.email", "t@example.com")
	gitRun(t, dir, "config", "user.name", "weave-test")
}

// TestWeaveHydrateSubmodulesFromLocalOrigin reproduces the coreutils self-repair
// blocker in miniature: a `git clone --local` leaves submodules EMPTY, and a repo
// whose build reaches into a submodule can't compile until it is hydrated.
func TestWeaveHydrateSubmodulesFromLocalOrigin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base := t.TempDir()

	// 1. A submodule origin repo (stands in for external/ollama/src).
	subOrigin := filepath.Join(base, "suborigin")
	mustMkdir(t, subOrigin)
	gitRun(t, subOrigin, "init", "-q")
	gitIdent(t, subOrigin)
	mustWrite(t, filepath.Join(subOrigin, "go.mod"), "module example.com/sub\n\ngo 1.21\n")
	gitRun(t, subOrigin, "add", ".")
	gitRun(t, subOrigin, "commit", "-qm", "init sub")

	// 2. A superproject origin that vendors the submodule (populated working tree).
	superOrigin := filepath.Join(base, "superorigin")
	mustMkdir(t, superOrigin)
	gitRun(t, superOrigin, "init", "-q")
	gitIdent(t, superOrigin)
	mustWrite(t, filepath.Join(superOrigin, "root.txt"), "x")
	gitRun(t, superOrigin, "add", ".")
	gitRun(t, superOrigin, "commit", "-qm", "root")
	gitRun(t, superOrigin, "-c", "protocol.file.allow=always", "submodule", "add", subOrigin, "vendor/sub")
	gitRun(t, superOrigin, "commit", "-qm", "add sub")

	// 3. `git clone --local` — exactly how a weave workspace is made. The
	//    submodule is left EMPTY (the bug).
	workspace := filepath.Join(base, "workspace")
	gitRun(t, base, "clone", "--local", "--no-hardlinks", superOrigin, workspace)
	if entries, _ := os.ReadDir(filepath.Join(workspace, "vendor", "sub")); len(entries) != 0 {
		t.Fatalf("precondition failed: submodule should be empty after --local clone, got %d entries", len(entries))
	}

	// 4. Hydrate from the local origin.
	if err := weaveHydrateSubmodules(superOrigin, workspace, io.Discard, io.Discard); err != nil {
		t.Fatalf("hydrate: %v", err)
	}

	// 5. The submodule working tree is now populated — the build can see go.mod.
	if _, err := os.Stat(filepath.Join(workspace, "vendor", "sub", "go.mod")); err != nil {
		t.Fatalf("submodule not hydrated: %v", err)
	}
	// A prior interrupted hydration can leave untracked nested artifacts.  The
	// next pre-agent hydration must restore a clean workspace, otherwise a later
	// wrapper auto-commit fails despite no agent source change.
	mustWrite(t, filepath.Join(workspace, "vendor", "sub", "hydration.tmp"), "scratch\n")
	if got := string(gitT(t, workspace, "status", "--porcelain")); got == "" {
		t.Fatal("precondition: nested artifact must make superproject dirty")
	}
	if err := weaveHydrateSubmodules(superOrigin, workspace, io.Discard, io.Discard); err != nil {
		t.Fatalf("re-hydrate clean: %v", err)
	}
	if got := string(gitT(t, workspace, "status", "--porcelain")); got != "" {
		t.Fatalf("hydration left workspace dirty: %q", got)
	}

	// 6. Isolation: the URL was synced back to its canonical .gitmodules value, not
	//    left pointing at the superproject-origin path (a breadcrumb an escaping
	//    agent could follow back to the source checkout).
	override := filepath.Join(superOrigin, "vendor", "sub")
	out, _ := exec.Command("git", "-C", workspace, "config", "submodule.vendor/sub.url").Output()
	if got := string(out); len(got) > 0 && filepath.Clean(trimNL(got)) == filepath.Clean(override) {
		t.Fatalf("submodule url left pointing at the local origin override %q — isolation breadcrumb", override)
	}
}

// A repo with no .gitmodules is a clean no-op.
func TestWeaveHydrateSubmodulesNoGitmodules(t *testing.T) {
	ws := t.TempDir()
	if err := weaveHydrateSubmodules(ws, ws, io.Discard, io.Discard); err != nil {
		t.Fatalf("expected no-op for a repo without submodules, got %v", err)
	}
}

// TestWeaveHydrateNestedSubmodulesCleanliness reproduces live wave #22:
// workspace hydration of a repo with nested submodules (e.g. external/podman/src
// containing its own submodules) must leave all submodule levels and the superproject
// completely clean so that auto-commit does not fail.
func TestWeaveHydrateNestedSubmodulesCleanliness(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base := t.TempDir()

	// 1. Level 2 submodule origin (stands in for nested dependency inside podman).
	sub2Origin := filepath.Join(base, "sub2origin")
	mustMkdir(t, sub2Origin)
	gitRun(t, sub2Origin, "init", "-q")
	gitIdent(t, sub2Origin)
	mustWrite(t, filepath.Join(sub2Origin, "nested.txt"), "nested content\n")
	gitRun(t, sub2Origin, "add", ".")
	gitRun(t, sub2Origin, "commit", "-qm", "init sub2")

	// 2. Level 1 submodule origin (stands in for external/podman/src).
	sub1Origin := filepath.Join(base, "sub1origin")
	mustMkdir(t, sub1Origin)
	gitRun(t, sub1Origin, "init", "-q")
	gitIdent(t, sub1Origin)
	mustWrite(t, filepath.Join(sub1Origin, "podman.txt"), "podman content\n")
	gitRun(t, sub1Origin, "add", ".")
	gitRun(t, sub1Origin, "commit", "-qm", "init sub1")
	gitRun(t, sub1Origin, "-c", "protocol.file.allow=always", "submodule", "add", sub2Origin, "vendor/sub2")
	gitRun(t, sub1Origin, "commit", "-qm", "add sub2 to sub1")

	// 3. Superproject origin (stands in for coreutils).
	superOrigin := filepath.Join(base, "superorigin")
	mustMkdir(t, superOrigin)
	gitRun(t, superOrigin, "init", "-q")
	gitIdent(t, superOrigin)
	mustWrite(t, filepath.Join(superOrigin, "root.txt"), "root content\n")
	gitRun(t, superOrigin, "add", ".")
	gitRun(t, superOrigin, "commit", "-qm", "root")
	gitRun(t, superOrigin, "-c", "protocol.file.allow=always", "submodule", "add", sub1Origin, "external/podman/src")
	gitRun(t, superOrigin, "commit", "-qm", "add sub1 to super")

	// Ensure sub1 inside superOrigin has its nested submodule sub2 populated.
	gitRun(t, superOrigin, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive")

	// 4. Create workspace using `git clone --local --no-hardlinks`.
	workspace := filepath.Join(base, "workspace")
	gitRun(t, base, "clone", "--local", "--no-hardlinks", superOrigin, workspace)

	// 5. Hydrate from local origin.
	if err := weaveHydrateSubmodules(superOrigin, workspace, io.Discard, io.Discard); err != nil {
		t.Fatalf("hydrate nested submodules: %v", err)
	}

	// 6. Verify nested files exist.
	if _, err := os.Stat(filepath.Join(workspace, "external", "podman", "src", "vendor", "sub2", "nested.txt")); err != nil {
		t.Fatalf("nested submodule file not hydrated: %v", err)
	}

	// 7. Verify workspace and all submodules are completely clean.
	if got := string(gitT(t, workspace, "status", "--porcelain")); got != "" {
		t.Fatalf("hydration left workspace dirty: %q", got)
	}
	if got := string(gitT(t, filepath.Join(workspace, "external", "podman", "src"), "status", "--porcelain")); got != "" {
		t.Fatalf("hydration left sub1 dirty: %q", got)
	}

	// 8. Re-hydration with tracked modification and commit mismatch in nested submodule must clean it up.
	mustWrite(t, filepath.Join(workspace, "external", "podman", "src", "vendor", "sub2", "nested.txt"), "modified\n")
	// Advance sub2Origin with a new commit and check it out in workspace sub2 to simulate nested commit drift.
	gitWriteAndCommit(t, sub2Origin, "nested.txt", "v2\n", "sub2 v2")
	gitRun(t, filepath.Join(workspace, "external", "podman", "src", "vendor", "sub2"), "fetch", sub2Origin, "HEAD")
	gitRun(t, filepath.Join(workspace, "external", "podman", "src", "vendor", "sub2"), "checkout", "-f", "FETCH_HEAD")

	if got := string(gitT(t, workspace, "status", "--porcelain")); got == "" {
		t.Fatal("precondition: drifted nested submodule must make workspace dirty")
	}
	if err := weaveHydrateSubmodules(superOrigin, workspace, io.Discard, io.Discard); err != nil {
		t.Fatalf("re-hydrate clean: %v", err)
	}
	if got := string(gitT(t, workspace, "status", "--porcelain")); got != "" {
		t.Fatalf("re-hydration left workspace dirty: %q", got)
	}
	if got := string(gitT(t, filepath.Join(workspace, "external", "podman", "src"), "status", "--porcelain")); got != "" {
		t.Fatalf("re-hydration left sub1 dirty: %q", got)
	}
}

func gitWriteAndCommit(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	mustWrite(t, filepath.Join(dir, file), content)
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-qm", msg)
}

// TestWeaveHydrateNestedSubmodulesRace verifies concurrent safety of nested submodule
// hydration across parallel workers.
func TestWeaveHydrateNestedSubmodulesRace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base := t.TempDir()

	sub2Origin := filepath.Join(base, "sub2origin")
	mustMkdir(t, sub2Origin)
	gitRun(t, sub2Origin, "init", "-q")
	gitIdent(t, sub2Origin)
	mustWrite(t, filepath.Join(sub2Origin, "nested.txt"), "nested content\n")
	gitRun(t, sub2Origin, "add", ".")
	gitRun(t, sub2Origin, "commit", "-qm", "init sub2")

	sub1Origin := filepath.Join(base, "sub1origin")
	mustMkdir(t, sub1Origin)
	gitRun(t, sub1Origin, "init", "-q")
	gitIdent(t, sub1Origin)
	mustWrite(t, filepath.Join(sub1Origin, "podman.txt"), "podman content\n")
	gitRun(t, sub1Origin, "add", ".")
	gitRun(t, sub1Origin, "commit", "-qm", "init sub1")
	gitRun(t, sub1Origin, "-c", "protocol.file.allow=always", "submodule", "add", sub2Origin, "vendor/sub2")
	gitRun(t, sub1Origin, "commit", "-qm", "add sub2")

	superOrigin := filepath.Join(base, "superorigin")
	mustMkdir(t, superOrigin)
	gitRun(t, superOrigin, "init", "-q")
	gitIdent(t, superOrigin)
	mustWrite(t, filepath.Join(superOrigin, "root.txt"), "root content\n")
	gitRun(t, superOrigin, "add", ".")
	gitRun(t, superOrigin, "commit", "-qm", "root")
	gitRun(t, superOrigin, "-c", "protocol.file.allow=always", "submodule", "add", sub1Origin, "external/podman/src")
	gitRun(t, superOrigin, "commit", "-qm", "add sub1")
	gitRun(t, superOrigin, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive")

	const workers = 3
	var wg sync.WaitGroup
	errCh := make(chan error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		ws := filepath.Join(base, fmt.Sprintf("workspace_%d", i))
		gitRun(t, base, "clone", "--local", "--no-hardlinks", superOrigin, ws)
		go func(w string) {
			defer wg.Done()
			if err := weaveHydrateSubmodules(superOrigin, w, io.Discard, io.Discard); err != nil {
				errCh <- fmt.Errorf("worker %s hydrate failed: %w", w, err)
				return
			}
			mustWrite(t, filepath.Join(w, "external", "podman", "src", "vendor", "sub2", "scratch.tmp"), "temp\n")
			if err := weaveHydrateSubmodules(superOrigin, w, io.Discard, io.Discard); err != nil {
				errCh <- fmt.Errorf("worker %s re-hydrate failed: %w", w, err)
				return
			}
			out, err := exec.Command("git", "-C", w, "status", "--porcelain").Output()
			if err != nil {
				errCh <- fmt.Errorf("worker %s git status failed: %w", w, err)
				return
			}
			if len(strings.TrimSpace(string(out))) > 0 {
				errCh <- fmt.Errorf("worker %s left dirty: %s", w, out)
				return
			}
		}(ws)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
