package weave

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	weavePairPassExit         = 0
	weavePairProvedExit       = 4
	weavePairBrokenBeforeExit = 5
	weavePairHarnessErrorExit = 6
	weavePairUnmeasuredExit   = 7
)

type weavePairVerdict string

const (
	weavePairPass         weavePairVerdict = "pass"
	weavePairBrokenBefore weavePairVerdict = "broken-before"
	weavePairUnmeasured   weavePairVerdict = "unmeasured"
	weavePairRefuted      weavePairVerdict = "refuted"
	weavePairHarnessError weavePairVerdict = "harness-error"
)

type weavePairReviewResult struct {
	CodingAgent string
	ReviewAgent string
	AddedTest   bool
	Verdict     weavePairVerdict
	Reason      string
	ExitCode    int
	Output      string
}

func (r weavePairReviewResult) verdictLine() string {
	return fmt.Sprintf("PAIR %s — %s", strings.ToUpper(string(r.Verdict)), r.Reason)
}

func weavePairExitForVerdict(verdict weavePairVerdict) (int, bool) {
	code, ok := map[weavePairVerdict]int{
		weavePairPass: weavePairPassExit, weavePairBrokenBefore: weavePairBrokenBeforeExit,
		weavePairUnmeasured: weavePairUnmeasuredExit, weavePairRefuted: weavePairProvedExit,
		weavePairHarnessError: weavePairHarnessErrorExit,
	}[verdict]
	return code, ok
}

// weaveNormalizePairReview is the absence-of-evidence backstop around both the
// real pair process and test/in-process runners. A zero value is not a pass: a
// runner must return a named verdict and reason, or it is a harness failure.
func weaveNormalizePairReview(res weavePairReviewResult, runErr error) weavePairReviewResult {
	if runErr != nil {
		res.Verdict = weavePairHarnessError
		res.ExitCode = weavePairHarnessErrorExit
		res.Reason = strings.TrimSpace(runErr.Error())
	}
	if res.Verdict == "" {
		res.Verdict = weavePairHarnessError
		res.ExitCode = weavePairHarnessErrorExit
		res.Reason = "pair runner returned no verdict"
	}
	if strings.TrimSpace(res.Reason) == "" {
		res.Verdict = weavePairHarnessError
		res.ExitCode = weavePairHarnessErrorExit
		res.Reason = "pair runner returned a verdict with no reason"
	}
	if want, ok := weavePairExitForVerdict(res.Verdict); !ok || res.ExitCode != want {
		verdict, code := res.Verdict, res.ExitCode
		res.Verdict = weavePairHarnessError
		res.ExitCode = weavePairHarnessErrorExit
		res.Reason = fmt.Sprintf("pair runner returned inconsistent verdict %q with exit %d", verdict, code)
	}
	return res
}

// weavePairReviewRunner is replaceable in tests so pull's merge behavior can
// be exercised without spending an agent invocation.
var weavePairReviewRunner = runWeavePairReview

// weaveRecordPairThrottle records the REVIEWER's tool on cooldown when a pair
// invocation died on a provider quota/rate limit. Without this write, `weave
// fleet` — the check the orchestrator trusts before dispatching — kept saying
// "available" while every `weave pull --review` burned a workspace pull and a
// PAIR HARNESS-ERROR (observed live with codex's exhausted quota, 2026-07-21).
// Best-effort: a cooldown write failure never fails the pull.
func weaveRecordPairThrottle(queueDir string, pr weavePairReviewResult, now time.Time) {
	if pr.Verdict != weavePairHarnessError || queueDir == "" {
		return
	}
	tool := pr.ReviewAgent // a binding ("codex:gpt-5.5") or bare tool name
	if i := strings.IndexByte(tool, ':'); i >= 0 {
		tool = tool[:i]
	}
	tool = normalizeToolName(tool)
	if tool == "" || tool == "." {
		return
	}
	evidence := pr.Reason + "\n" + pr.Output
	throttled, signal := weaveClassifyThrottle(tool, pr.ExitCode, evidence)
	if !throttled {
		return
	}
	reset, ok := parseThrottleReset(evidence, now)
	if !ok {
		// No parseable reset time: a short conservative cooldown still stops
		// the dispatch loop; the next real invocation refreshes the record.
		reset = now.Add(30 * time.Minute)
	}
	_ = recordToolCooldownCause(queueDir, tool, reset, weaveThrottleCause(signal))
}

func weaveCodingIdentity(it *weaveItem) (identity, tool, model string) {
	if it == nil {
		return "", "", ""
	}
	tool = it.Tool
	if it.LaunchSpec != nil {
		model = it.LaunchSpec.Model
		if it.LaunchSpec.Tool != "" && tool == "" {
			tool = it.LaunchSpec.Tool
		}
		if it.LaunchSpec.Agent != "" {
			identity = it.LaunchSpec.Agent
			// LaunchSpec.Model is the provider-side target, while separation
			// compares registry bindings. Resolve the recorded agent so aliases
			// and provider target names cannot disguise self-review.
			if l, err := weaveResolveAgent(it.LaunchSpec.Agent); err == nil && l != nil {
				identity, tool, model = l.Binding(), l.ToolName, l.ModelName
			}
		}
	}
	if tool != "" && model != "" {
		identity = tool + ":" + model
	}
	if identity == "" {
		identity = tool
	}
	return identity, tool, model
}

// weaveSelectReviewAgent resolves the requested reviewer and enforces the
// load-bearing separation: the acting pair cannot be the coder. "auto" is an
// explicit opt-in that asks weave to choose from the fleet registry.
func weaveSelectReviewAgent(requested string, it *weaveItem) (reviewer, coder string, err error) {
	coder, coderTool, coderModel := weaveCodingIdentity(it)
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", coder, nil
	}

	resolve := func(name string) (*weaveAgentLaunch, error) {
		l, err := weaveResolveAgent(name)
		if err != nil {
			return nil, err
		}
		if l == nil {
			return nil, fmt.Errorf("reviewer %q is not a fleet agent; use an agent nickname or tool:model binding", name)
		}
		return l, nil
	}

	if requested != "auto" {
		l, rerr := resolve(requested)
		if rerr != nil {
			return "", coder, rerr
		}
		sameBinding := coderTool != "" && l.ToolName == coderTool && (coderModel == "" || l.ModelName == coderModel)
		sameName := it != nil && it.LaunchSpec != nil && it.LaunchSpec.Agent != "" &&
			(requested == it.LaunchSpec.Agent || l.Nick == it.LaunchSpec.Agent)
		if !sameBinding && !sameName {
			return l.Binding(), coder, nil
		}
	}

	agents, _ := fleetCatalog().Agents()
	// Prefer diversity on both axes. If the local registry cannot supply that,
	// accept a different binding on either axis; it is still not self-review.
	for pass := 0; pass < 2; pass++ {
		for _, a := range agents {
			if a.Tool == coderTool && a.Model == coderModel {
				continue
			}
			if pass == 0 && (a.Tool == coderTool || (coderModel != "" && a.Model == coderModel)) {
				continue
			}
			l, rerr := resolve(a.Name)
			if rerr == nil {
				return l.Binding(), coder, nil
			}
		}
	}
	return "", coder, fmt.Errorf("review agent %q resolves to the coder %q and no different fleet agent is available", requested, coder)
}

func runWeavePairReview(workspace, diffRef, gateCommand, requested string, it *weaveItem) (weavePairReviewResult, error) {
	reviewer, coder, err := weaveSelectReviewAgent(requested, it)
	res := weavePairReviewResult{CodingAgent: coder, ReviewAgent: reviewer}
	if err != nil || reviewer == "" {
		if err == nil {
			err = errors.New("no review agent resolved")
		}
		return weaveNormalizePairReview(res, err), nil
	}
	if strings.TrimSpace(gateCommand) == "" {
		return weaveNormalizePairReview(res, fmt.Errorf("run #%d has no verify or suite gate for adversarial review; a pair cannot become the arbiter", it.ID)), nil
	}

	before, _ := gitOut(workspace, "rev-parse", "HEAD")
	before = strings.TrimSpace(before)
	task := fmt.Sprintf("Adversarially test run #%d (%s). Write and leave a failing test for every real defect; do not fix or approve the code.", it.ID, it.Title)
	args := []string{"pair", task, "--diff", diffRef, "--agent", reviewer, "--verify", gateCommand, "--json"}
	cmd := exec.Command("bashy", args...)
	cmd.Dir = workspace
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()
	res.Output = strings.TrimSpace(string(out))
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			return weaveNormalizePairReview(res, fmt.Errorf("launch bashy pair: %w", runErr)), nil
		}
	}
	if res.Output == "" {
		reason := fmt.Sprintf("bashy pair exited %d with no verdict", res.ExitCode)
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			reason += ": " + msg
		}
		return weaveNormalizePairReview(res, errors.New(reason)), nil
	}
	var pairResult struct {
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal(out, &pairResult); err != nil || pairResult.Outcome == "" {
		reason := "bashy pair returned malformed verdict"
		if err != nil {
			reason += ": " + err.Error()
		}
		return weaveNormalizePairReview(res, errors.New(reason)), nil
	}
	switch pairResult.Outcome {
	case "held":
		res.Verdict, res.ExitCode, res.Reason = weavePairPass, weavePairPassExit, "pair attacked the change and the gate stayed green"
	case "proved":
		res.Verdict, res.ExitCode, res.Reason = weavePairRefuted, weavePairProvedExit, "pair proved a defect against a green baseline"
	case "broken-before", "repaired":
		res.Verdict, res.ExitCode, res.Reason = weavePairBrokenBefore, weavePairBrokenBeforeExit, "verification was already broken before the pair could produce attributable evidence"
	case "unmeasured":
		res.Verdict, res.ExitCode, res.Reason = weavePairUnmeasured, weavePairUnmeasuredExit, "the baseline was never measured; the after gate found the work red"
	default:
		return weaveNormalizePairReview(res, fmt.Errorf("bashy pair returned unsupported outcome %q", pairResult.Outcome)), nil
	}

	// Pair evidence must survive the workspace. Commit whatever the acting pair
	// left; this does not approve it—the next verify/suite gate still decides.
	if committed, cerr := weaveCommitPairEvidence(workspace, fmt.Sprintf("weave: adversarial review evidence for run #%d by %s", it.ID, reviewer)); cerr != nil {
		return res, fmt.Errorf("commit pair evidence: %w", cerr)
	} else if committed {
		// The changed-path check below observes the new commit.
	}
	res.AddedTest = weaveReviewChangedTest(workspace, before)

	if want := map[weavePairVerdict]int{
		weavePairPass: weavePairPassExit, weavePairRefuted: weavePairProvedExit, weavePairBrokenBefore: weavePairBrokenBeforeExit,
		weavePairUnmeasured: weavePairUnmeasuredExit,
	}[res.Verdict]; res.ExitCode != want {
		return weaveNormalizePairReview(res, fmt.Errorf("bashy pair outcome %q disagrees with exit %d (want %d): %s", pairResult.Outcome, res.ExitCode, want, strings.TrimSpace(stderr.String()))), nil
	}
	return weaveNormalizePairReview(res, nil), nil
}

func weaveCommitPairEvidence(workspace, message string) (bool, error) {
	weaveEnsureBashyExclude(workspace)
	dirty, _, untracked := weaveMeasureDirtiness(workspace)
	if !dirty && untracked == 0 {
		return false, nil
	}
	if out, err := exec.Command("git", "-C", workspace, "add", "-A").CombinedOutput(); err != nil {
		return false, fmt.Errorf("git add pair evidence: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// A proof is intentionally red. A pre-commit hook that runs the suite must
	// not erase that durable evidence by refusing its commit.
	if out, err := exec.Command("git", "-C", workspace, "commit", "--no-verify", "-m", message).CombinedOutput(); err != nil {
		return false, fmt.Errorf("git commit pair evidence: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

func weaveReviewChangedTest(workspace, before string) bool {
	if before == "" {
		return false
	}
	out, err := exec.Command("git", "-C", workspace, "diff", "--name-only", before+"...HEAD").Output()
	if err != nil {
		return false
	}
	for _, name := range strings.Fields(string(out)) {
		base := strings.ToLower(filepath.Base(name))
		if strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, "test_") ||
			strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") || strings.Contains(base, "_test.") ||
			strings.Contains(strings.ToLower(filepath.ToSlash(name)), "/test/") ||
			strings.Contains(strings.ToLower(filepath.ToSlash(name)), "/tests/") {
			return true
		}
	}
	return false
}
