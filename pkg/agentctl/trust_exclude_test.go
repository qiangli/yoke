package agentctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bashy must not pollute the artifact it is judging.
//
// ApplyTrustPreseed writes opencode.json INTO the repository the agent is about to
// commit from. Caught live: a weave workspace showed
//
//	?? internal/bus/events_sink.go     <- the agent's actual work
//	?? opencode.json                   <- bashy's plumbing
//
// An agent told "commit your work" commits both, and the tool that set up the run
// ends up inside the diff it is supposed to be reviewing.
//
// .git/info/exclude, never .gitignore: local to the clone, untracked, so it cannot
// itself leak into a commit.
func TestPreseedExcludesItselfFromGit(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ApplyTrustPreseed(ws, "opencode.json"); err != nil {
		t.Fatalf("ApplyTrustPreseed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "opencode.json")); err != nil {
		t.Fatalf("preseed did not write its config: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("no .git/info/exclude — bashy's own config will land in the agent's commit: %v", err)
	}
	if !strings.Contains(string(b), "opencode.json") {
		t.Errorf("exclude does not name opencode.json:\n%s", b)
	}
}

// A weave workspace is a git WORKTREE, where .git is a FILE pointing at the real
// gitdir — so that is the common case here, not the exotic one. Following it wrong
// means the exclusion silently does nothing in exactly the place it is needed.
func TestPreseedExcludeFollowsAWorktreeGitFile(t *testing.T) {
	root := t.TempDir()
	realGit := filepath.Join(root, "realgit")
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(filepath.Join(realGit, "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".git"), []byte("gitdir: "+realGit+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ApplyTrustPreseed(ws, "opencode.json"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(realGit, "info", "exclude"))
	if err != nil {
		t.Fatalf("worktree gitdir not followed — the exclusion did nothing: %v", err)
	}
	if !strings.Contains(string(b), "opencode.json") {
		t.Errorf("exclude does not name opencode.json:\n%s", b)
	}
}

// Idempotent: a run that relaunches an agent must not append the same line forever.
func TestPreseedExcludeIsIdempotent(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := ApplyTrustPreseed(ws, "opencode.json"); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	if n := strings.Count(string(b), "opencode.json"); n != 1 {
		t.Errorf("opencode.json listed %d times, want 1:\n%s", n, b)
	}
}
