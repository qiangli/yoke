package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSprintUnlinkRoundTrip is the basic inverse property: what `link` adds,
// `unlink` removes, and the card stops naming it.
func TestSprintUnlinkRoundTrip(t *testing.T) {
	_, currentRoot := sameNamedSprintRepos(t)
	addPointedRun(t, currentRoot, "the work")
	t.Chdir(currentRoot)

	if out, code := runSprint(t, "add", "a sprint"); code != 0 {
		t.Fatalf("add sprint failed (exit %d): %s", code, out)
	}
	repo := filepath.Base(currentRoot)

	if out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "1"); code != 0 {
		t.Fatalf("link failed (exit %d): %s", code, out)
	}
	if out, code := runSprint(t, "show", "1", "--plain"); code != 0 || !strings.Contains(out, repo+"#1") {
		t.Fatalf("card should list the run before unlink: exit=%d output=%q", code, out)
	}
	if out, code := runSprint(t, "unlink", "1", "--repo", repo, "--task", "1"); code != 0 ||
		!strings.Contains(out, "unlinked") {
		t.Fatalf("unlink failed (exit %d): %s", code, out)
	}
	out, code := runSprint(t, "show", "1", "--plain")
	if code != 0 {
		t.Fatalf("show failed (exit %d): %s", code, out)
	}
	if strings.Contains(out, repo+"#1") {
		t.Fatalf("card still lists the run after unlink: %q", out)
	}
}

// TestSprintUnlinkWorksWhenTheRunIsGone is THE case unlink exists for, and the
// one an implementation reusing `link`'s validating resolver would fail.
//
// A link most often needs removing precisely because the run no longer exists:
// abandoned, pruned, or its whole queue deleted. `sprint link` refuses a
// missing run by design — "repo %q has no weave run #%d" — so an unlink built
// on the same resolver could never clean up a dangling reference, which is the
// only reference anyone wants to clean up.
func TestSprintUnlinkWorksWhenTheRunIsGone(t *testing.T) {
	_, currentRoot := sameNamedSprintRepos(t)
	addPointedRun(t, currentRoot, "doomed work")
	t.Chdir(currentRoot)

	if out, code := runSprint(t, "add", "a sprint"); code != 0 {
		t.Fatalf("add sprint failed (exit %d): %s", code, out)
	}
	repo := filepath.Base(currentRoot)
	if out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "1"); code != 0 {
		t.Fatalf("link failed (exit %d): %s", code, out)
	}

	// Destroy every weave queue on the host, leaving the sprint card's link
	// pointing at nothing at all.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(home, ".bashy", "weave")); err != nil {
		t.Fatal(err)
	}

	// Guard the premise: link must genuinely be unable to see the run now.
	// Without this the test could pass while the queue was still present and
	// prove nothing about dangling references.
	if out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "1"); code == 0 {
		t.Fatalf("premise broken: the run is still resolvable, so this test proves nothing: %q", out)
	}

	if out, code := runSprint(t, "unlink", "1", "--repo", repo, "--task", "1"); code != 0 ||
		!strings.Contains(out, "unlinked") {
		t.Fatalf("unlink must not require the run to exist (exit %d): %s", code, out)
	}
	if out, code := runSprint(t, "show", "1", "--plain"); code != 0 || strings.Contains(out, repo+"#1") {
		t.Fatalf("dangling link survived unlink: exit=%d output=%q", code, out)
	}
}

// TestSprintUnlinkNoOpDoesNotClaimARemoval pins the wording. A no-op reported
// as a removal is how a stale link survives a cleanup that believed it worked.
func TestSprintUnlinkNoOpDoesNotClaimARemoval(t *testing.T) {
	_, currentRoot := sameNamedSprintRepos(t)
	addPointedRun(t, currentRoot, "unrelated work")
	t.Chdir(currentRoot)

	if out, code := runSprint(t, "add", "a sprint"); code != 0 {
		t.Fatalf("add sprint failed (exit %d): %s", code, out)
	}
	repo := filepath.Base(currentRoot)

	out, code := runSprint(t, "unlink", "1", "--repo", repo, "--task", "1")
	if code != 0 {
		t.Fatalf("unlinking an absent link should not be an error (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "nothing removed") {
		t.Fatalf("a no-op must say so explicitly, got %q", out)
	}
	if strings.Contains(out, "unlinked") {
		t.Fatalf("a no-op must NOT read as a removal, got %q", out)
	}
}

// TestSprintUnlinkReleasesTheClaim proves unlink removes a real claim and not
// merely a display row: a live sprint's link blocks another sprint from
// claiming the run, and unlinking must lift that block.
func TestSprintUnlinkReleasesTheClaim(t *testing.T) {
	_, currentRoot := sameNamedSprintRepos(t)
	addPointedRun(t, currentRoot, "contested work")
	t.Chdir(currentRoot)

	for _, title := range []string{"first sprint", "second sprint"} {
		if out, code := runSprint(t, "add", title); code != 0 {
			t.Fatalf("add sprint %q failed (exit %d): %s", title, code, out)
		}
	}
	repo := filepath.Base(currentRoot)

	if out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "1"); code != 0 {
		t.Fatalf("link failed (exit %d): %s", code, out)
	}
	if out, code := runSprint(t, "link", "2", "--repo", repo, "--task", "1"); code == 0 {
		t.Fatalf("premise broken: a live sprint's claim should block sprint #2: %q", out)
	}
	if out, code := runSprint(t, "unlink", "1", "--repo", repo, "--task", "1"); code != 0 {
		t.Fatalf("unlink failed (exit %d): %s", code, out)
	}
	if out, code := runSprint(t, "link", "2", "--repo", repo, "--task", "1"); code != 0 {
		t.Fatalf("unlink must release the claim so another sprint can take it (exit %d): %s", code, out)
	}
}

// TestSprintUnlinkRequiresRepoAndTask keeps the argument contract symmetric
// with `link` — a run is only named by repo AND id.
func TestSprintUnlinkRequiresRepoAndTask(t *testing.T) {
	_, currentRoot := sameNamedSprintRepos(t)
	t.Chdir(currentRoot)
	if out, code := runSprint(t, "add", "a sprint"); code != 0 {
		t.Fatalf("add sprint failed (exit %d): %s", code, out)
	}
	if out, code := runSprint(t, "unlink", "1", "--task", "1"); code == 0 {
		t.Fatalf("missing --repo must fail: %q", out)
	}
	if out, code := runSprint(t, "unlink", "1", "--repo", "x"); code == 0 {
		t.Fatalf("missing --task must fail: %q", out)
	}
}
