package weave

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func weaveTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t",
		"GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@t",
	)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func weaveTestRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	weaveTestGit(t, repo, "init", "-q")
	weaveTestGit(t, repo, "config", "user.name", "weave test")
	weaveTestGit(t, repo, "config", "user.email", "weave-test@example.invalid")
	weaveTestGit(t, repo, "checkout", "-qb", "main")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	weaveTestGit(t, repo, "add", ".")
	weaveTestGit(t, repo, "commit", "-qm", "base")
	root, err := weaveRepoRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestMaybeAutoCommitCommitsDirtyTrackedAndUntrackedChanges(t *testing.T) {
	workspace := weaveTestRepo(t)
	weaveTestGit(t, workspace, "checkout", "-qb", "agent/weave-issue-1")
	if err := os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ev := weaveCollectTerminalEvidence(workspace, "main", "", "", nil, false)
	if !ev.Dirty || ev.UntrackedFiles != 1 || ev.CommitsAhead != 0 {
		t.Fatalf("pre auto-commit evidence = %+v, want dirty tracked changes, 1 untracked, 0 ahead", ev)
	}

	committed, err := maybeAutoCommit(workspace, "weave(auto): issue 1 — test")
	if err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("maybeAutoCommit reported no commit")
	}
	ev = weaveCollectTerminalEvidence(workspace, "main", "", "", nil, false)
	if ev.Dirty || ev.UntrackedFiles != 0 || ev.CommitsAhead != 1 {
		t.Fatalf("post auto-commit evidence = %+v, want clean with 1 commit ahead", ev)
	}
}

func TestAutoCommitOffLeavesDirtyWorkspaceUncommitted(t *testing.T) {
	workspace := weaveTestRepo(t)
	weaveTestGit(t, workspace, "checkout", "-qb", "agent/weave-issue-1")
	if err := os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ev := weaveCollectTerminalEvidence(workspace, "main", "", "", nil, false)
	if !ev.Dirty || ev.UntrackedFiles != 1 || ev.CommitsAhead != 0 {
		t.Fatalf("auto-commit off evidence = %+v, want dirty tracked changes, 1 untracked, 0 ahead", ev)
	}
}

func TestRunWeaveReverifyRefreshesManualCommitAttestation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := weaveTestRepo(t)
	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(dir, "workspaces", "issue-1")
	if err := os.MkdirAll(filepath.Dir(workspace), 0o755); err != nil {
		t.Fatal(err)
	}
	weaveTestGit(t, root, "clone", "--local", "--no-hardlinks", root, workspace)
	weaveTestGit(t, workspace, "checkout", "-qb", "agent/weave-issue-1")
	if err := os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	weaveTestGit(t, workspace, "add", ".")
	weaveTestGit(t, workspace, "commit", "-qm", "manual")

	dirtyExit := 0
	q := &weaveQueue{
		NextID: 2,
		Root:   root,
		Items: []*weaveItem{{
			ID:            1,
			Title:         "manual residue",
			State:         "submitted",
			Workspace:     workspace,
			Branch:        "agent/weave-issue-1",
			Created:       time.Now().UTC(),
			VerifyCommand: "printf rerun > verify.log",
			VerifyExit:    &dirtyExit,
			VerifyTree:    "working-tree-dirty",
			Dirty:         true,
			DirtyFiles:    1,
		}},
	}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}

	oldWD, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWD) }()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	cmd := newWeaveCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"reverify", "1", "--json"})
	if err := cmd.Execute(); err != nil {
		var ec interface{ ExitCode() int }
		if errors.As(err, &ec) {
			t.Fatalf("reverify exit=%d out=%s", ec.ExitCode(), buf.String())
		}
		t.Fatalf("reverify: %v out=%s", err, buf.String())
	}

	q2, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := findWeaveItem(q2, 1)
	if got == nil {
		t.Fatal("issue disappeared")
	}
	if got.Dirty || got.DirtyFiles != 0 || got.CommitsAhead != 1 || got.VerifyExit == nil || *got.VerifyExit != 0 || got.VerifyTree != "head" {
		t.Fatalf("reverified item = %+v, want clean, 1 ahead, verify exit 0 on head", got)
	}
	if b, err := os.ReadFile(filepath.Join(workspace, "verify.log")); err != nil || string(b) != "rerun" {
		t.Fatalf("verify command did not rerun: %q err=%v", b, err)
	}
}
