package weave

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/weave/memory"
)

func TestWeaveStartCapturesMemoryObservation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("system git not available; weave lifecycle needs it")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_AGENTIC", "")

	repo := t.TempDir()
	initMemoryTestRepo(t, repo)
	t.Chdir(repo)
	if out, code := runWeave(t, "add", "touch pkg/weave/memory file", "--json"); code != 0 {
		t.Fatalf("add exit=%d out=%s", code, out)
	}
	script := "mkdir -p pkg/weave && printf 'hello\\n' > pkg/weave/memory_test_fixture.txt && git add pkg/weave/memory_test_fixture.txt && git commit -q -m memory-fixture"
	if out, code := runWeave(t, "start", "--issue", "1", "--json", "--", "sh", "-c", script); code != 0 {
		t.Fatalf("start exit=%d out=%s", code, out)
	}
	root, err := weaveRepoRoot(mustGetwd(t))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "memory.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var got memory.Observation
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatal(err)
		}
	}
	if got.IssueID != 1 || got.Outcome != "submitted" || got.Commits != 1 {
		t.Fatalf("unexpected observation: %+v", got)
	}
	if !containsString(got.FilesTouched, "pkg/weave/memory_test_fixture.txt") {
		t.Fatalf("observation missing touched file: %+v", got.FilesTouched)
	}
}

func TestWeaveRememberRecallVerbs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("system git not available; weave memory verbs need repo root")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_AGENTIC", "")

	repo := t.TempDir()
	initMemoryTestRepo(t, repo)
	t.Chdir(repo)
	if out, code := runWeave(t, "remember", "parser retry failed because token cache was stale", "--tag", "parser", "--json"); code != 0 {
		t.Fatalf("remember exit=%d out=%s", code, out)
	}
	if out, code := runWeave(t, "recall", "token cache", "--json"); code != 0 || !strings.Contains(out, "parser retry failed") {
		t.Fatalf("recall exit=%d out=%s", code, out)
	}
	root, err := weaveRepoRoot(mustGetwd(t))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "memory.jsonl")); err != nil {
		t.Fatalf("memory path resolved differently than runtime: %v", err)
	}
}

func initMemoryTestRepo(t *testing.T, repo string) {
	t.Helper()
	gitE2E(t, repo, "init", "-q", "-b", "main")
	gitE2E(t, repo, "config", "user.email", "memory@test.local")
	gitE2E(t, repo, "config", "user.name", "Memory Test")
	if err := writeFile(filepath.Join(repo, "README.md"), "hello\n"); err != nil {
		t.Fatal(err)
	}
	gitE2E(t, repo, "add", "-A")
	gitE2E(t, repo, "commit", "-q", "-m", "init")
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func containsString(vals []string, want string) bool {
	for _, v := range vals {
		if v == want {
			return true
		}
	}
	return false
}
