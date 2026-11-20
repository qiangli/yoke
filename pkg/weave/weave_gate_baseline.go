package weave

// THE SUITE GATE MUST BLAME ONLY WHAT THE CANDIDATE BROKE.
//
// weaveRunCandidateSuiteGate runs the project gate on base+candidate and lets
// the exit status be the verdict. That is right only when the base is green.
// This repo's base is deliberately NOT green — tests for open stories are
// committed red on purpose (test/known-failures.txt exists for exactly that
// reason), so every candidate inherits the base's failures and the gate reports
// `suite-gate-failed` for work that broke nothing. The agent then goes looking
// for a break it did not cause, and the next person routes around the gate.
//
// So the verdict is a DIFF, not an exit status — the same ratchet
// scripts/ci-test-gate.sh applies to CI, moved to where weave decides a merge:
//
//   - a failure the base also has        -> PRE-EXISTING, reported, not fatal
//   - a failure the base does not have   -> NEW, blocks the merge
//   - a package that failed WITHOUT a failing test (a build or setup error)
//     -> never excused, because nothing was measured
//   - a run in which no test reported at all, or one the verify timeout cut
//     short -> INCONCLUSIVE, never scored and never cached
//
// The last two are the ratchet's non-negotiable rules, and they are not
// theoretical: a gate that let a build error look like a known failure is how
// coreutils CI once spent weeks reporting a verdict on a suite that never ran.
//
// The base measurement is cached by (base SHA, gate command) under the weave
// state dir, so a queue of candidates against one base pays for it once. It is
// only ever taken when the candidate gate FAILED — a green candidate needs no
// attribution, and that is what keeps this free in the common case.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// weaveGateBaselineTTL is disk hygiene, not correctness: a record is pinned to
// an exact (base SHA, command) pair, so it never goes stale — but bases move on
// and nobody re-gates a three-week-old one.
const weaveGateBaselineTTL = 14 * 24 * time.Hour

// weaveGateMaxReported bounds the report. The full output follows it; this is
// the summary a human reads first.
const weaveGateMaxReported = 10

// weaveGateOutcome is what one gate run SAID, reduced to the identities a
// verdict can be computed from.
type weaveGateOutcome struct {
	// Tests are failing TOP-LEVEL tests as "<package>\t<test>". Subtests are
	// folded into their parent: the baseline names top-level tests, and a
	// renamed subtest is not a new failure.
	Tests []string
	// Broken are packages that failed WITHOUT naming a failing test — a build
	// error, a TestMain failure, a panic that took the binary down. Never
	// excusable: the suite did not run, so there is nothing to compare.
	Broken []string
	// Ran is POSITIVE evidence that tests executed: a package that reported ok,
	// or any test that reported a result. Zero evidence means the run did not
	// happen (bad flags, no toolchain, dead checkout, a `go vet` ahead of it
	// that failed first) and scoring that base would blame the candidate for
	// every failure the base also has.
	//
	// It is deliberately NOT "at least one test passed". A single-package gate
	// (`go test ./pkg/weave/`) whose package has one failing test prints no `ok`
	// line at all — nothing passed by that measure, yet the suite ran in full,
	// and refusing to score it is the exact wrong answer for the commonest gate
	// shape there is.
	Ran bool
	// Incomplete records that the run was CUT SHORT — the 10m verify deadline
	// killed it mid-suite. The failures it named are real, but the ones it never
	// reached are unknown, so neither side of the comparison may use it: an
	// incomplete candidate excused by the base would merge an unmeasured suite,
	// and an incomplete base cached as the baseline would excuse real
	// regressions in every package the base never reached.
	Incomplete bool
}

// weaveGateBaselineRecord is the cached measurement of the gate on one base
// commit. BaseSHA and Command are stored, not just hashed, so a key collision
// or a layout change is caught on read instead of silently excusing failures.
type weaveGateBaselineRecord struct {
	BaseSHA    string    `json:"base_sha"`
	Command    string    `json:"command"`
	Exit       int       `json:"exit"`
	Tests      []string  `json:"failing_tests,omitempty"`
	Broken     []string  `json:"broken_packages,omitempty"`
	Ran        bool      `json:"suite_ran"`
	Incomplete bool      `json:"incomplete,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

// weaveGateVerdict is the re-scored meaning of a failing candidate gate.
type weaveGateVerdict struct {
	// Excused: every failure the candidate shows is already on the base.
	Excused bool
	// New failures, absent from the base. Non-empty means block.
	New []string
	// PreExisting are the candidate's failures the base also has.
	PreExisting []string
	// Reason is set when no comparison was possible at all. The candidate's
	// failure then stands on its own, which is the pre-existing behavior.
	Reason string
}

// weaveParseGateOutput reads the plain-text output of a `go test` gate.
//
// It parses the UNTRIMMED output on purpose: weaveTrimVerifyOutput hoists the
// salient lines to the front, which preserves every failure for a human but
// destroys the per-package grouping this attribution depends on (and can
// duplicate a `FAIL <pkg>` line away from its tests, turning a plain failure
// into a phantom build error).
func weaveParseGateOutput(s string) weaveGateOutcome {
	var (
		out     weaveGateOutcome
		pending []string
		tests   = map[string]bool{}
		broken  = map[string]bool{}
	)
	for _, ln := range strings.Split(s, "\n") {
		// Evidence the suite actually ran, gathered BEFORE the top-level filter
		// below: an indented subtest result and a `--- FAIL:` that is nobody's
		// baseline entry still prove that tests executed.
		if weaveGateLineProvesRun(ln) {
			out.Ran = true
		}
		if strings.Contains(ln, weaveVerifyTimeoutMarker) {
			out.Incomplete = true
		}
		switch {
		// Top-level failures only. go test indents subtests
		// ("    --- FAIL: TestX/sub"), so column 0 is the filter.
		case strings.HasPrefix(ln, "--- FAIL: "):
			name := strings.TrimPrefix(ln, "--- FAIL: ")
			if i := strings.IndexAny(name, " \t("); i >= 0 {
				name = name[:i]
			}
			if name == "" || strings.Contains(name, "/") {
				continue
			}
			pending = append(pending, name)
		case ln == "FAIL", ln == "FAIL\t":
			// go test's bare summary line. Carries no package.
		case strings.HasPrefix(ln, "FAIL\t"), strings.HasPrefix(ln, "FAIL "):
			fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(ln, "FAIL")))
			if len(fields) == 0 {
				continue
			}
			pkg := fields[0]
			switch {
			case strings.Contains(ln, "[build failed]"), strings.Contains(ln, "[setup failed]"):
				broken[pkg] = true
			case len(pending) == 0:
				// A package failed and named no test of its own: a panic that
				// killed the binary, a TestMain failure, a timeout. Nothing was
				// measured, so nothing can be attributed.
				broken[pkg] = true
			default:
				for _, t := range pending {
					tests[pkg+"\t"+t] = true
				}
			}
			pending = nil
		}
	}
	// Failures whose package line never arrived (truncated run, killed gate).
	// Keep them with an empty package rather than dropping evidence.
	for _, t := range pending {
		tests["\t"+t] = true
	}
	out.Tests = weaveGateSortedKeys(tests)
	out.Broken = weaveGateSortedKeys(broken)
	return out
}

// weaveGateLineProvesRun reports whether a line is evidence that tests
// executed: a package that reported ok, or any test (top-level or subtest)
// that reported a result.
//
// `?   pkg [no test files]` deliberately does NOT count — a package with no
// tests proves nothing about whether the suite ran.
func weaveGateLineProvesRun(ln string) bool {
	if strings.HasPrefix(ln, "ok ") || strings.HasPrefix(ln, "ok\t") {
		return true
	}
	ln = strings.TrimLeft(ln, " ")
	return strings.HasPrefix(ln, "--- PASS: ") ||
		strings.HasPrefix(ln, "--- FAIL: ") ||
		strings.HasPrefix(ln, "--- SKIP: ")
}

func weaveGateSortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// weaveGateFailureLabel renders a "<pkg>\t<test>" identity for a human.
func weaveGateFailureLabel(id string) string {
	pkg, test, ok := strings.Cut(id, "\t")
	if !ok {
		return id
	}
	if pkg == "" {
		return test
	}
	return pkg + " " + test
}

// weaveGateUnattributable reports why a failing candidate gate can never be
// excused by the base, or "" when the comparison is worth making.
//
// Checked BEFORE the base is measured: these answers do not depend on it, and
// the base run is the expensive part.
func weaveGateUnattributable(cand weaveGateOutcome) string {
	if len(cand.Broken) > 0 {
		return fmt.Sprintf("package(s) failed without a failing test — a build or setup error is never excused by the base: %s",
			strings.Join(cand.Broken, " "))
	}
	if cand.Incomplete {
		return "candidate gate run was CUT SHORT by the verify timeout — the packages it never reached are unmeasured, so the base cannot excuse it"
	}
	if len(cand.Tests) == 0 {
		return "gate failed without naming a failing test; nothing to attribute"
	}
	return ""
}

// weaveGateDecide re-scores a failing candidate gate against the base.
func weaveGateDecide(cand weaveGateOutcome, base *weaveGateBaselineRecord) weaveGateVerdict {
	if reason := weaveGateUnattributable(cand); reason != "" {
		return weaveGateVerdict{Reason: reason}
	}
	if base == nil {
		return weaveGateVerdict{Reason: "no base gate result available"}
	}
	if !base.Ran {
		return weaveGateVerdict{Reason: "base gate run was INCONCLUSIVE — no test reported a result, so the suite did not run there; refusing to score"}
	}
	if base.Incomplete {
		return weaveGateVerdict{Reason: "base gate run was CUT SHORT by the verify timeout — an incomplete baseline would excuse failures in every package it never reached"}
	}
	known := make(map[string]bool, len(base.Tests))
	for _, t := range base.Tests {
		known[t] = true
	}
	v := weaveGateVerdict{}
	for _, t := range cand.Tests {
		if known[t] {
			v.PreExisting = append(v.PreExisting, t)
		} else {
			v.New = append(v.New, t)
		}
	}
	v.Excused = len(v.New) == 0
	return v
}

// weaveGateBaselineReport is the summary that goes in front of the gate output.
// It is written for the agent that has to act on it: what is new (its problem),
// what is pre-existing (not its problem), and which base it was measured on.
func weaveGateBaselineReport(baseSHA string, v weaveGateVerdict, base *weaveGateBaselineRecord) string {
	short := baseSHA
	if len(short) > 12 {
		short = short[:12]
	}
	if v.Reason != "" {
		return fmt.Sprintf("[weave: gate baseline NOT applied — %s]", v.Reason)
	}
	var b strings.Builder
	if v.Excused {
		fmt.Fprintf(&b, "[weave: gate baseline — all %d failure(s) are PRE-EXISTING on base %s; this candidate broke nothing, merge allowed]",
			len(v.PreExisting), short)
	} else {
		fmt.Fprintf(&b, "[weave: gate baseline — %d NEW failure(s) vs base %s (%d pre-existing); the new ones block this merge]",
			len(v.New), short, len(v.PreExisting))
	}
	weaveGateAppendList(&b, "NEW", v.New)
	weaveGateAppendList(&b, "pre-existing", v.PreExisting)
	if base != nil {
		if missing := len(base.Tests) - len(v.PreExisting); missing > 0 {
			fmt.Fprintf(&b, "\n  (%d base failure(s) did not reproduce here)", missing)
		}
	}
	return b.String()
}

func weaveGateAppendList(b *strings.Builder, label string, ids []string) {
	for i, id := range ids {
		if i == weaveGateMaxReported {
			fmt.Fprintf(b, "\n  %s: +%d more", label, len(ids)-i)
			return
		}
		fmt.Fprintf(b, "\n  %s: %s", label, weaveGateFailureLabel(id))
	}
}

// weaveGateOutputWithReport puts the report where it survives the reducer.
//
// weaveTrimVerifyOutput keeps only lines that look like failures, so a report
// fed through it would be the first thing dropped. Trim the gate output first,
// within a budget that leaves room, then prepend.
func weaveGateOutputWithReport(report, raw string) string {
	if report == "" {
		return weaveTrimVerifyOutput(raw, 2000)
	}
	budget := 2000 - len(report) - 1
	if budget < 400 {
		budget = 400
	}
	return report + "\n" + weaveTrimVerifyOutput(raw, budget)
}

func weaveGateBaselineKey(baseSHA, command string) string {
	sum := sha256.Sum256([]byte(baseSHA + "\x00" + command))
	return hex.EncodeToString(sum[:8])
}

func weaveGateBaselineDir(dir string) string {
	return filepath.Join(dir, "gate-baseline")
}

func weaveGateBaselinePath(dir, baseSHA, command string) string {
	return filepath.Join(weaveGateBaselineDir(dir), weaveGateBaselineKey(baseSHA, command)+".json")
}

// weaveGateBaselineLoad returns the cached base measurement, or nil. Every
// failure to read one is a cache miss, never an error: the worst case is
// re-measuring, and a gate that refuses to run is worse than a slow one.
func weaveGateBaselineLoad(dir, baseSHA, command string) *weaveGateBaselineRecord {
	if dir == "" {
		return nil
	}
	b, err := os.ReadFile(weaveGateBaselinePath(dir, baseSHA, command))
	if err != nil {
		return nil
	}
	var rec weaveGateBaselineRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil
	}
	// The filename is a truncated hash. Verify the identity it claims.
	if rec.BaseSHA != baseSHA || rec.Command != command {
		return nil
	}
	return &rec
}

func weaveGateBaselineSave(dir string, rec *weaveGateBaselineRecord) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(weaveGateBaselineDir(dir), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	path := weaveGateBaselinePath(dir, rec.BaseSHA, rec.Command)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	weaveGateBaselinePrune(dir, time.Now())
	return nil
}

// weaveGateBaselinePrune drops records for bases nobody gates anymore.
func weaveGateBaselinePrune(dir string, now time.Time) {
	entries, err := os.ReadDir(weaveGateBaselineDir(dir))
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.IsDir() {
			continue
		}
		if now.Sub(info.ModTime()) > weaveGateBaselineTTL {
			os.Remove(filepath.Join(weaveGateBaselineDir(dir), e.Name()))
		}
	}
}

// weaveGateRunBaseline measures the gate on the base commit ALONE, reusing the
// isolated checkout the candidate was just gated in: the sibling replaces are
// already linked there and the build cache is warm, so this is the cheapest
// honest measurement available.
func weaveGateRunBaseline(root, checkout, baseSHA, command string) (*weaveGateBaselineRecord, error) {
	// --force: the candidate's merge is being discarded on purpose.
	if out, err := exec.Command(gitBin(), "-C", checkout,
		"checkout", "--quiet", "--force", "--detach", baseSHA).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("checkout gate baseline %s: %v: %s", baseSHA, err, strings.TrimSpace(string(out)))
	}
	// The candidate may have moved a submodule pin; put the base's back.
	// Best-effort, exactly as the candidate path treats it.
	_ = weaveHydrateSubmodules(root, checkout, io.Discard, io.Discard)
	exit, raw := weaveRunSuiteGateRaw(checkout, command)
	oc := weaveParseGateOutput(raw)
	return &weaveGateBaselineRecord{
		BaseSHA:    baseSHA,
		Command:    command,
		Exit:       exit,
		Tests:      oc.Tests,
		Broken:     oc.Broken,
		Ran:        oc.Ran,
		Incomplete: oc.Incomplete,
		RecordedAt: time.Now(),
	}, nil
}

// weaveGateRescore turns a failing candidate gate into a verdict about the
// CANDIDATE. It returns the exit status the merge should act on and the output
// to record, report first.
//
// Note on flakes: a base failure that was a flake would excuse a genuinely new
// failure of the same test. ci-test-gate.sh answers that with a confirming
// re-run; here the cost is a second full suite, so it is deliberately not done.
// A flaky test that hides a regression is a story about the flaky test.
func weaveGateRescore(dir, root, checkout, baseSHA, command string, exit int, raw string) (int, string) {
	cand := weaveParseGateOutput(raw)
	if reason := weaveGateUnattributable(cand); reason != "" {
		return exit, weaveGateOutputWithReport(
			weaveGateBaselineReport(baseSHA, weaveGateVerdict{Reason: reason}, nil), raw)
	}
	rec := weaveGateBaselineLoad(dir, baseSHA, command)
	if rec == nil {
		measured, err := weaveGateRunBaseline(root, checkout, baseSHA, command)
		if err != nil {
			return exit, weaveGateOutputWithReport(
				weaveGateBaselineReport(baseSHA, weaveGateVerdict{Reason: err.Error()}, nil), raw)
		}
		rec = measured
		// An inconclusive or cut-short measurement is not cached. It cannot
		// score this candidate, and caching it would either make every later
		// candidate against this base unscoreable too, or — worse, for the
		// cut-short case — pin a baseline that is missing the failures the run
		// never reached.
		if rec.Ran && !rec.Incomplete {
			_ = weaveGateBaselineSave(dir, rec)
		}
	}
	v := weaveGateDecide(cand, rec)
	report := weaveGateBaselineReport(baseSHA, v, rec)
	if v.Excused {
		return 0, weaveGateOutputWithReport(report, raw)
	}
	return exit, weaveGateOutputWithReport(report, raw)
}
