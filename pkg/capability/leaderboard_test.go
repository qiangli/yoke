package capability

import (
	"math"
	"os"
	"testing"
	"time"
)

func b(v bool) *bool { return &v }

func gated(agent string, pass bool) RunRecord {
	return RunRecord{At: "2026-08-02T00:00:00Z", Agent: agent, Source: SourceWeave, GatePass: b(pass)}
}

func find(t *testing.T, board Board, agent string) Standing {
	t.Helper()
	for _, s := range board.Standings {
		if s.Agent == agent {
			return s
		}
	}
	t.Fatalf("no standing for %q", agent)
	return Standing{}
}

// THE N=1 GUARD. Ranking on raw pass rate makes one lucky run the fleet
// leader; Wilson is what stops it, and this pins the numbers the package
// comment quotes.
func TestWilsonPunishesSmallSamples(t *testing.T) {
	cases := []struct {
		passes, trials int
		want           float64
	}{
		{1, 1, 0.207},
		{9, 10, 0.596},
		{90, 100, 0.825},
		{0, 5, 0.0},
	}
	for _, c := range cases {
		got := wilsonLower(c.passes, c.trials)
		if math.Abs(got-c.want) > 0.01 {
			t.Errorf("wilsonLower(%d,%d) = %.3f, want ~%.3f", c.passes, c.trials, got, c.want)
		}
	}
	// The property that matters more than any single value: a perfect small
	// sample must NOT outrank a near-perfect large one.
	if wilsonLower(1, 1) >= wilsonLower(90, 100) {
		t.Fatal("1/1 outranked 90/100 — the guard is not working")
	}
}

// A run whose gate never ran is the ABSENCE of evidence. It may not enter the
// denominator, or a harness crash would rank an agent down for someone else's
// failure.
func TestNilGateIsNotAFailure(t *testing.T) {
	recs := []RunRecord{
		gated("claude:opus5", true),
		{At: "2026-08-02T00:00:01Z", Agent: "claude:opus5", Source: SourceWeave}, // no gate
		gated("claude:opus5", true),
	}
	s := find(t, Compute(recs, ComputeOptions{}), "claude:opus5")
	if s.GatedRuns != 2 {
		t.Fatalf("gated runs = %d, want 2 — a nil gate must not be counted", s.GatedRuns)
	}
	if s.Passes != 2 || s.PassRate != 1.0 {
		t.Fatalf("passes=%d rate=%.2f, want 2 and 1.00", s.Passes, s.PassRate)
	}
}

func TestMinSamplesSeparatesRankedFromObserved(t *testing.T) {
	var recs []RunRecord
	for range 5 {
		recs = append(recs, gated("claude:opus5", true))
	}
	for range 2 {
		recs = append(recs, gated("codex:gpt6", true))
	}
	board := Compute(recs, ComputeOptions{MinSamples: 5})

	if got := find(t, board, "claude:opus5").Tier; got != TierRanked {
		t.Errorf("5 gated runs should be ranked, got %q", got)
	}
	if got := find(t, board, "codex:gpt6").Tier; got != TierObserved {
		t.Errorf("2 gated runs must be observed-not-ranked, got %q", got)
	}
	// Ranked always sorts above observed, whatever the rates.
	if board.Standings[0].Agent != "claude:opus5" {
		t.Fatalf("ranked tier must sort first, got %q", board.Standings[0].Agent)
	}
}

// The tier that makes the artifact honest: an agent that never ran must appear
// and must be labelled, so a reader can tell it from one that ran and failed.
func TestFleetIsFullyEnumeratedAndPriorsAreLabelled(t *testing.T) {
	m := &Matrix{Agents: map[string]map[Capability]Cell{
		"claude:opus5":  {CapCoding: {Quality: 0.9, Source: SourceHost, Samples: 13}},
		"opencode:glm5": {CapCoding: {Quality: 0.7, Source: SourcePrior}},
	}}
	board := Compute([]RunRecord{gated("claude:opus5", true)}, ComputeOptions{Matrix: m})

	if len(board.Standings) != 2 {
		t.Fatalf("both agents must be enumerated, got %d", len(board.Standings))
	}
	prior := find(t, board, "opencode:glm5")
	if prior.Tier != TierPrior {
		t.Errorf("an agent with no host evidence is prior, got %q", prior.Tier)
	}
	if prior.GatedRuns != 0 || prior.Passes != 0 {
		t.Error("a prior row must carry no run counts")
	}
	if prior.PriorQuality == 0 {
		t.Error("the seeded estimate should be carried so the row is not blank")
	}
}

// Runs that predate the ledger survive only as an EMA, which cannot be
// un-averaged into a pass count. They are observed, never ranked, and the board
// must say how many agents are in that position — otherwise their absence from
// the ranked tier reads as poor performance rather than as a recording gap.
func TestPreLedgerEvidenceIsObservedNeverRanked(t *testing.T) {
	m := &Matrix{Agents: map[string]map[Capability]Cell{
		"claude:fable5": {CapCoding: {Quality: 0.82, Source: SourceHost, Samples: 19}},
	}}
	board := Compute(nil, ComputeOptions{Matrix: m, MinSamples: 5})

	s := find(t, board, "claude:fable5")
	if s.Tier != TierObserved {
		t.Fatalf("19 EMA samples with no ledger records is OBSERVED, got %q", s.Tier)
	}
	if s.GatedRuns != 0 {
		t.Fatal("an EMA yields no gated-run count; claiming one would invent evidence")
	}
	if s.MatrixSamples != 19 {
		t.Fatalf("matrix samples = %d, want 19", s.MatrixSamples)
	}
	if board.PreLedger != 1 {
		t.Fatalf("pre-ledger agent count = %d, want 1", board.PreLedger)
	}
}

// A pty scrape counts novel output, not tool calls. Pooling the two modes
// produces a number that means nothing and looks like it means something.
func TestLoopDisciplineUsesEventsModeOnly(t *testing.T) {
	recs := []RunRecord{
		{At: "2026-08-02T00:00:00Z", Agent: "a:m", Source: SourceWeave, GatePass: b(true), CoachMode: "events", RepeatRatio: 1.2},
		{At: "2026-08-02T00:00:01Z", Agent: "a:m", Source: SourceWeave, GatePass: b(true), CoachMode: "pty", RepeatRatio: 9.4},
	}
	s := find(t, Compute(recs, ComputeOptions{}), "a:m")
	if s.RepeatSamples != 1 {
		t.Fatalf("repeat samples = %d, want 1 — pty records must not pool", s.RepeatSamples)
	}
	if s.RepeatRatio != 1.2 {
		t.Fatalf("repeat ratio = %.1f, want 1.2 (the events-mode value)", s.RepeatRatio)
	}
}

// An unrecognised verdict has told us nothing. Reading it as a rejection would
// manufacture a failure out of a parser gap.
func TestUnknownReviewVerdictIsNotEvidence(t *testing.T) {
	recs := []RunRecord{
		{At: "2026-08-02T00:00:00Z", Agent: "a:m", Source: SourceWeave, Review: "approve"},
		{At: "2026-08-02T00:00:01Z", Agent: "a:m", Source: SourceWeave, Review: "harness-error"},
		{At: "2026-08-02T00:00:02Z", Agent: "a:m", Source: SourceWeave, Review: "reject"},
	}
	s := find(t, Compute(recs, ComputeOptions{}), "a:m")
	if s.Reviewed != 2 {
		t.Fatalf("reviewed = %d, want 2 — an unparsed verdict is not a review", s.Reviewed)
	}
	if s.Survived != 1 {
		t.Fatalf("survived = %d, want 1", s.Survived)
	}
}

// Published cost is a RELATIVE index; absolute spend stays host-local.
func TestCostIsRelativeToTheCheapest(t *testing.T) {
	recs := []RunRecord{
		{At: "2026-08-02T00:00:00Z", Agent: "cheap:m", Source: SourceWeave, GatePass: b(true), CostMicro: 1000},
		{At: "2026-08-02T00:00:01Z", Agent: "dear:m", Source: SourceWeave, GatePass: b(true), CostMicro: 4000},
	}
	board := Compute(recs, ComputeOptions{})
	if got := find(t, board, "cheap:m").CostIndex; math.Abs(got-1.0) > 0.001 {
		t.Errorf("cheapest index = %.2f, want 1.00", got)
	}
	if got := find(t, board, "dear:m").CostIndex; math.Abs(got-4.0) > 0.001 {
		t.Errorf("dearer index = %.2f, want 4.00", got)
	}
}

// --- ledger ---------------------------------------------------------------

func TestLedgerRefusesAnUnattributedRecord(t *testing.T) {
	t.Setenv("BASHY_CAPABILITY_DIR", t.TempDir())
	if err := Append(RunRecord{Source: SourceWeave, GatePass: b(true)}); err == nil {
		t.Fatal("a record with no tool:model agent must be refused — evidence pointing at the wrong actor is worse than none")
	}
}

func TestLedgerRoundTripsAndSkipsTornLines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_CAPABILITY_DIR", dir)

	for i := range 3 {
		if err := Append(RunRecord{At: "2026-08-02T00:00:0" + string(rune('0'+i)) + "Z",
			Agent: "claude:opus5", Source: SourceWeave, GatePass: b(true)}); err != nil {
			t.Fatal(err)
		}
	}
	// A torn line from a concurrent writer must not make the history unreadable.
	f, err := os.OpenFile(LedgerPath(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	recs, err := ReadLedger()
	if err != nil {
		t.Fatalf("a torn line must not be fatal: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("read %d records, want 3", len(recs))
	}
}

func TestLedgerAbsentIsEmptyNotAnError(t *testing.T) {
	t.Setenv("BASHY_CAPABILITY_DIR", t.TempDir())
	recs, err := ReadLedger()
	if err != nil {
		t.Fatalf("a host that has run nothing is a state, not a failure: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("want no records, got %d", len(recs))
	}
}

// An unparseable timestamp must not silently shrink an agent's sample count.
func TestSinceKeepsRecordsWithUnreadableClocks(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	recs := []RunRecord{
		{At: "2026-08-02T11:00:00Z", Agent: "a:m"}, // inside
		{At: "2026-07-01T00:00:00Z", Agent: "a:m"}, // outside
		{At: "not-a-time", Agent: "a:m"},           // unreadable — kept
	}
	got := Since(recs, 2*time.Hour, now)
	if len(got) != 2 {
		t.Fatalf("kept %d, want 2 (the recent one and the unreadable one)", len(got))
	}
}
