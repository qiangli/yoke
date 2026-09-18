package weave

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWeaveContextTrailerIncludesStructuredEvidence(t *testing.T) {
	verifyExit := 0
	got := weaveContextTrailer(&weaveItem{
		ID:    12,
		Title: "carry resume context",
	}, weaveTerminalEvidence{
		FilesTouched: []string{"pkg/weave/a.go", "pkg/weave/b.go"},
		CommitsAhead: 3,
		VerifyExit:   &verifyExit,
	})

	for _, want := range []string{
		"[weave-context]",
		"issue: #12 carry resume context",
		"files: pkg/weave/a.go, pkg/weave/b.go",
		"commits-ahead: 3",
		"verify: exit=0",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("trailer missing %q:\n%s", want, got)
		}
	}
}

func TestParseWeaveContextTrailerRoundTrips(t *testing.T) {
	trailer := "[weave-context]\nissue: #7 parser\nfiles: pkg/weave/x.go\ncommits-ahead: 1\nverify: n/a"
	got, ok := parseWeaveContextTrailer("subject\n\nbody\n\n" + trailer + "\n")
	if !ok {
		t.Fatal("parseWeaveContextTrailer did not find trailer")
	}
	if got != trailer {
		t.Fatalf("parsed trailer mismatch:\nwant:\n%s\ngot:\n%s", trailer, got)
	}
	if got, ok := parseWeaveContextTrailer("subject\n\nno context here"); ok || got != "" {
		t.Fatalf("parse without trailer = %q, %v; want empty false", got, ok)
	}
}

func TestResumeInjectPrependsLastWeaveContextTrailer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("system git not available; resume inject needs it")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_AGENTIC", "")

	root := weaveTestRepo(t)
	t.Chdir(root)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := weaveRepoRoot(cwd)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := weaveQueueDir(resolvedRoot)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(dir, "workspaces", "issue-1")
	if err := os.MkdirAll(filepath.Dir(workspace), 0o755); err != nil {
		t.Fatal(err)
	}
	weaveTestGit(t, root, "clone", "--local", "--no-hardlinks", root, workspace)
	weaveTestGit(t, workspace, "checkout", "-qb", "agent/weave-issue-1")
	trailer := "[weave-context]\nissue: #1 resume thread\nfiles: pkg/weave/weave_impl.go\ncommits-ahead: 1\nverify: exit=0"
	weaveTestGit(t, workspace, "commit", "--allow-empty", "-m", "checkpoint", "-m", trailer)

	q := &weaveQueue{
		NextID: 2,
		Root:   resolvedRoot,
		Items: []*weaveItem{{
			ID:        1,
			Title:     "resume thread",
			State:     "failed",
			Workspace: workspace,
			Branch:    "agent/weave-issue-1",
			Created:   time.Now().UTC(),
		}},
	}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}

	if out, code := runWeave(t, "start", "--issue", "1", "--resume", "--no-spawn", "--json"); code != 0 {
		t.Fatalf("resume start exit=%d out=%s", code, out)
	}
	b, err := os.ReadFile(filepath.Join(workspace, "WEAVE_MEMORY.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, "## Resuming — last context") {
		t.Fatalf("WEAVE_MEMORY.md missing resume heading:\n%s", got)
	}
	if !strings.Contains(got, trailer) {
		t.Fatalf("WEAVE_MEMORY.md missing trailer:\n%s", got)
	}
}
