package todo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoFreshnessReportsAheadBehindAndLastFetch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	base := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	origin := filepath.Join(base, "origin.git")
	run(base, "init", "-q", "--bare", "-b", "main", origin)
	a := filepath.Join(base, "a")
	run(base, "clone", "-q", origin, a)
	run(a, "commit", "-q", "--allow-empty", "-m", "one")
	run(a, "push", "-q", "-u", "origin", "main")

	if fresh := RepoFreshness(a); !strings.Contains(fresh, "origin/main +0/-0") {
		t.Fatalf("in sync: %q", fresh)
	}
	// Another host pushes; this host fetches → behind, and the line says pull.
	b := filepath.Join(base, "b")
	run(base, "clone", "-q", origin, b)
	run(b, "commit", "-q", "--allow-empty", "-m", "two")
	run(b, "push", "-q", "origin", "main")
	run(a, "fetch", "-q")
	fresh := RepoFreshness(a)
	if !strings.Contains(fresh, "+0/-1") || !strings.Contains(fresh, "last fetch") || !strings.Contains(fresh, "run: git pull") {
		t.Fatalf("behind: %q", fresh)
	}
	// Not a checkout with an upstream → nothing, not an error.
	if fresh := RepoFreshness(t.TempDir()); fresh != "" {
		t.Fatalf("non-git dir printed %q", fresh)
	}
}
