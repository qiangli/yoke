package weave

import (
	"strings"
	"testing"
)

// setupSalvageableRun builds a run that committed real work and then died in
// "killed" state — the exact shape salvage exists for, and the shape that used
// to merge to the base branch with one line of output and no review at all.
func setupSalvageableRun(t *testing.T, verify string) string {
	t.Helper()
	root := setupIsolationFixture(t)
	t.Chdir(root)
	if out, code := runWeave(t, "add", "killed mid-flight", "--verify", verify, "--json"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	script := `set -e
echo wip > feature.txt
git add feature.txt
git -c user.email=a@a -c user.name=a commit -qm "wip(weave): work preserved from a killed run"`
	if out, code := runWeave(t, "start", "--issue", "1", "--json", "--", "sh", "-c", script); code != 0 {
		t.Fatalf("start exit=%d: %s", code, out)
	}
	dir, _ := weaveQueueDir(root)
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it := findWeaveItem(q, 1)
		it.State = "killed"
		it.KilledBy = "signal terminated forwarded from wrapper"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return root
}

func salvageMainSubject(t *testing.T, root string) string {
	t.Helper()
	return strings.TrimSpace(gitT(t, root, "log", "--format=%s", "-1"))
}

// Bare salvage uses the same optional deterministic gates as pull and never
// invokes a model reviewer unless --review-agent is explicitly supplied.
func TestWeaveSalvageMergesWithoutModelReview(t *testing.T) {
	root := setupSalvageableRun(t, "test -f feature.txt")

	original := weavePairReviewRunner
	t.Cleanup(func() { weavePairReviewRunner = original })
	weavePairReviewRunner = func(workspace, diffRef, gateCommand, requested string, it *weaveItem) (weavePairReviewResult, error) {
		t.Fatalf("bare salvage ran a pair review")
		return weavePairReviewResult{}, nil
	}

	out, code := runWeave(t, "salvage", "1", "--json")
	if code != 0 || !strings.Contains(out, `"status": "merged"`) {
		t.Fatalf("bare salvage did not merge through deterministic gates (exit %d): %s", code, out)
	}
	if got := salvageMainSubject(t, root); got == "seed" {
		t.Fatalf("bare salvage left the base branch unmoved: %s", got)
	}
}

// Salvage must run the SAME adversarial gate as pull — a pair that proves a
// defect blocks the merge, exactly as it does for `weave pull --review-agent`.
func TestWeaveSalvageReviewAgentBlocksOnRefutedVerdict(t *testing.T) {
	root := setupSalvageableRun(t, "test -f feature.txt")
	pinPassthroughJudge(t)

	original := weavePairReviewRunner
	t.Cleanup(func() { weavePairReviewRunner = original })
	called := 0
	weavePairReviewRunner = func(workspace, diffRef, gateCommand, requested string, it *weaveItem) (weavePairReviewResult, error) {
		called++
		if requested != "reviewer" {
			t.Fatalf("salvage did not forward --review-agent: requested=%q", requested)
		}
		return weavePairReviewResult{
			CodingAgent: "sh", ReviewAgent: "codex:gpt", AddedTest: true,
			Verdict: weavePairRefuted, Reason: "pair proved the WIP is dead code", ExitCode: weavePairProvedExit,
		}, nil
	}

	out, code := runWeave(t, "salvage", "1", "--review-agent", "reviewer", "--json")
	if code != weavePairProvedExit {
		t.Fatalf("salvage exit=%d, want %d (pair refutation): %s", code, weavePairProvedExit, out)
	}
	if called != 1 {
		t.Fatalf("pair calls=%d, want 1", called)
	}
	if !strings.Contains(out, `"status": "review-block"`) || !strings.Contains(out, `"pair_verdict": "refuted"`) {
		t.Fatalf("salvage did not report the pair block: %s", out)
	}
	if got := salvageMainSubject(t, root); got != "seed" {
		t.Fatalf("refuted salvage merged anyway: %s", got)
	}
}

// The happy path still works: a reviewed salvage that passes both the pair and
// the verify gate merges — and the merge is attributed to the reviewer.
func TestWeaveSalvageReviewAgentMergesOnPass(t *testing.T) {
	root := setupSalvageableRun(t, "test -f feature.txt")
	pinPassthroughJudge(t)

	original := weavePairReviewRunner
	t.Cleanup(func() { weavePairReviewRunner = original })
	weavePairReviewRunner = func(workspace, diffRef, gateCommand, requested string, it *weaveItem) (weavePairReviewResult, error) {
		return weavePairReviewResult{
			CodingAgent: "sh", ReviewAgent: "codex:gpt", AddedTest: false,
			Verdict: weavePairPass, Reason: "pair attacked the WIP and the gate stayed green", ExitCode: weavePairPassExit,
		}, nil
	}

	out, code := runWeave(t, "salvage", "1", "--review-agent", "reviewer", "--json")
	if code != 0 || !strings.Contains(out, `"status": "merged"`) {
		t.Fatalf("reviewed salvage did not merge (exit %d): %s", code, out)
	}
	if !strings.Contains(out, `"pair_verdict": "pass"`) || !strings.Contains(out, `"review_agent": "codex:gpt"`) {
		t.Fatalf("merge lost its review attribution: %s", out)
	}
	if got := salvageMainSubject(t, root); got == "seed" {
		t.Fatalf("reviewed salvage left the base branch unmoved: %s", got)
	}
}

// --no-review remains a compatibility spelling. Model review is already off by
// default, so it need not emit an alarming escape warning.
func TestWeaveSalvageNoReviewOverridesStoredReviewAgent(t *testing.T) {
	root := setupSalvageableRun(t, "test -f feature.txt")
	dir, _ := weaveQueueDir(root)
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it := findWeaveItem(q, 1)
		it.ReviewAgent = "opencode:deepseek-v4-pro"
		it.PairVerdict = string(weavePairHarnessError)
		it.PairReason = "pair opencode:deepseek-v4-pro: context deadline exceeded"
		it.PairExit = weavePairHarnessErrorExit
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	original := weavePairReviewRunner
	t.Cleanup(func() { weavePairReviewRunner = original })
	weavePairReviewRunner = func(workspace, diffRef, gateCommand, requested string, it *weaveItem) (weavePairReviewResult, error) {
		t.Fatalf("--no-review ran a pair review")
		return weavePairReviewResult{}, nil
	}

	out, code := runWeave(t, "salvage", "1", "--no-review")
	if code != 0 {
		t.Fatalf("salvage --no-review exit=%d: %s", code, out)
	}
	if strings.Contains(strings.ToUpper(out), "MERGING UNREVIEWED WORK") {
		t.Fatalf("compatibility flag emitted obsolete mandatory-review warning: %s", out)
	}
	if strings.Contains(out, "PAIR HARNESS-ERROR") || strings.Contains(out, "context deadline exceeded") {
		t.Fatalf("--no-review presented stored pair evidence as though a pair ran now: %s", out)
	}
	if got := salvageMainSubject(t, root); got == "seed" {
		t.Fatalf("--no-review salvage did not merge: %s", got)
	}
}

// "inspect and merge" was false advertising: salvage never inspected anything.
// It must show the diff it is about to merge.
func TestWeaveSalvageShowsTheDiffItIsAboutToMerge(t *testing.T) {
	setupSalvageableRun(t, "test -f feature.txt")
	out, code := runWeave(t, "salvage", "1", "--no-review")
	if code != 0 {
		t.Fatalf("salvage exit=%d: %s", code, out)
	}
	if !strings.Contains(out, "feature.txt") {
		t.Fatalf("salvage merged without showing the diff: %s", out)
	}
}

// Merging and publishing are separate decisions. Salvage merges locally; the
// remote must not move.
func TestWeaveSalvageNeverPushes(t *testing.T) {
	root := setupSalvageableRun(t, "test -f feature.txt")
	remote := t.TempDir()
	gitT(t, remote, "init", "-q", "--bare", "-b", "main")
	gitT(t, root, "remote", "add", "origin", remote)
	gitT(t, root, "push", "-q", "origin", "main")
	before := strings.TrimSpace(gitT(t, remote, "rev-parse", "main"))

	if out, code := runWeave(t, "salvage", "1", "--no-review"); code != 0 {
		t.Fatalf("salvage exit=%d: %s", code, out)
	}
	if got := salvageMainSubject(t, root); got == "seed" {
		t.Fatalf("salvage did not merge locally: %s", got)
	}
	if after := strings.TrimSpace(gitT(t, remote, "rev-parse", "main")); after != before {
		t.Fatalf("salvage PUSHED unreviewed work to the remote: %s -> %s", before, after)
	}
}

// --review-agent and --no-review contradict each other; refusing beats silently
// picking one.
func TestWeaveSalvageRejectsContradictoryReviewFlags(t *testing.T) {
	root := setupSalvageableRun(t, "test -f feature.txt")
	out, code := runWeave(t, "salvage", "1", "--review-agent", "reviewer", "--no-review")
	if code == 0 {
		t.Fatalf("contradictory flags accepted: %s", out)
	}
	if got := salvageMainSubject(t, root); got != "seed" {
		t.Fatalf("base branch moved on a rejected invocation: %s", got)
	}
}

// A run that already carries a passing pair verdict has been through the gate;
// salvage must not demand a second review of the same work.
func TestWeaveSalvageAcceptsRecordedPassingVerdict(t *testing.T) {
	root := setupSalvageableRun(t, "test -f feature.txt")
	dir, _ := weaveQueueDir(root)
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it := findWeaveItem(q, 1)
		it.PairVerdict = string(weavePairPass)
		it.PairReason = "pair attacked the change and the gate stayed green"
		it.ReviewAgent = "codex:gpt"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out, code := runWeave(t, "salvage", "1")
	if code != 0 {
		t.Fatalf("salvage of already-reviewed work exit=%d: %s", code, out)
	}
	if got := salvageMainSubject(t, root); got == "seed" {
		t.Fatalf("already-reviewed salvage did not merge: %s", got)
	}
}
