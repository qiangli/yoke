package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeAgentWorkflow_FreshRepo(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// 1. git init -q .
	initRes, err := Exec(ctx, dir, []string{"init", "-q", "."})
	if err != nil {
		t.Fatalf("git init -q .: %v", err)
	}
	if initRes.ExitCode != 0 {
		t.Fatalf("git init -q . exit code = %d, stderr = %s", initRes.ExitCode, initRes.Stderr)
	}
	if initRes.Stdout != "" {
		t.Errorf("git init -q . expected empty stdout, got %q", initRes.Stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf(".git not created: %v", err)
	}

	// 2. Add a file
	filePath := filepath.Join(dir, "README.md")
	if err := os.WriteFile(filePath, []byte("# Hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Exec(ctx, dir, []string{"add", "."}); err != nil {
		t.Fatalf("git add .: %v", err)
	}

	// 3. git -c user.email=.. -c user.name=.. commit -qm init
	commitRes, err := Exec(ctx, dir, []string{"-c", "user.email=genie@example.com", "-c", "user.name=Genie Agent", "commit", "-qm", "init"})
	if err != nil {
		t.Fatalf("git commit with -c and -qm: %v", err)
	}
	if commitRes.ExitCode != 0 {
		t.Fatalf("git commit exit code = %d, stderr = %s", commitRes.ExitCode, commitRes.Stderr)
	}
	if commitRes.Stdout != "" {
		t.Errorf("git commit -qm expected empty stdout, got %q", commitRes.Stdout)
	}

	// Verify author from git log -1
	logRes, err := Exec(ctx, dir, []string{"log", "-1"})
	if err != nil {
		t.Fatalf("git log -1: %v", err)
	}
	if !strings.Contains(logRes.Stdout, "Author: Genie Agent <genie@example.com>") {
		t.Errorf("expected Author 'Genie Agent <genie@example.com>', got:\n%s", logRes.Stdout)
	}

	// 4. git log --oneline
	onelineRes, err := Exec(ctx, dir, []string{"log", "--oneline"})
	if err != nil {
		t.Fatalf("git log --oneline: %v", err)
	}
	if onelineRes.ExitCode != 0 {
		t.Fatalf("git log --oneline exit code = %d, stderr = %s", onelineRes.ExitCode, onelineRes.Stderr)
	}
	if !strings.Contains(onelineRes.Stdout, "init") {
		t.Errorf("expected 'init' in git log --oneline, got:\n%s", onelineRes.Stdout)
	}
}

func TestNativeInit_Variants(t *testing.T) {
	ctx := context.Background()

	// init with explicit dir non-quiet
	d1 := filepath.Join(t.TempDir(), "repo1")
	res, err := Exec(ctx, "", []string{"init", d1})
	if err != nil {
		t.Fatalf("init dir: %v", err)
	}
	if !strings.Contains(res.Stdout, "Initialized empty Git repository in") {
		t.Errorf("expected 'Initialized empty Git repository in', got %q", res.Stdout)
	}

	// Reinit should succeed gracefully
	res, err = Exec(ctx, d1, []string{"init", "-q", "."})
	if err != nil {
		t.Fatalf("re-init -q: %v", err)
	}
	if res.Stdout != "" {
		t.Errorf("expected empty stdout on re-init -q, got %q", res.Stdout)
	}

	// init --quiet
	d2 := filepath.Join(t.TempDir(), "repo2")
	res, err = Exec(ctx, "", []string{"init", "--quiet", d2})
	if err != nil {
		t.Fatalf("init --quiet: %v", err)
	}
	if res.Stdout != "" {
		t.Errorf("expected empty stdout on --quiet, got %q", res.Stdout)
	}

	// init -b mymain
	d3 := filepath.Join(t.TempDir(), "repo3")
	res, err = Exec(ctx, "", []string{"init", "-q", "-b", "trunk", d3})
	if err != nil {
		t.Fatalf("init -b trunk: %v", err)
	}
	br, err := Exec(ctx, d3, []string{"symbolic-ref", "HEAD"})
	if err != nil {
		t.Fatalf("symbolic-ref: %v", err)
	}
	if !strings.Contains(br.Stdout, "refs/heads/trunk") {
		t.Errorf("expected refs/heads/trunk, got %q", br.Stdout)
	}
}

func TestNativeCommit_Flags(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	if _, err := Exec(ctx, dir, []string{"init", "-q", "."}); err != nil {
		t.Fatal(err)
	}

	f := filepath.Join(dir, "file.txt")
	_ = os.WriteFile(f, []byte("line1\n"), 0o644)
	if _, err := Exec(ctx, dir, []string{"add", "file.txt"}); err != nil {
		t.Fatal(err)
	}

	// -q -m
	res, err := Exec(ctx, dir, []string{"commit", "-q", "-m", "first commit"})
	if err != nil || res.Stdout != "" {
		t.Fatalf("commit -q -m: %v (stdout: %q)", err, res.Stdout)
	}

	// clustered -qm
	_ = os.WriteFile(f, []byte("line2\n"), 0o644)
	if _, err := Exec(ctx, dir, []string{"add", "file.txt"}); err != nil {
		t.Fatal(err)
	}
	res, err = Exec(ctx, dir, []string{"commit", "-qm", "second commit"})
	if err != nil || res.Stdout != "" {
		t.Fatalf("commit -qm: %v (stdout: %q)", err, res.Stdout)
	}

	// clustered -qam
	_ = os.WriteFile(f, []byte("line3\n"), 0o644)
	res, err = Exec(ctx, dir, []string{"commit", "-qam", "third commit"})
	if err != nil || res.Stdout != "" {
		t.Fatalf("commit -qam: %v (stdout: %q)", err, res.Stdout)
	}

	// --quiet -m
	_ = os.WriteFile(f, []byte("line4\n"), 0o644)
	if _, err := Exec(ctx, dir, []string{"add", "file.txt"}); err != nil {
		t.Fatal(err)
	}
	res, err = Exec(ctx, dir, []string{"commit", "--quiet", "-m", "fourth commit"})
	if err != nil || res.Stdout != "" {
		t.Fatalf("commit --quiet -m: %v (stdout: %q)", err, res.Stdout)
	}

	// verify 4 commits in log
	logRes, err := Exec(ctx, dir, []string{"log", "--oneline"})
	if err != nil {
		t.Fatalf("log --oneline: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(logRes.Stdout), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 commits, got %d:\n%s", len(lines), logRes.Stdout)
	}
}

func TestNativeConfigOverrides_ReadAndCommand(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	if _, err := Exec(ctx, dir, []string{"init", "-q", "."}); err != nil {
		t.Fatalf("init: %v", err)
	}

	// -c override query via config
	res, err := Exec(ctx, dir, []string{"-c", "agent.name=Genie", "config", "agent.name"})
	if err != nil {
		t.Fatalf("config query with -c: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "Genie" {
		t.Errorf("expected 'Genie', got %q", res.Stdout)
	}

	res, err = Exec(ctx, dir, []string{"-c", "user.name=Alice", "-c", "user.email=alice@example.test", "config", "user.name"})
	if err != nil {
		t.Fatalf("config user.name: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "Alice" {
		t.Errorf("expected 'Alice', got %q", res.Stdout)
	}

	// Malformed -c
	res, err = Exec(ctx, dir, []string{"-c", "badformat", "status"})
	if err != nil {
		t.Fatalf("unexpected go error for bad -c: %v", err)
	}
	if res.ExitCode != 128 {
		t.Errorf("expected exit code 128 for bad -c format, got %d", res.ExitCode)
	}
}

func TestNativeLog_OnelineMergeCommit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	if _, err := Exec(ctx, dir, []string{"init", "-q", "."}); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Initial commit on main
	f1 := filepath.Join(dir, "f1.txt")
	_ = os.WriteFile(f1, []byte("f1\n"), 0o644)
	if _, err := Exec(ctx, dir, []string{"add", "."}); err != nil {
		t.Fatal(err)
	}
	if _, err := Exec(ctx, dir, []string{"-c", "user.name=Tester", "-c", "user.email=t@e.com", "commit", "-qm", "initial"}); err != nil {
		t.Fatal(err)
	}

	// Branch "feature"
	if _, err := Exec(ctx, dir, []string{"checkout", "-b", "feature"}); err != nil {
		t.Fatal(err)
	}
	f2 := filepath.Join(dir, "f2.txt")
	_ = os.WriteFile(f2, []byte("f2\n"), 0o644)
	if _, err := Exec(ctx, dir, []string{"add", "."}); err != nil {
		t.Fatal(err)
	}
	if _, err := Exec(ctx, dir, []string{"-c", "user.name=Tester", "-c", "user.email=t@e.com", "commit", "-qm", "feature commit"}); err != nil {
		t.Fatal(err)
	}

	// Switch back to main (or master) and create another commit
	headRef, _ := Exec(ctx, dir, []string{"rev-parse", "--abbrev-ref", "HEAD"})
	featureBranch := strings.TrimSpace(headRef.Stdout)
	mainBranch := "main"
	if featureBranch != "feature" {
		t.Fatalf("expected to be on feature branch, on %s", featureBranch)
	}
	// find main branch name
	brList, _ := Exec(ctx, dir, []string{"branch"})
	for _, l := range strings.Split(brList.Stdout, "\n") {
		name := strings.TrimSpace(strings.TrimPrefix(l, "*"))
		if name != "" && name != "feature" {
			mainBranch = name
			break
		}
	}

	if _, err := Exec(ctx, dir, []string{"checkout", mainBranch}); err != nil {
		t.Fatal(err)
	}

	// Commit on main so merge is a true merge commit (--no-ff)
	f3 := filepath.Join(dir, "f3.txt")
	_ = os.WriteFile(f3, []byte("f3\n"), 0o644)
	if _, err := Exec(ctx, dir, []string{"add", "."}); err != nil {
		t.Fatal(err)
	}
	if _, err := Exec(ctx, dir, []string{"-c", "user.name=Tester", "-c", "user.email=t@e.com", "commit", "-qm", "main branch commit"}); err != nil {
		t.Fatal(err)
	}

	// Merge feature (--no-ff)
	if _, err := Exec(ctx, dir, []string{"-c", "user.name=Tester", "-c", "user.email=t@e.com", "merge", "--no-ff", "feature"}); err != nil {
		t.Fatalf("merge --no-ff feature: %v", err)
	}

	// Now run git log --oneline
	res, err := Exec(ctx, dir, []string{"log", "--oneline"})
	if err != nil {
		t.Fatalf("git log --oneline failed: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("git log --oneline exit code = %d, stderr = %s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "feature commit") || !strings.Contains(res.Stdout, "initial") {
		t.Errorf("expected commits in log --oneline output:\n%s", res.Stdout)
	}

	// log --oneline -1
	res1, err := Exec(ctx, dir, []string{"log", "--oneline", "-1"})
	if err != nil {
		t.Fatalf("git log --oneline -1 failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(res1.Stdout), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 line, got %d", len(lines))
	}

	// log --oneline HEAD
	resHead, err := Exec(ctx, dir, []string{"log", "--oneline", "HEAD"})
	if err != nil {
		t.Fatalf("git log --oneline HEAD failed: %v", err)
	}
	if resHead.ExitCode != 0 {
		t.Fatalf("git log --oneline HEAD exit code = %d", resHead.ExitCode)
	}
}

func TestNativeLog_EmptyRepo(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	if _, err := Exec(ctx, dir, []string{"init", "-q", "."}); err != nil {
		t.Fatalf("init: %v", err)
	}

	res, err := Exec(ctx, dir, []string{"log", "--oneline"})
	if err != nil {
		t.Fatalf("expected Exec not to return Go error for empty repo log, got %v", err)
	}
	if res.ExitCode != 128 {
		t.Errorf("expected exit code 128 for empty repo log, got %d", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "does not have any commits yet") {
		t.Errorf("expected stderr to mention 'does not have any commits yet', got %q", res.Stderr)
	}
}
