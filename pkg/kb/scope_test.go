// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package kb

import (
	"os"
	"path/filepath"
	"testing"
)

func kbGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	// FindGitRoot resolves the repo from os.Getwd(); compare against the same
	// value so the macOS /var → /private/var symlink does not cause a mismatch.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return cwd
}

func TestResolveKBDirRepoScope(t *testing.T) {
	t.Setenv("BASHY_KB_DIR", "") // ensure env does not force the host store
	root := kbGitRepo(t)
	var dir string
	label, err := resolveKBDir(&dir, false, false, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if label != "repo" || dir != filepath.Join(root, "docs", "kb") {
		t.Fatalf("repo scope = %q %q, want repo %s", label, dir, filepath.Join(root, "docs", "kb"))
	}
}

func TestResolveKBDirExplicitDirWins(t *testing.T) {
	kbGitRepo(t)
	dir := "/explicit/store"
	label, err := resolveKBDir(&dir, false, false, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if label != "dir" || dir != "/explicit/store" {
		t.Fatalf("--dir not honored: %q %q", label, dir)
	}
}

// The load-bearing safety: with BASHY_KB_DIR set, an agent that happens to be
// inside a git repo must still write to the host store — not silently start a
// docs/kb/ inside (and pollute) that repo. Env wins over repo auto-detect.
func TestResolveKBDirEnvForcesHostInsideRepo(t *testing.T) {
	host := t.TempDir()
	t.Setenv("BASHY_KB_DIR", host)
	kbGitRepo(t)
	var dir string
	label, err := resolveKBDir(&dir, false, false, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if label != "user" || dir != host {
		t.Fatalf("env did not force host store inside a repo: %q %q, want user %s", label, dir, host)
	}
}

// But an explicit --repo beats the env — the operator asked for this repo.
func TestResolveKBDirRepoFlagBeatsEnv(t *testing.T) {
	t.Setenv("BASHY_KB_DIR", t.TempDir())
	root := kbGitRepo(t)
	var dir string
	label, err := resolveKBDir(&dir, true, false, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if label != "repo" || dir != filepath.Join(root, "docs", "kb") {
		t.Fatalf("--repo did not beat env: %q %q", label, dir)
	}
}

// A committed repo store must never nest its own .git inside docs/kb/ — that
// would make git treat docs/kb/ as an embedded repo and its pages would never
// track. The parent repo is the version control.
func TestRepoStoreDoesNotNestGit(t *testing.T) {
	root := kbGitRepo(t)
	st := Open(filepath.Join(root, RepoSub))
	p := &Page{Slug: "x", Type: TypeLesson, Title: "t", Description: "d", Status: "candidate"}
	if err := st.Write(p, "add"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, RepoSub, ".git")); !os.IsNotExist(err) {
		t.Fatalf("repo store nested a .git under docs/kb/ (err=%v) — should defer to the parent repo", err)
	}
	// The pages/journal still landed (the store works; only self-versioning is skipped).
	if _, err := os.Stat(filepath.Join(root, RepoSub, "pages", "x.md")); err != nil {
		t.Fatalf("page not written: %v", err)
	}
}

// The agent ring resolves under <YCODE_DATA_DIR>/kb and, being explicit, wins
// over both the BASHY_KB_DIR host shortcut and an auto-detected repo.
func TestResolveKBDirAgentRing(t *testing.T) {
	agentData := t.TempDir()
	t.Setenv("YCODE_DATA_DIR", agentData)
	t.Setenv("BASHY_KB_DIR", t.TempDir()) // host env set...
	kbGitRepo(t)                          // ...and inside a repo — --ring agent still wins
	var dir string
	label, err := resolveKBDir(&dir, false, false, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if label != "agent" || dir != filepath.Join(agentData, "kb") {
		t.Fatalf("agent ring = %q %q, want agent %s", label, dir, filepath.Join(agentData, "kb"))
	}
}

// Without a per-agent store (no YCODE_DATA_DIR), --ring agent errors rather than
// silently falling back to another ring.
func TestResolveKBDirAgentRingNeedsAgentData(t *testing.T) {
	t.Setenv("YCODE_DATA_DIR", "")
	t.Chdir(t.TempDir())
	var dir string
	if _, err := resolveKBDir(&dir, false, false, true, ""); err == nil {
		t.Fatal("--ring agent with no agent-data dir must error")
	}
}

func TestResolveKBDirBaseDirTravel(t *testing.T) {
	t.Setenv("BASHY_KB_DIR", t.TempDir())
	kbGitRepo(t)
	other := t.TempDir()
	var dir string
	label, err := resolveKBDir(&dir, false, false, false, other)
	if err != nil {
		t.Fatal(err)
	}
	if label != "repo" || dir != filepath.Join(other, "docs", "kb") {
		t.Fatalf("--base-dir travel failed: %q %q, want repo %s", label, dir, filepath.Join(other, "docs", "kb"))
	}
}
