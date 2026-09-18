package weave

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// pinPassthroughJudge makes the fail-closed judge vet a no-op for tests that
// exercise the pair RUNNER directly (they stub weavePairReviewRunner and pass a
// placeholder reviewer name). It echoes the requested reviewer back as eligible,
// so those tests keep testing the runner/verdict path rather than the fleet
// catalog's band/family eligibility, which has its own dedicated tests.
func pinPassthroughJudge(t *testing.T) {
	t.Helper()
	prev := weaveVetJudge
	weaveVetJudge = func(requested string, it *weaveItem) (string, string, error) {
		coder, _, _ := weaveCodingIdentity(it)
		return requested, coder, nil
	}
	t.Cleanup(func() { weaveVetJudge = prev })
}

func TestWeavePullPairEvidenceBlocksMerge(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	pinPassthroughJudge(t)
	if out, code := runWeave(t, "add", "buggy work", "--verify", "test ! -f adversarial_test.go", "--json"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	script := `set -e
echo buggy > feature.txt
git add feature.txt
git -c user.email=a@a -c user.name=a commit -qm "buggy feature"`
	if out, code := runWeave(t, "start", "--issue", "1", "--json", "--", "sh", "-c", script); code != 0 {
		t.Fatalf("start exit=%d: %s", code, out)
	}

	original := weavePairReviewRunner
	t.Cleanup(func() { weavePairReviewRunner = original })
	weavePairReviewRunner = func(workspace, diffRef, gateCommand, requested string, it *weaveItem) (weavePairReviewResult, error) {
		if requested != "reviewer" || diffRef == "" || gateCommand == "" {
			t.Fatalf("pair args requested=%q diff=%q gate=%q", requested, diffRef, gateCommand)
		}
		if err := os.WriteFile(filepath.Join(workspace, "adversarial_test.go"), []byte("package adversarial\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitT(t, workspace, "add", "adversarial_test.go")
		gitT(t, workspace, "commit", "-qm", "pair proof")
		return weavePairReviewResult{
			CodingAgent: "sh", ReviewAgent: "codex:gpt", AddedTest: true,
			Verdict: weavePairRefuted, Reason: "pair proved the regression", ExitCode: weavePairProvedExit,
		}, nil
	}

	out, code := runWeave(t, "pull", "1", "--review-agent", "reviewer", "--json")
	if code != weavePairProvedExit {
		t.Fatalf("pull exit=%d: %s", code, out)
	}
	if !strings.Contains(out, `"status": "review-block"`) || !strings.Contains(out, `"pair_verdict": "refuted"`) || !strings.Contains(out, `"review_added_test": true`) {
		t.Fatalf("pair proof did not block merge with durable evidence: %s", out)
	}
	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	it := findWeaveItem(q, 1)
	if it.State != "failed" || it.CodingAgent != "sh" || it.ReviewAgent != "codex:gpt" || !it.ReviewAddedTest {
		t.Fatalf("item evidence/state = %#v", it)
	}
	if got := gitT(t, it.Workspace, "show", "--format=", "--name-only", "HEAD"); !strings.Contains(got, "adversarial_test.go") {
		t.Fatalf("pair proof was not committed: %s", got)
	}
	if got := gitT(t, root, "log", "--format=%s", "-1"); got != "seed" {
		t.Fatalf("buggy branch merged despite red gate: %s", got)
	}
}

func TestWeavePullReviewOptInAndCleanMerge(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	pinPassthroughJudge(t)
	if out, code := runWeave(t, "add", "clean work", "--verify", "test -f feature.txt", "--json"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	script := `set -e
echo clean > feature.txt
git add feature.txt
git -c user.email=a@a -c user.name=a commit -qm "clean feature"`
	if out, code := runWeave(t, "start", "--issue", "1", "--json", "--", "sh", "-c", script); code != 0 {
		t.Fatalf("start exit=%d: %s", code, out)
	}

	original := weavePairReviewRunner
	originalObserve := weaveTestObservePullLockHeld
	t.Cleanup(func() {
		weavePairReviewRunner = original
		weaveTestObservePullLockHeld = originalObserve
	})
	called := 0
	var lockHeld time.Duration
	weaveTestObservePullLockHeld = func(d time.Duration) { lockHeld = d }
	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	weavePairReviewRunner = func(workspace, diffRef, gateCommand, requested string, it *weaveItem) (weavePairReviewResult, error) {
		called++
		assertPullLockFree(t, dir)
		return weavePairReviewResult{
			CodingAgent: "sh", ReviewAgent: "claude:opus", AddedTest: false,
			Verdict: weavePairPass, Reason: "pair attacked the change and the gate stayed green", ExitCode: weavePairPassExit,
		}, nil
	}
	out, code := runWeave(t, "pull", "1", "--review-agent", "reviewer", "--json")
	if code != 0 || !strings.Contains(out, `"status": "merged"`) || !strings.Contains(out, `"pair_verdict": "pass"`) || !strings.Contains(out, `"pair_reason":`) {
		t.Fatalf("clean reviewed run did not merge (exit %d): %s", code, out)
	}
	if called != 1 {
		t.Fatalf("pair calls=%d, want 1", called)
	}
	if lockHeld <= 0 {
		t.Fatal("merge lock hold was not observed")
	}
	t.Logf("measured pull.lock hold after review: %s", lockHeld)
}

func TestWeavePullRefusesVerdictWhenBaseMovesDuringReview(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	pinPassthroughJudge(t)
	if out, code := runWeave(t, "add", "reviewed against old base", "--verify", "test -f feature.txt", "--json"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	script := `set -e
echo candidate > feature.txt
git add feature.txt
git -c user.email=a@a -c user.name=a commit -qm "candidate feature"`
	if out, code := runWeave(t, "start", "--issue", "1", "--json", "--", "sh", "-c", script); code != 0 {
		t.Fatalf("start exit=%d: %s", code, out)
	}

	original := weavePairReviewRunner
	t.Cleanup(func() { weavePairReviewRunner = original })
	weavePairReviewRunner = func(workspace, diffRef, gateCommand, requested string, it *weaveItem) (weavePairReviewResult, error) {
		if err := os.WriteFile(filepath.Join(root, "peer.txt"), []byte("peer\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitT(t, root, "add", "peer.txt")
		gitT(t, root, "commit", "-qm", "peer moved base")
		return weavePairReviewResult{
			CodingAgent: "sh", ReviewAgent: "claude:opus",
			Verdict: weavePairPass, Reason: "candidate passed against the old base", ExitCode: weavePairPassExit,
		}, nil
	}

	out, code := runWeave(t, "pull", "1", "--review-agent", "reviewer", "--json")
	if code == 0 {
		t.Fatalf("stale verdict merged after base moved: %s", out)
	}
	if !strings.Contains(out, "merge target moved") || !strings.Contains(out, "verdict is stale") {
		t.Fatalf("refusal did not name the moved evidence target: %s", out)
	}
	if got := gitT(t, root, "log", "--format=%s", "-1"); got != "peer moved base" {
		t.Fatalf("candidate merged on stale verdict; base tip is %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "feature.txt")); !os.IsNotExist(err) {
		t.Fatalf("candidate file landed despite stale verdict: stat err=%v", err)
	}
}

func TestWeavePullPairHarnessErrorDoesNotMerge(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	pinPassthroughJudge(t)
	if out, code := runWeave(t, "add", "review harness regression", "--verify", "test -f feature.txt", "--json"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	script := `set -e
echo clean > feature.txt
git add feature.txt
git -c user.email=a@a -c user.name=a commit -qm "clean feature"`
	if out, code := runWeave(t, "start", "--issue", "1", "--json", "--", "sh", "-c", script); code != 0 {
		t.Fatalf("start exit=%d: %s", code, out)
	}

	original := weavePairReviewRunner
	t.Cleanup(func() { weavePairReviewRunner = original })
	weavePairReviewRunner = func(workspace, diffRef, gateCommand, requested string, it *weaveItem) (weavePairReviewResult, error) {
		return weavePairReviewResult{
			CodingAgent: "sh",
			ReviewAgent: "opencode:deepseek-v4-pro",
		}, errors.New("pair opencode:deepseek-v4-pro: context deadline exceeded")
	}

	out, code := runWeave(t, "pull", "1", "--review-agent", "reviewer", "--json")
	if code != weavePairHarnessErrorExit {
		t.Fatalf("pull exit=%d, want harness exit %d: %s", code, weavePairHarnessErrorExit, out)
	}
	if !strings.Contains(out, `"pair_verdict": "harness-error"`) || !strings.Contains(out, "context deadline exceeded") {
		t.Fatalf("missing HARNESS-ERROR verdict and reason: %s", out)
	}
	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	it := findWeaveItem(q, 1)
	if it.State != "submitted" || !it.NeedsSteward || !strings.Contains(it.StewardReason, "PAIR HARNESS-ERROR") {
		t.Fatalf("harness error must leave a named submitted run: %#v", it)
	}
	if got := gitT(t, root, "log", "--format=%s", "-1"); got != "seed" {
		t.Fatalf("run merged despite harness error: %s", got)
	}
	plain, plainCode := runWeave(t, "pull", "1", "--review-agent", "reviewer", "--plain")
	if plainCode != weavePairHarnessErrorExit || !strings.Contains(plain, "PAIR HARNESS-ERROR — pair opencode:deepseek-v4-pro: context deadline exceeded") {
		t.Fatalf("plain pull did not print the harness verdict and reason (exit %d): %s", plainCode, plain)
	}
}

func TestWeavePullIgnoresRecordedHarnessErrorWithoutReviewer(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	if out, code := runWeave(t, "add", "review retry required", "--verify", "test -f feature.txt", "--json"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	script := `set -e
echo clean > feature.txt
git add feature.txt
git -c user.email=a@a -c user.name=a commit -qm "clean feature"`
	if out, code := runWeave(t, "start", "--issue", "1", "--json", "--", "sh", "-c", script); code != 0 {
		t.Fatalf("start exit=%d: %s", code, out)
	}
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

	out, code := runWeave(t, "pull", "1", "--plain")
	if code != 0 || !strings.Contains(out, "merged") {
		t.Fatalf("bare deterministic pull was poisoned by stale pair evidence (exit %d): %s", code, out)
	}
	if strings.Contains(out, "PAIR HARNESS-ERROR") || strings.Contains(out, "context deadline exceeded") {
		t.Fatalf("bare pull presented stale pair evidence as part of this invocation: %s", out)
	}
	if got := gitT(t, root, "log", "--format=%s", "-1"); got == "seed" {
		t.Fatalf("bare deterministic pull did not merge: %s", got)
	}
}

func TestWeaveSelectReviewAgentReplacesCoder(t *testing.T) {
	agents, _ := fleetCatalog().Agents()
	if len(agents) < 2 {
		t.Skip("fleet registry has fewer than two agents")
	}
	coder := agents[0]
	it := &weaveItem{Tool: coder.Tool, LaunchSpec: &weaveLaunchSpec{Tool: coder.Tool, Agent: coder.Name, Model: coder.Model}}
	reviewer, coding, err := weaveSelectReviewAgent(coder.Name, it)
	if err != nil {
		t.Fatal(err)
	}
	if reviewer == "" || reviewer == coding || reviewer == coder.Tool+":"+coder.Model {
		t.Fatalf("self-review was not replaced: coder=%q reviewer=%q", coding, reviewer)
	}
}

func TestReviewAgentIsDefaultOffAndThreadsIntoAutopilot(t *testing.T) {
	for name, cmd := range map[string]*cobra.Command{
		"pull":      newWeavePullCmd(),
		"autopilot": newWeaveAutopilotCmd(),
		"heartbeat": newWeaveHeartbeatCmd(),
	} {
		flag := cmd.Flag("review-agent")
		if flag == nil || flag.DefValue != "" {
			t.Fatalf("%s --review-agent default = %#v, want present and off", name, flag)
		}
	}

	dir := t.TempDir()
	if err := saveWeaveQueue(dir, &weaveQueue{}); err != nil {
		t.Fatal(err)
	}
	prompt, err := buildWeaveAutopilotPrompt(t.TempDir(), dir, "", "codex:gpt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "pull <issue> --review-agent codex:gpt") {
		t.Fatalf("autopilot did not receive fleet-wide review requirement: %s", prompt)
	}
}
