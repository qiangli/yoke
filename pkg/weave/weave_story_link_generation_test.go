package weave

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// THE DEFECT THESE TESTS EXIST FOR.
//
// Weave issue ids are QUEUE-LOCAL and RECYCLED: prune a queue and the next
// `weave add` reuses the freed numbers. The sprint link table keyed a run on
// (repo, queue, id), which names a SLOT rather than a unit of work — so a
// brand-new run silently inherited the identity of a retired one and
// `sprint link` refused it as "already linked to sprint #N".
//
// It happened three times across two sprints (outpost#4, outpost#5,
// cloudbox#1 — the last reported as belonging to a sprint that had shipped
// weeks earlier). Each time the association had to be recorded in prose in the
// sprint thread because the data model could not hold it, which is precisely
// the failure a board exists to prevent.
//
// Born (the run's creation time, written once by `weave add` and never
// rewritten) is the generation discriminator.

func TestSameSprintRun_RecycledIDIsNotTheSameRun(t *testing.T) {
	oldGen := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	newGen := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	retired := sprintRun{Repo: "outpost", ID: 4, Queue: "outpost-06baad9c", Born: oldGen}
	fresh := sprintRun{Repo: "outpost", ID: 4, Queue: "outpost-06baad9c", Born: newGen}

	same, err := sameSprintRun(retired, fresh)
	if err != nil {
		t.Fatalf("sameSprintRun: %v", err)
	}
	if same {
		t.Error("a recycled id in the same queue must NOT read as the same run; " +
			"that is what refused three legitimate links")
	}
}

func TestSameSprintRun_SameGenerationIsStillTheSameRun(t *testing.T) {
	born := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	a := sprintRun{Repo: "outpost", ID: 4, Queue: "outpost-06baad9c", Born: born}
	b := sprintRun{Repo: "outpost", ID: 4, Queue: "outpost-06baad9c", Born: born}

	same, err := sameSprintRun(a, b)
	if err != nil {
		t.Fatalf("sameSprintRun: %v", err)
	}
	if !same {
		t.Error("identical generation must still dedupe — otherwise one run " +
			"could be claimed by two live sprints")
	}
}

// Legacy records carry no Born. They must keep the old fail-closed behaviour:
// without a generation on BOTH sides there is no evidence the ids differ, and
// guessing would let two live sprints claim one run.
func TestSameSprintRun_LegacyRecordStillDedupes(t *testing.T) {
	legacy := sprintRun{Repo: "outpost", ID: 4, Queue: "outpost-06baad9c"}
	fresh := sprintRun{Repo: "outpost", ID: 4, Queue: "outpost-06baad9c",
		Born: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)}

	same, err := sameSprintRun(legacy, fresh)
	if err != nil {
		t.Fatalf("sameSprintRun: %v", err)
	}
	if !same {
		t.Error("a legacy record without Born must stay fail-closed; only a " +
			"PROVEN different generation may unblock a link")
	}
}

// Born must never override repo identity: different repos commonly reuse small
// issue numbers, and that was already handled before this field existed.
func TestSameSprintRun_DifferentRepoNeverMatches(t *testing.T) {
	born := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	a := sprintRun{Repo: "outpost", ID: 1, Queue: "outpost-06baad9c", Born: born}
	b := sprintRun{Repo: "cloudbox", ID: 1, Queue: "cloudbox-deadbeef", Born: born}

	same, err := sameSprintRun(a, b)
	if err != nil {
		t.Fatalf("sameSprintRun: %v", err)
	}
	if same {
		t.Error("same id in different repos must never collide")
	}
}

// End-to-end: a run linked by a sprint that has SHIPPED must not block a new
// sprint from linking it.
//
// This is the other half of the same defect. Even with generations, a finished
// sprint holding a link means the work it touched can never be picked up by a
// successor sprint — and a `done` sprint's links are a historical record, not
// a live claim. Both sprint 84 collisions and the cloudbox one were held by
// sprints that had already closed.
//
// A run may be claimed by at most one sprint that is STILL IN PLAY. The
// finished sprint keeps its record either way; `sprint show` still lists it.
func TestSprintLink_DoneSprintDoesNotBlockANewClaim(t *testing.T) {
	_, currentRoot := sameNamedSprintRepos(t)
	addPointedRun(t, currentRoot, "the work")
	t.Chdir(currentRoot)

	for _, title := range []string{"shipped sprint", "successor sprint"} {
		if out, code := runSprint(t, "add", title); code != 0 {
			t.Fatalf("add sprint %q failed (exit %d): %s", title, code, out)
		}
	}
	repo := filepath.Base(currentRoot)

	if out, code := runSprint(t, "link", "1", "--repo", repo, "--task", "1"); code != 0 {
		t.Fatalf("first link failed (exit %d): %s", code, out)
	}
	// While sprint #1 is still in play it owns the run.
	if out, code := runSprint(t, "link", "2", "--repo", repo, "--task", "1"); code == 0 ||
		!strings.Contains(out, "already linked to sprint #1") {
		t.Fatalf("a LIVE sprint must still hold its claim: exit=%d output=%q", code, out)
	}
	// Ship it.
	if out, code := runSprint(t, "move", "1", "done"); code != 0 {
		t.Fatalf("move to done failed (exit %d): %s", code, out)
	}
	// Now the successor may claim the same run.
	if out, code := runSprint(t, "link", "2", "--repo", repo, "--task", "1"); code != 0 {
		t.Fatalf("a DONE sprint must not block a new claim (exit %d): %s", code, out)
	}
}
