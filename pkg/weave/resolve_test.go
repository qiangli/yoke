package weave

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ref"
)

// hermeticHome points HOME/BASHY_HOME (and the sprint store) at scratch dirs so
// neither the sprint board nor any weave queue touches the real host.
func hermeticHome(t *testing.T) (home, sprintDir string) {
	t.Helper()
	home = t.TempDir()
	sprintDir = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_SPRINT_DIR", sprintDir)
	return home, sprintDir
}

// scratchQueue writes a queue.json under the machine queue root with the given
// tag, so weaveAllQueueDirs enumerates it. Root gives the queue a repo basename.
func scratchQueue(t *testing.T, home, tag, root string, items ...*weaveItem) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(weaveStateRoot(home), tag)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: items}); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSprintFound(t *testing.T) {
	_, sprintDir := hermeticHome(t)
	if err := saveWeaveQueue(sprintDir, &weaveQueue{Stories: []*weaveStory{
		{ID: 168, Title: "Uniform refs", Column: "doing"},
	}}); err != nil {
		t.Fatal(err)
	}

	g := ref.NewRegistry()
	RegisterRefs(g)

	n, err := g.Resolve("sprint:168")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if n.Kind != ref.Sprint || n.ID != "168" || n.Ref != "sprint:168" {
		t.Fatalf("identity = %+v", n)
	}
	if n.Title != "Uniform refs" || n.Status != "doing" {
		t.Errorf("title/status = %q/%q", n.Title, n.Status)
	}
	if n.Open != "bashy sprint show 168" {
		t.Errorf("open = %q", n.Open)
	}
	if n.Where != sprintDir {
		t.Errorf("where = %q, want %q", n.Where, sprintDir)
	}
}

func TestResolveSprintNotFound(t *testing.T) {
	_, sprintDir := hermeticHome(t)
	if err := saveWeaveQueue(sprintDir, &weaveQueue{Stories: []*weaveStory{
		{ID: 1, Title: "other", Column: "backlog"},
	}}); err != nil {
		t.Fatal(err)
	}
	g := ref.NewRegistry()
	RegisterRefs(g)

	_, err := g.Resolve("sprint:999")
	if !errors.Is(err, ref.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveRunFound(t *testing.T) {
	home, _ := hermeticHome(t)
	root := filepath.Join(t.TempDir(), "solo")
	scratchQueue(t, home, "solo-abc", root, &weaveItem{ID: 7, Title: "do it", State: "working"})

	g := ref.NewRegistry()
	RegisterRefs(g)

	n, err := g.Resolve("run:solo-7")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if n.Kind != ref.Run || n.ID != "solo-7" {
		t.Fatalf("identity = %+v", n)
	}
	if n.Title != "do it" || n.Status != "working" {
		t.Errorf("title/status = %q/%q", n.Title, n.Status)
	}
	if n.Where != root {
		t.Errorf("where = %q, want %q", n.Where, root)
	}
	if n.Open != "bashy weave status 7" {
		t.Errorf("open = %q", n.Open)
	}
}

func TestResolveRunNotFound(t *testing.T) {
	home, _ := hermeticHome(t)
	root := filepath.Join(t.TempDir(), "solo")
	scratchQueue(t, home, "solo-abc", root, &weaveItem{ID: 7, Title: "do it", State: "working"})

	g := ref.NewRegistry()
	RegisterRefs(g)

	if _, err := g.Resolve("run:solo-999"); !errors.Is(err, ref.ErrNotFound) {
		t.Fatalf("absent id: err = %v, want ErrNotFound", err)
	}
	if _, err := g.Resolve("run:nosuchrepo-1"); !errors.Is(err, ref.ErrNotFound) {
		t.Fatalf("absent repo: err = %v, want ErrNotFound", err)
	}
}

// Edge: two scratch queues whose repos share a basename make run:shared-3
// ambiguous — an error that names BOTH repo paths and never picks one.
func TestResolveRunAmbiguousAcrossQueues(t *testing.T) {
	home, _ := hermeticHome(t)
	rootA := filepath.Join(t.TempDir(), "shared")
	rootB := filepath.Join(t.TempDir(), "shared")
	scratchQueue(t, home, "shared-aaa", rootA, &weaveItem{ID: 3, Title: "a", State: "todo"})
	scratchQueue(t, home, "shared-bbb", rootB, &weaveItem{ID: 3, Title: "b", State: "working"})

	g := ref.NewRegistry()
	RegisterRefs(g)

	_, err := g.Resolve("run:shared-3")
	if err == nil {
		t.Fatal("two queues share a basename — resolving picked one instead of erroring")
	}
	if errors.Is(err, ref.ErrNotFound) {
		t.Fatalf("ambiguous must not be ErrNotFound: %v", err)
	}
	for _, p := range []string{rootA, rootB} {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("ambiguity error does not name repo path %s: %v", p, err)
		}
	}
}

// TestResolveRunCurrentCheckoutWins: standing INSIDE one of two same-named
// checkouts, run:<base>-<n> is the run in THIS checkout — the same answer
// `weave status <n>` gives — and the other queue is never consulted. Only from
// a neutral cwd do the two collide (TestResolveRunAmbiguousAcrossQueues).
func TestResolveRunCurrentCheckoutWins(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; weaveRepoRoot needs it")
	}
	home, _ := hermeticHome(t)
	rootA := filepath.Join(t.TempDir(), "shared")
	rootB := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(rootA, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", rootA, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	// The queue dir for rootA must be the one weaveQueueDir derives for it, so
	// the resolver's "current checkout" lookup lands on it.
	canonA, err := filepath.EvalSymlinks(rootA)
	if err != nil {
		t.Fatal(err)
	}
	tagA, _ := weaveQueueNames(weaveCanonicalRepoRoot(canonA))
	scratchQueue(t, home, tagA, canonA, &weaveItem{ID: 3, Title: "mine", State: "todo"})
	scratchQueue(t, home, "shared-other", rootB, &weaveItem{ID: 3, Title: "theirs", State: "working"})

	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(rootA); err != nil {
		t.Fatal(err)
	}

	g := ref.NewRegistry()
	RegisterRefs(g)
	n, err := g.Resolve("run:shared-3")
	if err != nil {
		t.Fatalf("inside the checkout, run:shared-3 must resolve to this checkout's run, got: %v", err)
	}
	if n.Title != "mine" || n.Where != canonA {
		t.Fatalf("resolved the wrong checkout: %+v", n)
	}
}
