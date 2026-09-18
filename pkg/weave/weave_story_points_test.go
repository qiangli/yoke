package weave

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSprintLinkRequiresValidPointsBeforeStoryMutation(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	if out, code := runSprint(t, "add", "bounded sprint", "--json"); code != 0 {
		t.Fatalf("sprint add failed (exit %d): %s", code, out)
	}
	if out, code := runWeave(t, "add", "candidate", "--json"); code != 0 {
		t.Fatalf("weave add failed (exit %d): %s", code, out)
	}
	repo := filepath.Base(root)
	assertUnlinked := func(stage string) {
		t.Helper()
		storyDir, err := sprintStoreDir()
		if err != nil {
			t.Fatal(err)
		}
		q, err := loadWeaveQueue(storyDir)
		if err != nil {
			t.Fatal(err)
		}
		s := findWeaveStory(q, 1)
		if s == nil || len(s.Runs) != 0 {
			t.Fatalf("%s mutated sprint links: %+v", stage, s)
		}
	}
	if err := validateSprintRunLink(repo, 99); err == nil || !strings.Contains(err.Error(), "no weave run #99") {
		t.Fatalf("missing-run validation: %v", err)
	}
	out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "99")
	if code == 0 || !strings.Contains(out, "no weave run #99") {
		t.Fatalf("missing-run link exit=%d output=%q", code, out)
	}
	assertUnlinked("missing-run rejection")

	if err := validateSprintRunLink(repo, 1); err == nil || !strings.Contains(err.Error(), "no story points") {
		t.Fatalf("unpointed validation: %v", err)
	}
	out, code = runSprint(t, "link", "1", "--repo", repo, "--task", "1")
	if code == 0 || !strings.Contains(out, "no story points") {
		t.Fatalf("unpointed link exit=%d output=%q", code, out)
	}
	assertUnlinked("unpointed rejection")

	dir, _ := weaveQueueDir(root)
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		findWeaveItem(q, 1).Points = 4 // simulate corrupt legacy state
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := validateSprintRunLink(repo, 1); err == nil || !strings.Contains(err.Error(), "invalid story points 4") {
		t.Fatalf("invalid-point validation: %v", err)
	}
	out, code = runSprint(t, "link", "1", "--repo", repo, "--task", "1")
	if code == 0 || !strings.Contains(out, "invalid story points 4") {
		t.Fatalf("invalid-point link exit=%d output=%q", code, out)
	}
	assertUnlinked("invalid-point rejection")

	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		findWeaveItem(q, 1).Points = 2
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "1"); code != 0 {
		t.Fatalf("valid pointed link failed (exit %d): %s", code, out)
	}
	storyDir, _ := sprintStoreDir()
	q, _ := loadWeaveQueue(storyDir)
	s := findWeaveStory(q, 1)
	if s == nil || len(s.Runs) != 1 {
		t.Fatalf("valid link missing: %+v", s)
	}
	got := s.Runs[0]
	if got.Repo != repo || got.ID != 1 || got.Queue != filepath.Base(dir) {
		t.Fatalf("valid link identity wrong: %+v", got)
	}
	// Born is the GENERATION discriminator — ids are queue-local and recycled,
	// so a link without one cannot tell a fresh run from a retired one that
	// happened to reuse the number. Asserted as non-zero rather than by struct
	// equality: the value is a wall clock, and pinning it would only re-break
	// this test the next time the record grows a field.
	if got.Born.IsZero() {
		t.Fatalf("link did not record the run's generation: %+v", got)
	}
}

func TestSprintLinkRejectsClaimedRunWithoutCompliantBudget(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	if out, code := runSprint(t, "add", "bounded sprint", "--json"); code != 0 {
		t.Fatalf("sprint add failed (exit %d): %s", code, out)
	}
	if out, code := runWeave(t, "add", "candidate", "--points", "2", "--json"); code != 0 {
		t.Fatalf("weave add failed (exit %d): %s", code, out)
	}
	repo := filepath.Base(root)
	dir, _ := weaveQueueDir(root)
	setClaimed := func(spec *weaveLaunchSpec) {
		t.Helper()
		if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
			it := findWeaveItem(q, 1)
			it.State = "working"
			it.LaunchSpec = spec
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertUnlinked := func(stage string) {
		t.Helper()
		storyDir, _ := sprintStoreDir()
		q, err := loadWeaveQueue(storyDir)
		if err != nil {
			t.Fatal(err)
		}
		if s := findWeaveStory(q, 1); s == nil || len(s.Runs) != 0 {
			t.Fatalf("%s mutated sprint links: %+v", stage, s)
		}
	}

	for _, tc := range []struct {
		name string
		spec *weaveLaunchSpec
		want string
	}{
		{name: "missing", want: "no bounded launch runtime"},
		{name: "zero", spec: &weaveLaunchSpec{}, want: "no bounded launch runtime"},
		{name: "over", spec: &weaveLaunchSpec{MaxRuntime: 8 * time.Minute}, want: "exceeds the 2-point cap 7m30s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setClaimed(tc.spec)
			out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "1")
			if code == 0 || !strings.Contains(out, tc.want) {
				t.Fatalf("link exit=%d output=%q, want %q", code, out, tc.want)
			}
			assertUnlinked(tc.name)
		})
	}

	setClaimed(&weaveLaunchSpec{MaxRuntime: 7 * time.Minute})
	if out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "1"); code != 0 {
		t.Fatalf("compliant claimed link failed (exit %d): %s", code, out)
	}
}

func TestWeavePointRejectsChangesAfterClaimWithoutMutation(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	if out, code := runWeave(t, "add", "claimed", "--points", "2", "--json"); code != 0 {
		t.Fatalf("weave add failed (exit %d): %s", code, out)
	}
	dir, _ := weaveQueueDir(root)
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it := findWeaveItem(q, 1)
		it.State = "working"
		it.LaunchSpec = &weaveLaunchSpec{MaxRuntime: 7 * time.Minute}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out, code := runWeave(t, "point", "1", "3")
	if code == 0 || !strings.Contains(out, "points may only change while state is todo") {
		t.Fatalf("point exit=%d output=%q", code, out)
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := findWeaveItem(q, 1).Points; got != 2 {
		t.Fatalf("rejected point change mutated points to %d", got)
	}
}

// TestSprintLinkAcceptsUnpointedRunWithExplicitBound pins the Sprint 175
// story filed by Sprint 171: story points derive the run's wall-clock cap
// (8 points = 30 m) and a larger explicit --max-runtime is rejected, so a
// lane that legitimately needs hours must stay unpointed — and an unlinked
// run leaves the sprint's token totals "missing". Linking promises a
// bounded execution; an explicit --max-runtime is that promise too, so an
// unpointed run links once launched under one. Unpointed runs still in
// planning, and unbounded launches, stay refused.
func TestSprintLinkAcceptsUnpointedRunWithExplicitBound(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	if out, code := runSprint(t, "add", "bounded sprint", "--json"); code != 0 {
		t.Fatalf("sprint add failed (exit %d): %s", code, out)
	}
	if out, code := runWeave(t, "add", "long lane", "--json"); code != 0 {
		t.Fatalf("weave add failed (exit %d): %s", code, out)
	}
	repo := filepath.Base(root)
	dir, _ := weaveQueueDir(root)
	setState := func(state string, spec *weaveLaunchSpec) {
		t.Helper()
		if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
			it := findWeaveItem(q, 1)
			it.State = state
			it.LaunchSpec = spec
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	linked := func() int {
		t.Helper()
		storyDir, _ := sprintStoreDir()
		q, err := loadWeaveQueue(storyDir)
		if err != nil {
			t.Fatal(err)
		}
		s := findWeaveStory(q, 1)
		if s == nil {
			return 0
		}
		return len(s.Runs)
	}

	// Still planning: no promise yet, and the message names both ways out.
	out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "1")
	if code == 0 || !strings.Contains(out, "no story points") || !strings.Contains(out, "--max-runtime") {
		t.Fatalf("unpointed todo link exit=%d output=%q", code, out)
	}
	if linked() != 0 {
		t.Fatal("unpointed todo link mutated the story")
	}

	// Launched without a bound: still refused.
	setState("working", &weaveLaunchSpec{})
	out, code = runSprint(t, "link", "1", "--repo", repo, "--task", "1")
	if code == 0 || !strings.Contains(out, "no bounded launch runtime") {
		t.Fatalf("unbounded link exit=%d output=%q", code, out)
	}
	if linked() != 0 {
		t.Fatal("unbounded link mutated the story")
	}

	// Launched with an explicit bound well past the 8-point cap: links.
	setState("working", &weaveLaunchSpec{MaxRuntime: 150 * time.Minute})
	if out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "1"); code != 0 {
		t.Fatalf("bounded unpointed link failed (exit %d): %s", code, out)
	}
	if linked() != 1 {
		t.Fatalf("bounded unpointed link did not record the run: %d runs", linked())
	}
}
