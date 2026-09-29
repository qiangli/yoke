package ladder

import (
	"math"
	"strings"
	"testing"
)

// scorecardNeutralInput returns an input whose non-delivery components are all
// at their best (review 1, breakdown 1, efficiency 1, hygiene 1) so tests can
// vary one part at a time.
func scorecardNeutralInput(assignments []Assignment) ScorecardInput {
	return ScorecardInput{
		Assignments:          assignments,
		CostPerPoint:         2,
		ExpectedCostPerPoint: 2,
		WallPerPoint:         10,
		ExpectedWallPerPoint: 10,
		HygieneChecksPassed:  5,
		HygieneChecksTotal:   5,
	}
}

func scorecardAssignment(points int, agentR, outcome float64) Assignment {
	agent := Rating{R: agentR, RD: 50, Vol: InitialVol}
	return Assignment{
		Story:         "s1",
		Points:        points,
		StoryRating:   1500,
		Agent:         "agent-a",
		AgentRating:   agent,
		BestAvailable: agent,
		Outcome:       outcome,
		Rated:         true,
	}
}

func TestComputeScorecardWeakTeamBeatsStrongTeamMiss(t *testing.T) {
	weights := DefaultScorecardWeights()

	// A weak team (1300 vs 1500 stories) delivering everything.
	weakBeat := scorecardNeutralInput([]Assignment{scorecardAssignment(3, 1300, 1)})
	// A strong team (1700 vs 1500 stories) delivering nothing.
	strongMiss := scorecardNeutralInput([]Assignment{scorecardAssignment(3, 1700, 0)})

	beat, err := ComputeScorecard(weakBeat, weights)
	if err != nil {
		t.Fatalf("ComputeScorecard(weakBeat): %v", err)
	}
	miss, err := ComputeScorecard(strongMiss, weights)
	if err != nil {
		t.Fatalf("ComputeScorecard(strongMiss): %v", err)
	}
	if beat.Score <= miss.Score {
		t.Fatalf("beating expectation with a weak team scored %v, missing with a strong team %v; want beat > miss",
			beat.Score, miss.Score)
	}
	if beat.Components["delivery"] <= 0.5 {
		t.Errorf("weak-team beat delivery = %v, want > 0.5", beat.Components["delivery"])
	}
	if miss.Components["delivery"] >= 0.5 {
		t.Errorf("strong-team miss delivery = %v, want < 0.5", miss.Components["delivery"])
	}
	if beat.Actual <= beat.Expected {
		t.Errorf("weak-team beat: actual %v <= expected %v", beat.Actual, beat.Expected)
	}
}

func TestComputeScorecardAutoFailForcesZero(t *testing.T) {
	kinds := []AutoFail{
		AutoFailFalseDone,
		AutoFailRedGateMerge,
		AutoFailDominanceBreach,
		AutoFailBrokenSharedState,
		AutoFailDetectedBypass,
	}
	for _, kind := range kinds {
		in := scorecardNeutralInput([]Assignment{scorecardAssignment(3, 1300, 1)})
		in.AutoFails = []AutoFail{kind}
		card, err := ComputeScorecard(in, DefaultScorecardWeights())
		if err != nil {
			t.Fatalf("ComputeScorecard(%s): %v", kind, err)
		}
		if card.Score != 0 {
			t.Errorf("auto-fail %s: score = %v, want 0", kind, card.Score)
		}
		if len(card.AutoFails) != 1 || card.AutoFails[0] != kind {
			t.Errorf("auto-fail %s: AutoFails = %v, want [%s]", kind, card.AutoFails, kind)
		}
		// Components are still reported for diagnosis.
		if card.Components["delivery"] == 0 {
			t.Errorf("auto-fail %s: delivery component zeroed, want reported", kind)
		}
	}
}

func TestComputeScorecardUnratedExcluded(t *testing.T) {
	rated := scorecardAssignment(3, 1300, 1)
	unratedFlag := scorecardAssignment(5, 1600, 0)
	unratedFlag.Rated = false
	unratedNaN := scorecardAssignment(2, 1600, math.NaN())

	base, err := ComputeScorecard(scorecardNeutralInput([]Assignment{rated}), DefaultScorecardWeights())
	if err != nil {
		t.Fatalf("ComputeScorecard(base): %v", err)
	}
	with, err := ComputeScorecard(
		scorecardNeutralInput([]Assignment{rated, unratedFlag, unratedNaN}),
		DefaultScorecardWeights())
	if err != nil {
		t.Fatalf("ComputeScorecard(with unrated): %v", err)
	}

	if with.Unrated != 2 {
		t.Errorf("Unrated = %d, want 2", with.Unrated)
	}
	if with.Expected != base.Expected || with.Actual != base.Actual || with.Regret != base.Regret {
		t.Errorf("unrated assignments changed totals: expected %v/%v actual %v/%v regret %v/%v",
			with.Expected, base.Expected, with.Actual, base.Actual, with.Regret, base.Regret)
	}
	if with.Components["delivery"] != base.Components["delivery"] {
		t.Errorf("unrated assignments changed delivery: %v vs %v",
			with.Components["delivery"], base.Components["delivery"])
	}
}

func TestComputeScorecardWeightsValidation(t *testing.T) {
	in := scorecardNeutralInput([]Assignment{scorecardAssignment(3, 1500, 1)})
	bad := ScorecardWeights{Delivery: 0.35, Review: 0.25, Breakdown: 0.15, Efficiency: 0.15, Hygiene: 0.05}
	if _, err := ComputeScorecard(in, bad); err == nil {
		t.Fatal("weights summing to 0.95 accepted, want error")
	}
	good := DefaultScorecardWeights()
	sum := good.Delivery + good.Review + good.Breakdown + good.Efficiency + good.Hygiene
	if math.Abs(sum-1) > 1e-9 {
		t.Fatalf("DefaultScorecardWeights sum = %v, want 1", sum)
	}
	if _, err := ComputeScorecard(in, good); err != nil {
		t.Fatalf("default weights rejected: %v", err)
	}
}

func TestComputeScorecardRegretLowersDelivery(t *testing.T) {
	noRegret := scorecardAssignment(3, 1300, 1)
	withRegret := noRegret
	withRegret.BestAvailable = Rating{R: 1900, RD: 50, Vol: InitialVol}

	base, err := ComputeScorecard(scorecardNeutralInput([]Assignment{noRegret}), DefaultScorecardWeights())
	if err != nil {
		t.Fatalf("ComputeScorecard(no regret): %v", err)
	}
	regretted, err := ComputeScorecard(scorecardNeutralInput([]Assignment{withRegret}), DefaultScorecardWeights())
	if err != nil {
		t.Fatalf("ComputeScorecard(with regret): %v", err)
	}
	if base.Regret != 0 {
		t.Errorf("best == chosen: regret = %v, want 0", base.Regret)
	}
	if regretted.Regret <= 0 {
		t.Errorf("stronger best available: regret = %v, want > 0", regretted.Regret)
	}
	if regretted.Components["delivery"] >= base.Components["delivery"] {
		t.Errorf("regret did not lower delivery: %v vs %v",
			regretted.Components["delivery"], base.Components["delivery"])
	}
}

func TestComputeScorecardReviewComponent(t *testing.T) {
	tests := []struct {
		name                               string
		planted, caught, escaped, falseRej int
		want                               float64
	}{
		{"perfect", 4, 4, 0, 0, 1},
		{"partial recall with escapes", 4, 3, 1, 2, 0.75 - 0.25 - 0.2},
		{"nothing planted clean", 0, 0, 0, 0, 1},
		{"nothing planted but escapes", 0, 0, 2, 0, 0},
		{"clamped at zero", 4, 0, 3, 3, 0},
	}
	for _, tc := range tests {
		in := scorecardNeutralInput([]Assignment{scorecardAssignment(3, 1500, 1)})
		in.PlantedDefects = tc.planted
		in.PlantedCaught = tc.caught
		in.EscapedRegressions = tc.escaped
		in.FalseRejections = tc.falseRej
		card, err := ComputeScorecard(in, DefaultScorecardWeights())
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if math.Abs(card.Components["review"]-tc.want) > 1e-9 {
			t.Errorf("%s: review = %v, want %v", tc.name, card.Components["review"], tc.want)
		}
	}
}

func TestComputeScorecardBreakdownComponent(t *testing.T) {
	in := scorecardNeutralInput([]Assignment{scorecardAssignment(3, 1500, 1)})
	in.SpecFailures = 2
	in.ReSplits = 3
	card, err := ComputeScorecard(in, DefaultScorecardWeights())
	if err != nil {
		t.Fatal(err)
	}
	if want := 1 - 0.2*2 - 0.1*3; math.Abs(card.Components["breakdown"]-want) > 1e-9 {
		t.Errorf("breakdown = %v, want %v", card.Components["breakdown"], want)
	}
	in.SpecFailures = 10
	card, err = ComputeScorecard(in, DefaultScorecardWeights())
	if err != nil {
		t.Fatal(err)
	}
	if card.Components["breakdown"] != 0 {
		t.Errorf("breakdown = %v, want clamped to 0", card.Components["breakdown"])
	}
}

func TestComputeScorecardEfficiencyMissingExpected(t *testing.T) {
	in := scorecardNeutralInput([]Assignment{scorecardAssignment(3, 1500, 1)})
	in.ExpectedCostPerPoint = 0
	in.ExpectedWallPerPoint = 0
	card, err := ComputeScorecard(in, DefaultScorecardWeights())
	if err != nil {
		t.Fatal(err)
	}
	if card.Components["efficiency"] != 0.5 {
		t.Errorf("efficiency with no expectations = %v, want 0.5", card.Components["efficiency"])
	}
	found := false
	for _, note := range card.Notes {
		if strings.Contains(note, "expected") {
			found = true
		}
	}
	if !found {
		t.Errorf("missing-expectation note absent from %v", card.Notes)
	}

	// Twice the expected cost halves the cost ratio; wall on target stays 1.
	in = scorecardNeutralInput([]Assignment{scorecardAssignment(3, 1500, 1)})
	in.CostPerPoint = 4 // expected 2
	card, err = ComputeScorecard(in, DefaultScorecardWeights())
	if err != nil {
		t.Fatal(err)
	}
	if want := (0.5 + 1.0) / 2; math.Abs(card.Components["efficiency"]-want) > 1e-9 {
		t.Errorf("efficiency = %v, want %v", card.Components["efficiency"], want)
	}
}

func TestComputeScorecardHygieneFailClosed(t *testing.T) {
	in := scorecardNeutralInput([]Assignment{scorecardAssignment(3, 1500, 1)})
	in.HygieneChecksPassed = 0
	in.HygieneChecksTotal = 0
	card, err := ComputeScorecard(in, DefaultScorecardWeights())
	if err != nil {
		t.Fatal(err)
	}
	if card.Components["hygiene"] != 0 {
		t.Errorf("hygiene with zero checks = %v, want 0 (fail closed)", card.Components["hygiene"])
	}
	found := false
	for _, note := range card.Notes {
		if strings.Contains(note, "hygiene") {
			found = true
		}
	}
	if !found {
		t.Errorf("hygiene fail-closed note absent from %v", card.Notes)
	}
}

func TestComputeScorecardSupervisorInstructionsReportedNotScored(t *testing.T) {
	quiet := scorecardNeutralInput([]Assignment{scorecardAssignment(3, 1300, 1)})
	steered := quiet
	steered.SupervisorInstructions = 12

	a, err := ComputeScorecard(quiet, DefaultScorecardWeights())
	if err != nil {
		t.Fatal(err)
	}
	b, err := ComputeScorecard(steered, DefaultScorecardWeights())
	if err != nil {
		t.Fatal(err)
	}
	if a.Score != b.Score {
		t.Errorf("supervisor instructions changed score: %v vs %v", a.Score, b.Score)
	}
	if b.SupervisorInstructions != 12 {
		t.Errorf("SupervisorInstructions = %d, want 12", b.SupervisorInstructions)
	}
}

func TestSprintOpponent(t *testing.T) {
	agent := func(r float64) Rating { return Rating{R: r, RD: 50, Vol: InitialVol} }
	assignments := []Assignment{
		{Points: 1, StoryRating: 1400, AgentRating: agent(1400), Rated: true, Outcome: 1},
		{Points: 3, StoryRating: 1600, AgentRating: agent(1500), Rated: true, Outcome: 1},
	}
	// Points-weighted mean story = (1*1400 + 3*1600) / 4 = 1550.
	// Mean story 1500, mean agent 1450 -> +50 for the weaker team.
	opp := SprintOpponent(assignments)
	if math.Abs(opp.R-1600) > 1e-9 {
		t.Errorf("SprintOpponent R = %v, want 1600", opp.R)
	}
	if opp.RD != 50 {
		t.Errorf("SprintOpponent RD = %v, want 50", opp.RD)
	}

	// A stronger team makes the same sprint an easier opponent.
	strong := []Assignment{
		{Points: 1, StoryRating: 1400, AgentRating: agent(1700), Rated: true, Outcome: 1},
		{Points: 3, StoryRating: 1600, AgentRating: agent(1700), Rated: true, Outcome: 1},
	}
	if got := SprintOpponent(strong); got.R >= opp.R {
		t.Errorf("stronger team: opponent R = %v, want < %v", got.R, opp.R)
	}

	// No assignments: neutral opponent at the initial rating.
	empty := SprintOpponent(nil)
	if empty.R != InitialR || empty.RD != 50 {
		t.Errorf("SprintOpponent(nil) = %+v, want R=%v RD=50", empty, InitialR)
	}
}

func TestComputeScorecardScoreIsWeightedSum(t *testing.T) {
	in := scorecardNeutralInput([]Assignment{scorecardAssignment(3, 1300, 1)})
	in.SpecFailures = 1
	w := DefaultScorecardWeights()
	card, err := ComputeScorecard(in, w)
	if err != nil {
		t.Fatal(err)
	}
	want := w.Delivery*card.Components["delivery"] +
		w.Review*card.Components["review"] +
		w.Breakdown*card.Components["breakdown"] +
		w.Efficiency*card.Components["efficiency"] +
		w.Hygiene*card.Components["hygiene"]
	if math.Abs(card.Score-want) > 1e-9 {
		t.Errorf("score = %v, want weighted sum %v", card.Score, want)
	}
	if card.Score < 0 || card.Score > 1 {
		t.Errorf("score = %v, want within [0,1]", card.Score)
	}
}
