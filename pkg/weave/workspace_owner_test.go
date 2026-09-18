package weave

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A weave command run from INSIDE a workspace must resolve to the queue that
// created it, not mint a fresh root keyed on the clone's path.
//
// The regression this pins is not hypothetical: ~/.bashy/weave had accumulated
// 237 empty roots (204 named issue-N-<hash>, 31 <repo>-<hash>) because every
// agent that ran a weave command inside its own workspace forked a root. They
// were empty precisely BECAUSE they were wrong — the real queue stayed behind
// in the dispatching root, so nothing ever detected the fork.
func TestWeaveQueueDirResolvesUpwardFromAWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	owner := filepath.Join(weaveStateRoot(home), "coreutils-909dd8b2")
	for _, sub := range []string{"workspaces", "sandboxes"} {
		if err := os.MkdirAll(filepath.Join(owner, sub, "issue-29"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, sub := range []string{"workspaces", "sandboxes"} {
		ws := filepath.Join(owner, sub, "issue-29")
		got, err := weaveQueueDir(ws)
		if err != nil {
			t.Fatalf("weaveQueueDir(%s): %v", sub, err)
		}
		if got != owner {
			t.Fatalf("%s: queue dir = %q, want the owning root %q", sub, got, owner)
		}
	}

	// And an ordinary repo still gets its own root, keyed by basename+hash.
	repo := filepath.Join(home, "projects", "coreutils")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := weaveQueueDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got == owner {
		t.Fatal("an ordinary repo must not resolve to another repo's weave root")
	}
	if filepath.Dir(got) != weaveStateRoot(home) {
		t.Fatalf("ordinary repo root %q is not directly under the state root", got)
	}

	// A directory that merely lives under the state root but is not a workspace
	// (no workspaces/ or sandboxes/ segment) must NOT be swallowed by a root.
	stray := filepath.Join(weaveStateRoot(home), "coreutils-909dd8b2", "logs")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, _ := weaveQueueDir(stray); got == owner {
		t.Fatal("a non-workspace path under the state root must not resolve to the owner")
	}
}

// prune sweeps state roots that hold no queue at all — the forks the old
// path-keyed weaveQueueDir minted — but only those.
func TestWeaveSweepEmptyStateRoots(t *testing.T) {
	home := t.TempDir()
	root := weaveStateRoot(home)
	now := time.Now()
	old := now.Add(-24 * time.Hour)

	mk := func(name string, files map[string]string, mod time.Time) string {
		d := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(d, "workspaces"), 0o755); err != nil {
			t.Fatal(err)
		}
		for rel, body := range files {
			p := filepath.Join(d, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chtimes(d, mod, mod); err != nil {
			t.Fatal(err)
		}
		return d
	}

	fork := mk("issue-29-0517bf6c", nil, old)      // swept
	deepFork := mk("coreutils-deadbeef", nil, old) // swept
	withQueue := mk("coreutils-909dd8b2", map[string]string{"queue.json": "{}"}, old)
	onlyMemory := mk("sh-7e2e7b65", map[string]string{"memory.jsonl": "{}\n"}, old)
	onlyLog := mk("bashy-6497d06f", map[string]string{"logs/issue-1.log": "x"}, old)
	young := mk("outpost-00000000", nil, now) // inside the grace window
	keep := mk("dhnt-31437fad", nil, old)     // the caller's own root

	swept := weaveSweepEmptyStateRoots(home, keep, now)

	got := map[string]bool{}
	for _, s := range swept {
		got[s] = true
	}
	for _, want := range []string{"issue-29-0517bf6c", "coreutils-deadbeef"} {
		if !got[want] {
			t.Errorf("%s holds no queue and should have been swept; swept=%v", want, swept)
		}
	}
	for name, d := range map[string]string{
		"a root with a queue":            withQueue,
		"a root with only memory":        onlyMemory,
		"a root with only a log":         onlyLog,
		"a root inside the grace window": young,
		"the caller's own root":          keep,
	} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("%s must survive prune, but it is gone (%s)", name, filepath.Base(d))
		}
	}
	if _, err := os.Stat(fork); err == nil {
		t.Error("the swept fork is still on disk")
	}
	if _, err := os.Stat(deepFork); err == nil {
		t.Error("the swept clone-root is still on disk")
	}
}
