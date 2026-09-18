// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package handoff

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "t@t")
	git(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "base")
	return dir
}

// The whole feature in one test: a session leaves work in flight, the record
// TRAVELS (we never touch the origin repo again — it is simulated by a second,
// independent clone at the same base), and the successor reconstitutes the exact
// working tree. If this passes, "cross-tool, cross-machine" is not a claim, it is
// a property.
func TestCaptureTravelsAndApplies(t *testing.T) {
	origin := newRepo(t)

	// In-flight work of every kind a real session has:
	//   a modified tracked file (unstaged),
	//   a modified tracked file that was STAGED (the index — the thing that broke us),
	//   a brand-new untracked file (routinely the entire point).
	if err := os.WriteFile(filepath.Join(origin, "tracked.txt"), []byte("v2-unstaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "staged.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(origin, "new.txt"), []byte("brand new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ws, err := CaptureWork(origin)
	if err != nil {
		t.Fatalf("CaptureWork: %v", err)
	}
	if ws.Clean {
		t.Fatal("captured Clean=true with work in flight")
	}
	if ws.BaseSHA == "" {
		t.Fatal("no BaseSHA: a successor could not tell what the diff applies to")
	}

	// Capture must be a READ. An agent being killed mid-edit must not have its
	// work moved by the very command meant to preserve it.
	if got := readFile(t, filepath.Join(origin, "tracked.txt")); got != "v2-unstaged\n" {
		t.Fatalf("capture mutated the origin tree: tracked.txt = %q", got)
	}

	// The record travels. Build the successor from scratch at the same base —
	// no shared stash, no shared index, no path back to the origin.
	successor := newRepo(t)

	if err := Apply(ws, successor); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The staged file arrives as CONTENT, not as an index entry: handoff carries
	// what was CHANGED, not what someone had half-decided to commit.
	for path, want := range map[string]string{
		"tracked.txt": "v2-unstaged\n",
		"staged.txt":  "staged\n",
		"new.txt":     "brand new\n",
	} {
		if got := readFile(t, filepath.Join(successor, path)); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

// A clean tree is an honest, common case — "I am stopping for the day and I have
// committed everything" is a handoff too.
func TestCaptureClean(t *testing.T) {
	repo := newRepo(t)
	ws, err := CaptureWork(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !ws.Clean {
		t.Fatalf("clean repo captured as dirty: diff=%q untracked=%v", ws.Diff, ws.Untracked)
	}
	if err := Apply(ws, newRepo(t)); err != nil {
		t.Fatalf("applying a clean handoff must be a no-op, got: %v", err)
	}
}

// Applying onto a dirty tree must REFUSE. Landing a patch on top of someone
// else's uncommitted edits manufactures a conflict that neither agent
// understands and neither can attribute — which is precisely the failure this
// whole feature exists to end.
func TestApplyRefusesDirtyTree(t *testing.T) {
	origin := newRepo(t)
	if err := os.WriteFile(filepath.Join(origin, "tracked.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := CaptureWork(origin)
	if err != nil {
		t.Fatal(err)
	}

	target := newRepo(t)
	if err := os.WriteFile(filepath.Join(target, "tracked.txt"), []byte("someone else was here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = Apply(ws, target)
	if err == nil {
		t.Fatal("applied onto a dirty tree — must refuse")
	}
	if !strings.Contains(err.Error(), "dirty tree") {
		t.Fatalf("wrong refusal: %v", err)
	}
}

// Pending must find a handoff by path-set INTERSECTION, not root equality. A
// session that handed off while working across three repos must be discoverable
// by an agent that later opens any ONE of them — the exact case a per-repo key
// would miss, and the exact shape of the regression that prompted this work.
func TestPendingIntersectsAcrossRepos(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	r := &Record{
		ID:        NewID(now, "/w/bashy"),
		CreatedAt: now,
		Project: Project{
			Name: "bashy", Primary: "/w/bashy",
			Roots: []string{"/w/bashy", "/w/sh", "/w/coreutils"},
		},
		Continuity: "mid-refactor",
		Dispatch:   Dispatch{Disposition: DispatchPark},
	}
	if _, err := Save(dir, r); err != nil {
		t.Fatal(err)
	}

	// An agent opening the SIBLING repo must be told.
	got, err := Pending(dir, []string{"/w/sh"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("a handoff spanning 3 repos was invisible from a sibling: got %d", len(got))
	}

	// And from a SUBDIRECTORY of a member: an agent in <repo>/internal is working
	// in <repo>.
	if got, _ = Pending(dir, []string{"/w/coreutils/pkg/weave"}); len(got) != 1 {
		t.Fatalf("invisible from a subdirectory of a member repo: got %d", len(got))
	}

	// An unrelated project must NOT see it.
	if got, _ = Pending(dir, []string{"/w/somethingelse"}); len(got) != 0 {
		t.Fatalf("leaked into an unrelated project: got %d", len(got))
	}

	// A resumed handoff is no longer pending — it cannot be silently picked up twice.
	ts := time.Now()
	r.ResumedAt = &ts
	if _, err := Save(dir, r); err != nil {
		t.Fatal(err)
	}
	if got, _ = Pending(dir, []string{"/w/bashy"}); len(got) != 0 {
		t.Fatalf("a resumed handoff is still pending: got %d", len(got))
	}
}

// A record must be loadable from ANY path — one that arrived by scp or over the
// mesh, never filed into this host's store. That is the artifact-not-pointer rule.
func TestLoadFromArbitraryPath(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	r := &Record{ID: NewID(now, "/w/x"), CreatedAt: now, Continuity: "brief"}
	path, err := Save(dir, r)
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "arrived-by-scp.json")
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(elsewhere, b, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(elsewhere)
	if err != nil {
		t.Fatalf("a record that travelled could not be read: %v", err)
	}
	if got.Continuity != "brief" {
		t.Fatalf("continuity lost in transit: %q", got.Continuity)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}
