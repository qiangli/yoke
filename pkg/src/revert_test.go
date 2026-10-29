package src_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/src"
)

func TestRevert_DryRunAndApply(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_AGENT", "test-agent")
	_, newSHA := seedFixture(t, home)
	fetchAt(t, home, newSHA)
	dir := destOf(home)

	reviewWaiver(t, home)
	pristineSHA := resolve(t, dir, "pristine")

	// 1. Initially at pristine and clean.
	res, err := src.Revert(home, "yoke", false)
	if err != nil {
		t.Fatalf("Revert dry-run: %v", err)
	}
	if len(res.Commits) != 0 || len(res.Files) != 0 {
		t.Fatalf("expected nothing to discard initially, got %+v", res)
	}

	// 2. Open a PR, merge it into local, so local has a commit past pristine.
	pr := openSkillPR(t, home, "feature-x", "Feature X", "")
	if _, err := src.MergePR(home, pr.ID); err != nil {
		t.Fatalf("MergePR: %v", err)
	}
	localSHA := resolve(t, dir, "local")
	if localSHA == pristineSHA {
		t.Fatalf("local did not advance past pristine")
	}

	// 3. Make uncommitted changes: modify a file and create an untracked file.
	writeWork(t, dir, "pkg/weave/engine.go", "// uncommitted modification\n")
	writeWork(t, dir, "untracked.txt", "untracked file\n")

	// 4. Dry-run: should list what would be discarded.
	res, err = src.Revert(home, "yoke", false)
	if err != nil {
		t.Fatalf("Revert dry-run: %v", err)
	}
	if len(res.Commits) == 0 {
		t.Errorf("expected commits to discard in dry-run, got 0")
	}
	hasEngine := false
	hasUntracked := false
	for _, f := range res.Files {
		if strings.Contains(f, "pkg/weave/engine.go") {
			hasEngine = true
		}
		if strings.Contains(f, "untracked.txt") {
			hasUntracked = true
		}
	}
	if !hasEngine || !hasUntracked {
		t.Errorf("expected modified and untracked files in dry-run, got %v", res.Files)
	}

	// Verify dry-run did NOT change anything:
	if sha := resolve(t, dir, "local"); sha != localSHA {
		t.Errorf("dry-run moved local: %s != %s", sha, localSHA)
	}
	if content := readWork(t, dir, "untracked.txt"); content != "untracked file\n" {
		t.Errorf("dry-run deleted untracked file")
	}

	// 5. Apply: should reset checkout to pristine (base tip).
	res, err = src.Revert(home, "yoke", true)
	if err != nil {
		t.Fatalf("Revert apply: %v", err)
	}
	if !res.Applied {
		t.Errorf("Applied = false, want true")
	}

	// Verify local is at pristineSHA.
	if sha := resolve(t, dir, "local"); sha != pristineSHA {
		t.Errorf("local is at %s, want %s", sha, pristineSHA)
	}

	// Verify HEAD is on local.
	_, branchLines, err := ygit.Branch(ygit.BranchOptions{RepoPath: dir})
	if err != nil {
		t.Fatal(err)
	}
	cur := ""
	for _, l := range branchLines {
		if strings.HasPrefix(l, "* ") {
			cur = strings.TrimPrefix(l, "* ")
		}
	}
	if cur != "local" {
		t.Errorf("current branch = %q, want local", cur)
	}

	// Verify untracked file is gone and modified file is reset.
	if _, err := os.Stat(filepath.Join(dir, "untracked.txt")); !os.IsNotExist(err) {
		t.Errorf("untracked file still exists after revert --apply")
	}
	if content := readWork(t, dir, "pkg/weave/engine.go"); strings.Contains(content, "uncommitted modification") || strings.Contains(content, "feature-x") {
		t.Errorf("file still contains changes after revert --apply: %q", content)
	}

	// Verify State is pristine.
	st, err := src.State(home, "yoke")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != src.StatePristine {
		t.Errorf("State = %q, want %q", st.State, src.StatePristine)
	}

	// Verify Check is clean.
	violations, err := src.Check(home)
	if err != nil || len(violations) > 0 {
		t.Errorf("Check violations after revert: %v (%v)", violations, err)
	}
}

func TestRevert_NeverTouchesOtherRepos(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_AGENT", "test-agent")
	_, newSHA := seedFixture(t, home)
	fetchAt(t, home, newSHA)

	// Create a second repo in home/src/otherrepo with its own branches and PRs
	dirB := filepath.Join(home, "src", "otherrepo")
	if _, err := ygit.Clone(ygit.CloneOptions{URL: filepath.Join(home, "seed", "yoke.git"), Path: dirB}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ygit.Branch(ygit.BranchOptions{RepoPath: dirB, Name: "pristine"}); err != nil {
		t.Fatal(err)
	}
	if err := src.CreateBranch(home, "otherrepo", "other-branch"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, dirB, "pkg/weave/engine.go", "// other repo change\n")
	otherCommit := commitWork(t, dirB, "other commit")
	otherPR, err := src.OpenPR(home, "otherrepo", "other-branch", "Other PR", "")
	if err != nil {
		t.Fatal(err)
	}

	// Make changes in yoke
	reviewWaiver(t, home)
	prA := openSkillPR(t, home, "yoke-branch", "Yoke PR", "")
	if _, err := src.MergePR(home, prA.ID); err != nil {
		t.Fatal(err)
	}

	// Revert yoke
	if _, err := src.Revert(home, "yoke", true); err != nil {
		t.Fatalf("Revert yoke: %v", err)
	}

	// Verify otherrepo's branch, commit, and PR are completely untouched
	if sha := resolve(t, dirB, "other-branch"); sha != otherCommit {
		t.Errorf("otherrepo branch moved: %s != %s", sha, otherCommit)
	}
	gotPR, err := src.GetPR(home, otherPR.ID)
	if err != nil || gotPR.State != "open" {
		t.Errorf("otherrepo PR touched: %+v (%v)", gotPR, err)
	}
	if body := readWork(t, dirB, "pkg/weave/engine.go"); !strings.Contains(body, "other repo change") {
		t.Errorf("otherrepo working tree changed: %q", body)
	}
}

func TestOpenPR_RefusesWhenNoDiff(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_AGENT", "test-agent")
	_, newSHA := seedFixture(t, home)
	fetchAt(t, home, newSHA)

	// Create topic branch off local without any commits
	if err := src.CreateBranch(home, "yoke", "empty-topic"); err != nil {
		t.Fatal(err)
	}

	// Attempt to open PR: must be refused with a plain message
	_, err := src.OpenPR(home, "yoke", "empty-topic", "Empty Topic PR", "")
	if err == nil {
		t.Fatal("OpenPR succeeded on branch with no diff, want refusal")
	}
	if !strings.Contains(err.Error(), "no diff against base") {
		t.Errorf("OpenPR err = %q, want error mentioning 'no diff against base'", err.Error())
	}

	// Verify no PR record was created
	prs, err := src.ListPRs(home, "yoke")
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 0 {
		t.Errorf("PR was recorded: %+v", prs)
	}

	// CLI test: src pr open yoke empty-topic --title "Test"
	cmd := src.NewSrcCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"pr", "open", "yoke", "empty-topic", "--title", "Test"})
	t.Setenv("BASHY_HOME", home)
	err = cmd.Execute()
	if err == nil {
		t.Fatal("CLI pr open succeeded on empty diff, want refusal")
	}
	if !strings.Contains(err.Error(), "no diff against base") {
		t.Errorf("CLI pr open err = %q, want 'no diff against base'", err.Error())
	}
}

func TestRevert_CLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_AGENT", "test-agent")
	t.Setenv("BASHY_HOME", home)
	_, newSHA := seedFixture(t, home)
	fetchAt(t, home, newSHA)
	dir := destOf(home)

	run := func(args ...string) (string, error) {
		cmd := src.NewSrcCmd()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}

	// 1. Clean repo: dry-run prints (nothing to discard)
	out, err := run("revert", "yoke")
	if err != nil {
		t.Fatalf("revert dry-run clean: %v", err)
	}
	if !strings.Contains(out, "(nothing to discard)") {
		t.Errorf("revert clean out = %q, want '(nothing to discard)'", out)
	}

	// 2. Add local changes
	writeWork(t, dir, "test.txt", "hello\n")

	// 3. Dry-run lists what would be discarded
	out, err = run("revert", "yoke")
	if err != nil {
		t.Fatalf("revert dry-run dirty: %v", err)
	}
	if !strings.Contains(out, "would discard:") || !strings.Contains(out, "test.txt") {
		t.Errorf("revert dirty out = %q, want would discard test.txt", out)
	}

	// 4. Apply resets to pristine
	out, err = run("revert", "yoke", "--apply")
	if err != nil {
		t.Fatalf("revert apply: %v", err)
	}
	if !strings.Contains(out, "reverted yoke to pristine") {
		t.Errorf("revert apply out = %q, want 'reverted yoke to pristine'", out)
	}
}

func TestRevert_FromTopicBranchPreservesTopicBranch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_AGENT", "test-agent")
	_, newSHA := seedFixture(t, home)
	fetchAt(t, home, newSHA)
	dir := destOf(home)

	pristineSHA := resolve(t, dir, "pristine")

	// Create topic branch and commit on it
	if err := src.CreateBranch(home, "yoke", "topic-keep"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, dir, "pkg/weave/engine.go", "// topic keep\n")
	topicCommit := commitWork(t, dir, "topic commit")

	// Add an uncommitted file
	writeWork(t, dir, "dirty.txt", "dirty\n")

	// Revert yoke with apply
	res, err := src.Revert(home, "yoke", true)
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if !res.Applied {
		t.Fatal("Applied = false")
	}

	// Local is now pristine
	if sha := resolve(t, dir, "local"); sha != pristineSHA {
		t.Errorf("local sha = %s, want %s", sha, pristineSHA)
	}

	// Topic branch still exists and points to its commit
	if sha := resolve(t, dir, "topic-keep"); sha != topicCommit {
		t.Errorf("topic-keep moved: %s != %s", sha, topicCommit)
	}

	// Dirty file is gone
	if _, err := os.Stat(filepath.Join(dir, "dirty.txt")); !os.IsNotExist(err) {
		t.Errorf("dirty.txt still exists")
	}
}
