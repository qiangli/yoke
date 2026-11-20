package weave

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Gate output shaped exactly like `go test ./...`: one passing package, one
// failing test in another, the bare FAIL summary line and the per-package FAIL.
const (
	gateBaseOutput = "ok  \texample.com/p/a\t0.01s\n" +
		"--- FAIL: TestOld (0.00s)\n" +
		"    old_test.go:4: pre-existing, committed red for an open story\n" +
		"FAIL\n" +
		"FAIL\texample.com/p/b\t0.02s\n" +
		"FAIL\n"

	gateCandidateOutput = "ok  \texample.com/p/a\t0.01s\n" +
		"--- FAIL: TestOld (0.00s)\n" +
		"    old_test.go:4: pre-existing, committed red for an open story\n" +
		"--- FAIL: TestNew (0.00s)\n" +
		"    new_test.go:9: THIS one is the candidate's\n" +
		"FAIL\n" +
		"FAIL\texample.com/p/b\t0.02s\n" +
		"FAIL\n"
)

func gateBaselineRecord(t *testing.T, output string, exit int) *weaveGateBaselineRecord {
	t.Helper()
	oc := weaveParseGateOutput(output)
	return &weaveGateBaselineRecord{
		BaseSHA: "0123456789abcdef0123", Command: "go test ./...",
		Exit: exit, Tests: oc.Tests, Broken: oc.Broken,
		Ran: oc.Ran, Incomplete: oc.Incomplete,
	}
}

// A FAILURE THE BASE ALREADY HAS IS NOT THIS CANDIDATE'S FAILURE.
//
// This repo commits tests red for open stories on purpose, so a gate that
// reads its own exit status rejects work that broke nothing — and the agent
// burns a session hunting a break it did not cause.
func TestGateBaselineExcusesPreExistingFailure(t *testing.T) {
	cand := weaveParseGateOutput(gateBaseOutput)
	base := gateBaselineRecord(t, gateBaseOutput, 1)

	v := weaveGateDecide(cand, base)
	if !v.Excused {
		t.Fatalf("pre-existing failure not excused: new=%v", v.New)
	}
	if len(v.New) != 0 {
		t.Fatalf("new = %v, want none", v.New)
	}
	if len(v.PreExisting) != 1 || v.PreExisting[0] != "example.com/p/b\tTestOld" {
		t.Fatalf("pre-existing = %v", v.PreExisting)
	}

	// Excused is not silent: the report has to say what was forgiven and
	// against which base, or the next reader cannot tell a measured pass from
	// a gate that was never run.
	report := weaveGateBaselineReport(base.BaseSHA, v, base)
	for _, want := range []string{"PRE-EXISTING", "012345678", "example.com/p/b TestOld"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
}

// A FAILURE THE BASE DOES NOT HAVE BLOCKS, and the pre-existing ones are still
// reported so the reader is not left guessing which of the two is theirs.
func TestGateBaselineBlocksNewFailure(t *testing.T) {
	cand := weaveParseGateOutput(gateCandidateOutput)
	base := gateBaselineRecord(t, gateBaseOutput, 1)

	v := weaveGateDecide(cand, base)
	if v.Excused {
		t.Fatal("new failure was excused; the gate would merge a regression")
	}
	if len(v.New) != 1 || v.New[0] != "example.com/p/b\tTestNew" {
		t.Fatalf("new = %v, want [example.com/p/b TestNew]", v.New)
	}
	if len(v.PreExisting) != 1 || v.PreExisting[0] != "example.com/p/b\tTestOld" {
		t.Fatalf("pre-existing = %v", v.PreExisting)
	}

	report := weaveGateBaselineReport(base.BaseSHA, v, base)
	for _, want := range []string{"NEW", "example.com/p/b TestNew", "pre-existing: example.com/p/b TestOld"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
}

// The same test name failing in a DIFFERENT package is a different failure.
// Identity is package+test, which is why the output is parsed per package block.
func TestGateBaselineIdentityIsPackageQualified(t *testing.T) {
	cand := weaveParseGateOutput("ok  \texample.com/p/a\t0.01s\n--- FAIL: TestOld (0.00s)\nFAIL\texample.com/p/c\t0.02s\n")
	base := gateBaselineRecord(t, gateBaseOutput, 1)
	v := weaveGateDecide(cand, base)
	if v.Excused {
		t.Fatalf("TestOld in another package was excused: %+v", v)
	}
	if len(v.New) != 1 || v.New[0] != "example.com/p/c\tTestOld" {
		t.Fatalf("new = %v", v.New)
	}
}

// RULE 1 OF THE RATCHET: a package that fails WITHOUT a failing test — a build
// error, a TestMain failure, a panic — is never baselineable. Nothing ran, so
// nothing was measured, and "the base has it too" says nothing. coreutils CI
// spent weeks reporting a verdict on a suite that died before its first test.
func TestGateBaselineNeverExcusesBuildError(t *testing.T) {
	broken := "ok  \texample.com/p/a\t0.01s\n# example.com/p/b\nb.go:7:2: undefined: nope\nFAIL\texample.com/p/b [build failed]\n"
	cand := weaveParseGateOutput(broken)
	if len(cand.Broken) != 1 || cand.Broken[0] != "example.com/p/b" {
		t.Fatalf("broken = %v", cand.Broken)
	}
	// Even with the IDENTICAL break recorded on the base.
	v := weaveGateDecide(cand, gateBaselineRecord(t, broken, 1))
	if v.Excused {
		t.Fatal("build error excused by the base")
	}
	if !strings.Contains(v.Reason, "build or setup error") {
		t.Fatalf("reason = %q", v.Reason)
	}
	if !strings.Contains(weaveGateBaselineReport("abc", v, nil), "NOT applied") {
		t.Fatalf("report does not say the baseline was skipped: %s", weaveGateBaselineReport("abc", v, nil))
	}
}

// A package that failed and named no test of its own (panic, timeout, TestMain)
// is the same thing without a [build failed] marker.
func TestGateBaselinePackageFailureWithoutTestIsBroken(t *testing.T) {
	oc := weaveParseGateOutput("ok  \texample.com/p/a\t0.01s\npanic: boom\nFAIL\texample.com/p/b\t0.30s\n")
	if len(oc.Broken) != 1 || oc.Broken[0] != "example.com/p/b" {
		t.Fatalf("broken = %v, tests = %v", oc.Broken, oc.Tests)
	}
}

// RULE 2 OF THE RATCHET: an INCONCLUSIVE run is never scored. A base run where
// no test reported at all did not execute the suite, and treating that as "the
// base has no failures" would blame the candidate for every one of them.
func TestGateBaselineRefusesInconclusiveBase(t *testing.T) {
	cand := weaveParseGateOutput(gateCandidateOutput)
	// A gate that died before its first test: a `go vet` ahead of `go test`,
	// a missing toolchain, a dead checkout. No test line of any kind.
	base := gateBaselineRecord(t, "vet: ./a.go:7:2: undefined: nope\nFAIL\texample.com/p/b\t0.02s\n", 1)
	if base.Ran {
		t.Fatal("a run with no test result was scored as having run")
	}
	v := weaveGateDecide(cand, base)
	if v.Excused {
		t.Fatal("excused against a base run that never executed")
	}
	if !strings.Contains(v.Reason, "INCONCLUSIVE") {
		t.Fatalf("reason = %q", v.Reason)
	}
}

// A green base excuses nothing: every failure is the candidate's.
func TestGateBaselineGreenBaseBlocksEverything(t *testing.T) {
	cand := weaveParseGateOutput(gateCandidateOutput)
	base := gateBaselineRecord(t, "ok  \texample.com/p/a\t0.01s\nok  \texample.com/p/b\t0.02s\n", 0)
	v := weaveGateDecide(cand, base)
	if v.Excused || len(v.New) != 2 {
		t.Fatalf("green base: %+v", v)
	}
}

// No base result at all (checkout failed, gate could not run there) leaves the
// candidate's failure standing — the pre-existing behavior. Fail closed.
func TestGateBaselineMissingBaseFailsClosed(t *testing.T) {
	v := weaveGateDecide(weaveParseGateOutput(gateCandidateOutput), nil)
	if v.Excused {
		t.Fatal("excused with no base measurement")
	}
}

// Subtests fold into their parent: the baseline names top-level tests, and a
// renamed or added subtest under an already-failing test is not a new failure.
func TestGateParseFoldsSubtests(t *testing.T) {
	oc := weaveParseGateOutput("ok  \texample.com/p/a\t0.01s\n" +
		"--- FAIL: TestOld (0.00s)\n    --- FAIL: TestOld/case-7 (0.00s)\nFAIL\texample.com/p/b\t0.02s\n")
	if len(oc.Tests) != 1 || oc.Tests[0] != "example.com/p/b\tTestOld" {
		t.Fatalf("tests = %v", oc.Tests)
	}
	if !oc.Ran {
		t.Fatal("Ran not set by an ok package line")
	}
}

// A SINGLE-PACKAGE GATE PRINTS NO `ok` LINE WHEN THAT PACKAGE FAILS.
//
// `go test ./pkg/weave/` with one failing test is the commonest gate shape
// there is, and its output contains not one passing line — the other 600 tests
// in the package passed silently. Reading "nothing passed" as "the suite did
// not run" would make the baseline refuse to score exactly the gate it was
// written for. Evidence of a RUN is a test reporting, not a test passing.
func TestGateBaselineScoresSinglePackageGateWithNoPassingLine(t *testing.T) {
	out := "--- FAIL: TestOld (0.00s)\n    old_test.go:4: pre-existing\nFAIL\nFAIL\texample.com/p/b\t4.28s\nFAIL\n"
	base := gateBaselineRecord(t, out, 1)
	if !base.Ran {
		t.Fatal("a full package run with no ok line was scored as never having run")
	}
	v := weaveGateDecide(weaveParseGateOutput(out), base)
	if !v.Excused {
		t.Fatalf("pre-existing failure not excused: %+v", v)
	}
}

// A RUN THE TIMEOUT CUT SHORT MEASURED NOTHING WHOLE.
//
// The failures it named are real; the packages it never reached are unknown. So
// a cut-short CANDIDATE can never be excused — all-pre-existing there would
// merge an unmeasured suite — and a cut-short BASE can neither score nor be
// cached, since it is missing the failures of every package it never reached.
func TestGateBaselineRefusesTimedOutRuns(t *testing.T) {
	cut := gateBaseOutput + "\n" + weaveVerifyTimeoutMarker
	oc := weaveParseGateOutput(cut)
	if !oc.Incomplete {
		t.Fatal("the timeout marker was not recognized")
	}

	// Candidate cut short, every named failure pre-existing: still not excused.
	v := weaveGateDecide(oc, gateBaselineRecord(t, gateBaseOutput, 1))
	if v.Excused {
		t.Fatal("a gate that timed out was excused; an unmeasured suite would merge")
	}
	if !strings.Contains(v.Reason, "CUT SHORT") {
		t.Fatalf("reason = %q", v.Reason)
	}

	// Base cut short: cannot be the baseline.
	v = weaveGateDecide(weaveParseGateOutput(gateCandidateOutput), gateBaselineRecord(t, cut, 1))
	if v.Excused || !strings.Contains(v.Reason, "CUT SHORT") {
		t.Fatalf("cut-short base was scored: %+v", v)
	}
}

// ... and such a base is not cached either, or one timed-out measurement would
// become the standing baseline for every later candidate on this base.
func TestGateRescoreDoesNotCacheTimedOutBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "proj")
	baseSHA := gitInitCommit(t, repo, "gate-output.txt", gateBaseOutput+"\n"+weaveVerifyTimeoutMarker)
	gitInitCommit(t, repo, "gate-output.txt", gateCandidateOutput)
	state := filepath.Join(tmp, "state")
	command := "cat gate-output.txt; exit 1"

	exit, out := weaveGateRescore(state, repo, repo, baseSHA, command, 1, gateCandidateOutput)
	if exit != 1 {
		t.Fatalf("exit = %d, want 1\n%s", exit, out)
	}
	if !strings.Contains(out, "CUT SHORT") {
		t.Fatalf("output does not report the cut-short base:\n%s", out)
	}
	if weaveGateBaselineLoad(state, baseSHA, command) != nil {
		t.Fatal("a cut-short base measurement was cached")
	}
}

// The report must survive the output reducer. weaveTrimVerifyOutput keeps only
// lines that look like failures, so a report fed through it would be the first
// thing dropped — which is the defect this repo keeps re-finding: a verdict
// that outlives its evidence.
func TestGateBaselineReportSurvivesTrimming(t *testing.T) {
	huge := strings.Repeat("ok  \texample.com/p/filler\t0.01s\n", 4000) + gateCandidateOutput
	out := weaveGateOutputWithReport("[weave: gate baseline — 1 NEW failure(s)]", huge)
	if !strings.HasPrefix(out, "[weave: gate baseline") {
		t.Fatalf("report lost:\n%s", out[:200])
	}
	if !strings.Contains(out, "--- FAIL: TestNew") {
		t.Fatalf("failure evidence lost:\n%s", out)
	}
}

// THE BASE GATE IS ACTUALLY RUN, ON THE BASE COMMIT, AND THE RESULT IS REUSED.
//
// The expensive half: measuring the base means a second suite run, so it is
// taken only when the candidate gate failed and then cached by (base SHA, gate
// command) — a queue of candidates against one base pays once.
func TestGateRescoreMeasuresBaseCommitAndCachesIt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "proj")
	baseSHA := gitInitCommit(t, repo, "gate-output.txt", gateBaseOutput)
	// The checkout is at the merged candidate; rescore resets it to baseSHA.
	gitInitCommit(t, repo, "gate-output.txt", gateCandidateOutput)
	state := filepath.Join(tmp, "state")
	command := "cat gate-output.txt; exit 1"

	exit, out := weaveGateRescore(state, repo, repo, baseSHA, command, 1, gateCandidateOutput)
	if exit != 1 {
		t.Fatalf("exit = %d, want 1 (TestNew is absent from the base)\n%s", exit, out)
	}
	for _, want := range []string{"NEW", "example.com/p/b TestNew", "pre-existing: example.com/p/b TestOld"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}

	rec := weaveGateBaselineLoad(state, baseSHA, command)
	if rec == nil {
		t.Fatal("base measurement was not cached")
	}
	if len(rec.Tests) != 1 || rec.Tests[0] != "example.com/p/b\tTestOld" {
		t.Fatalf("cached base tests = %v (the base commit's own failure)", rec.Tests)
	}
	if !rec.Ran || rec.Exit == 0 {
		t.Fatalf("cached record = %+v", rec)
	}

	// Destroying the repo makes a SECOND measurement impossible, so a verdict
	// now can only have come from the cache.
	if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}
	exit, out = weaveGateRescore(state, repo, repo, baseSHA, command, 1, gateBaseOutput)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0 — the only failure is pre-existing\n%s", exit, out)
	}
	if !strings.Contains(out, "PRE-EXISTING") {
		t.Fatalf("no report of the pre-existing failure:\n%s", out)
	}

	// A different gate command is a different measurement, never a cache hit.
	if weaveGateBaselineLoad(state, baseSHA, "go test ./pkg/weave/") != nil {
		t.Fatal("cache hit across gate commands")
	}
	if weaveGateBaselineLoad(state, strings.Repeat("f", 40), command) != nil {
		t.Fatal("cache hit across base SHAs")
	}
}

// An inconclusive base is not cached: it says nothing, and caching it would
// make every later candidate against that base unscoreable too.
func TestGateRescoreDoesNotCacheInconclusiveBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "proj")
	baseSHA := gitInitCommit(t, repo, "gate-output.txt", "FAIL\texample.com/p/b\t0.02s\n")
	gitInitCommit(t, repo, "gate-output.txt", gateCandidateOutput)
	state := filepath.Join(tmp, "state")
	command := "cat gate-output.txt; exit 1"

	exit, out := weaveGateRescore(state, repo, repo, baseSHA, command, 1, gateCandidateOutput)
	if exit != 1 {
		t.Fatalf("exit = %d, want 1\n%s", exit, out)
	}
	if !strings.Contains(out, "INCONCLUSIVE") {
		t.Fatalf("output does not report the inconclusive base:\n%s", out)
	}
	if weaveGateBaselineLoad(state, baseSHA, command) != nil {
		t.Fatal("an inconclusive base measurement was cached")
	}
}

// Disk hygiene: records for bases nobody gates anymore are dropped. Correctness
// does not depend on it — a record is pinned to an exact (SHA, command) pair.
func TestGateBaselinePruneDropsStaleRecords(t *testing.T) {
	state := t.TempDir()
	rec := gateBaselineRecord(t, gateBaseOutput, 1)
	if err := weaveGateBaselineSave(state, rec); err != nil {
		t.Fatal(err)
	}
	path := weaveGateBaselinePath(state, rec.BaseSHA, rec.Command)
	old := time.Now().Add(-weaveGateBaselineTTL - time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	weaveGateBaselinePrune(state, time.Now())
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale record survived the prune: %v", err)
	}
}
