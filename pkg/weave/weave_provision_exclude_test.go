package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Provisioned agent skills are workspace scaffolding, not worker code: the
// dirty-tree preservation commit must never capture them, while a real worker
// file is still committed. Skill files the repo already tracks stay tracked
// (info/exclude never silences tracked paths).
func TestMaybeAutoCommitExcludesProvisionedSkills(t *testing.T) {
	workspace := weaveTestRepo(t)
	weaveTestGit(t, workspace, "checkout", "-qb", "agent/weave-issue-1")

	// A skill file the repo already tracks: committing a change to it must
	// keep working after the fix.
	trackedSkill := filepath.Join(workspace, ".agents", "skills", "tracked", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(trackedSkill), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trackedSkill, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	weaveTestGit(t, workspace, "add", ".")
	weaveTestGit(t, workspace, "commit", "-qm", "base with tracked skill")

	// Simulate weave skill provisioning into the workspace.
	for _, p := range []string{
		filepath.Join(workspace, ".agents", "skills", "guide", "SKILL.md"),
		filepath.Join(workspace, ".agents", "skills", "guide", ".bashy-export.json"),
		filepath.Join(workspace, ".claude", "skills", "guide", "SKILL.md"),
		filepath.Join(workspace, ".claude", "skills", "guide", ".bashy-export.json"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("provisioned\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Real worker output plus a change to the tracked skill file.
	if err := os.WriteFile(filepath.Join(workspace, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trackedSkill, []byte("improved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	weaveEnsureBashyExclude(workspace)
	weaveTestGit(t, workspace, "add", "-A")
	staged := weaveTestGit(t, workspace, "diff", "--cached", "--name-only")
	for _, bad := range []string{".agents/skills/guide", ".claude/skills/guide"} {
		if strings.Contains(staged, bad) {
			t.Fatalf("worker git add -A staged provisioned skills:\n%s", staged)
		}
	}

	committed, err := maybeAutoCommit(workspace, "weave(auto): issue 1 — test")
	if err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("maybeAutoCommit reported no commit")
	}
	names := weaveTestGit(t, workspace, "show", "--name-only", "--format=", "HEAD")
	if !strings.Contains(names, "work.txt") {
		t.Fatalf("preservation commit missing worker file:\n%s", names)
	}
	for _, bad := range []string{".agents/skills/guide", ".claude/skills/guide"} {
		if strings.Contains(names, bad) {
			t.Fatalf("preservation commit captured provisioned skills:\n%s", names)
		}
	}
	if !strings.Contains(names, filepath.ToSlash(filepath.Join(".agents", "skills", "tracked", "SKILL.md"))) {
		t.Fatalf("tracked skill change was wrongly excluded:\n%s", names)
	}
}

func TestWeaveEnsureBashyExcludeWorktree(t *testing.T) {
	repo := weaveTestRepo(t)
	workspace := filepath.Join(t.TempDir(), "worker")
	weaveTestGit(t, repo, "worktree", "add", "-b", "worker", workspace)
	weaveEnsureBashyExclude(workspace)
	if got := weaveTestGit(t, workspace, "check-ignore", ".agents/skills/guide/SKILL.md"); !strings.Contains(got, ".agents/skills/guide/SKILL.md") {
		t.Fatalf("worktree did not ignore provisioned skill: %q", got)
	}
}
