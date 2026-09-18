package weave

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// RESOLVING a queue dir must not CREATE it.
//
// The regression: weaveQueueDir ended in an unconditional MkdirAll, and every
// read path goes through it — `weave list`, the dashboard collectors, and
// the conductor-address lookup behind `bashy mb`. So merely LOOKING at a repo
// minted a permanent ~/.bashy/weave/<repo>-<hash> tag for it, one per working
// directory anyone ever read from. They were empty, indistinguishable at a
// glance from a real workspace, and nothing reported creating them.
//
// This is the narrow sibling of TestWeaveQueueDirResolvesUpwardFromAWorkspace:
// that one pins WHICH root a path resolves to, this one pins that resolving
// alone writes nothing.
func TestResolvingAQueueDirCreatesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	repo := filepath.Join(home, "projects", "widget")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	dir, err := weaveQueueDir(repo)
	if err != nil {
		t.Fatalf("weaveQueueDir: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("resolving created %s (stat err = %v); a read must not mint weave state", dir, err)
	}
	// Not even the state root, which would leave ~/.bashy/weave behind on a
	// host that has never run a weave.
	if _, err := os.Stat(weaveStateRoot(home)); !os.IsNotExist(err) {
		t.Fatalf("resolving created the state root %s", weaveStateRoot(home))
	}

	// An absent dir is a queue with no runs, which is the true answer for a
	// repo nobody has woven — so no read path needs the dir to exist.
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatalf("loadWeaveQueue over an absent dir: %v", err)
	}
	if q == nil || len(q.Items) != 0 {
		t.Fatalf("loadWeaveQueue over an absent dir = %+v, want an empty queue", q)
	}
}

// The conductor-address lookup is the read path that was caught doing this: it
// runs on every `bashy mb` send to resolve conductor:<n> addresses, including
// from long-lived inbox watchers, so it recreated a tag as fast as one could be
// deleted.
//
// weaveRepoRoot shells out to real git, which this repo's tests otherwise never
// require — so the in-repo case SKIPS without git rather than making the suite
// depend on it. The guarantee still holds unconditionally: the shared resolver
// is pinned by TestResolvingAQueueDirCreatesNothing above.
func TestConductorRoleLookupCreatesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	repo := filepath.Join(home, "projects", "widget")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	// Outside a repo entirely: the documented answer is "no conductor addresses
	// here", reached without writing anything.
	if got := conductorRoles(); len(got) != 0 {
		t.Fatalf("conductorRoles() outside a repo = %+v, want none", got)
	}
	if _, err := os.Stat(weaveStateRoot(home)); !os.IsNotExist(err) {
		t.Fatalf("resolving conductor addresses outside a repo created %s", weaveStateRoot(home))
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH; the in-repo half of this test needs weaveRepoRoot")
	}
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Skipf("git init: %v", err)
	}

	// Now genuinely inside a repo with no queue — the case that was minting a
	// tag on every single send.
	if got := conductorRoles(); len(got) != 0 {
		t.Fatalf("conductorRoles() = %+v, want none for a repo with no queue", got)
	}
	if _, err := os.Stat(weaveStateRoot(home)); !os.IsNotExist(err) {
		t.Fatalf("resolving conductor addresses in a repo created %s", weaveStateRoot(home))
	}
}

// The other half of the contract: a WRITE still creates the dir it needs, so
// moving the mkdir off the read path did not just break writing.
func TestWritingAQueueCreatesItsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	repo := filepath.Join(home, "projects", "widget")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := weaveQueueDir(repo)
	if err != nil {
		t.Fatal(err)
	}

	if err := saveWeaveQueue(dir, &weaveQueue{NextID: 1}); err != nil {
		t.Fatalf("saveWeaveQueue into a not-yet-created dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "queue.json")); err != nil {
		t.Fatalf("queue.json was not written: %v", err)
	}

	// And it round-trips, so the dir is a real queue and not just a directory.
	q, err := loadWeaveQueue(dir)
	if err != nil || q == nil {
		t.Fatalf("loadWeaveQueue after save = %+v, %v", q, err)
	}
}
