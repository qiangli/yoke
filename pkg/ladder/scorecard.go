package ladder

import (
	"errors"
	"fmt"
	"math"
)

// This file scores one managed sprint (design section 10): the manager is
// judged against the team's expected result, not the raw outcome, so beating
// expectation with a weak team outranks missing it with a strong team.

// AutoFail is a condition that zeroes the sprint score regardless of the
// component values (design section 10, "Automatic fails").
type AutoFail string

const (
	// AutoFailFalseDone is reporting work done while its gates are red, or
	// other false claims.
	AutoFailFalseDone AutoFail = "false-done"
	// AutoFailRedGateMerge is merging a red gate.
	AutoFailRedGateMerge AutoFail = "red-gate-merge"
	// AutoFailDominanceBreach is violating the dominance invariant or
	// waiving an escalation.
	AutoFailDominanceBreach AutoFail = "dominance-breach"
	// AutoFailBrokenSharedState is leaving shared state broken: stranded
	// leases, lost unmerged work.
	AutoFailBrokenSharedState AutoFail = "broken-shared-state"
	// AutoFailDetectedBypass is acting outside the managed-sprint path in a
	// way that enforcement detects.
	AutoFailDetectedBypass AutoFail = "detected-bypass"
)

// scorecardStoryRD is the uncertainty assigned to a story rating when it is
// used as a Glicko opponent for an assignment's expected score. Story ratings
// are settled scalars, so the RD is small but nonzero.
const scorecardStoryRD = 30.0

// Assignment is one agent × story pairing the manager made, with its outcome.
// Outcome is the blame-attributed score in {1, 0.5, 0}; an assignment blame
// left unrated carries Rated == false (or a NaN outcome) and is excluded from
// delivery and regret, only counted.
type Assignment struct {
	Story         string
	Points        int
	StoryRating   float64
	Agent         string
	AgentRating   Rating
	BestAvailable Rating
	Outcome       float64
	Rated         bool
}

// ScorecardInput is everything a sprint scorecard is computed from.
// SupervisorInstructions is reported beside the score, never in it.
type ScorecardInput struct {
	Assignments []Assignment

	PlantedDefects     int
	PlantedCaught      int
	EscapedRegressions int
	FalseRejections    int

	SpecFailures int
	ReSplits     int

	StallDetectMinutes []float64

	CostPerPoint         float64
	ExpectedCostPerPoint float64
	WallPerPoint         float64
	ExpectedWallPerPoint float64

	HygieneChecksPassed int
	HygieneChecksTotal  int

	SupervisorInstructions int
	AutoFails              []AutoFail
}

// ScorecardWeights weighs the five scored components; they must sum to 1.
type ScorecardWeights struct {
	Delivery   float64
	Review     float64
	Breakdown  float64
	Efficiency float64
	Hygiene    float64
}

// DefaultScorecardWeights is the initial weighting from design section 10,
// to be refit after three seasons.
func DefaultScorecardWeights() ScorecardWeights {
	return ScorecardWeights{
		Delivery:   0.35,
		Review:     0.25,
		Breakdown:  0.15,
		Efficiency: 0.15,
		Hygiene:    0.10,
	}
}

// Scorecard is the sprint manager's score: Score is the manage-match outcome
// in [0,1], zero whenever any AutoFail is present. Components are still
// reported in that case so the failure stays diagnosable.
type Scorecard struct {
	Score      float64
	Components map[string]float64

	Expected float64
	Actual   float64
	Regret   float64
	Unrated  int

	SupervisorInstructions int
	AutoFails              []AutoFail
	Notes                  []string
}

// ComputeScorecard scores one sprint from its inputs. Pure computation: no
// I/O, no clock.
//
// Delivery maps the value added (actual − expected points, both summed over
// rated assignments only) onto [0,1] with 0.5 as "exactly as expected":
// clamp(0.5 + (actual−expected)/(2·max(expected,1))), so delivering double
// the expectation saturates at 1 and delivering nothing bottoms at 0.
// Assignment regret — Σ points × (Expected(best) − Expected(chosen)) — is
// reported on its own and folded into delivery as a penalty of
// regret/(2·ratedPoints), not weighted as a separate component.
func ComputeScorecard(in ScorecardInput, w ScorecardWeights) (Scorecard, error) {
	sum := w.Delivery + w.Review + w.Breakdown + w.Efficiency + w.Hygiene
	if math.Abs(sum-1) > 1e-9 {
		return Scorecard{}, fmt.Errorf("ladder: scorecard weights sum to %v, want 1", sum)
	}
	if w.Delivery < 0 || w.Review < 0 || w.Breakdown < 0 || w.Efficiency < 0 || w.Hygiene < 0 {
		return Scorecard{}, errors.New("ladder: scorecard weights must be non-negative")
	}

	card := Scorecard{
		Components:             make(map[string]float64, 5),
		SupervisorInstructions: in.SupervisorInstructions,
		AutoFails:              in.AutoFails,
	}

	// Delivery and regret over rated assignments only.
	var ratedPoints int
	for _, a := range in.Assignments {
		if !a.Rated || math.IsNaN(a.Outcome) {
			card.Unrated++
			continue
		}
		pts := float64(a.Points)
		story := Rating{R: a.StoryRating, RD: scorecardStoryRD, Vol: InitialVol}
		card.Expected += pts * Expected(a.AgentRating, story)
		card.Actual += pts * a.Outcome
		card.Regret += pts * (Expected(a.BestAvailable, story) - Expected(a.AgentRating, story))
		ratedPoints += a.Points
	}
	delivery := scorecardClamp01(0.5 + (card.Actual-card.Expected)/(2*math.Max(card.Expected, 1)))
	if ratedPoints > 0 {
		delivery = scorecardClamp01(delivery - card.Regret/(2*float64(ratedPoints)))
	} else {
		card.Notes = append(card.Notes, "no rated assignments; delivery neutral")
	}
	card.Components["delivery"] = delivery

	// Review: planted recall, minus escaped regressions and false rejections.
	recall := 0.0
	switch {
	case in.PlantedDefects > 0:
		recall = float64(in.PlantedCaught) / float64(in.PlantedDefects)
	case in.EscapedRegressions == 0:
		recall = 1
	}
	card.Components["review"] = scorecardClamp01(
		recall - 0.25*float64(in.EscapedRegressions) - 0.1*float64(in.FalseRejections))

	card.Components["breakdown"] = scorecardClamp01(
		1 - 0.2*float64(in.SpecFailures) - 0.1*float64(in.ReSplits))

	costRatio := scorecardRatio(in.ExpectedCostPerPoint, in.CostPerPoint, "cost", &card.Notes)
	wallRatio := scorecardRatio(in.ExpectedWallPerPoint, in.WallPerPoint, "wall", &card.Notes)
	card.Components["efficiency"] = (costRatio + wallRatio) / 2

	// Hygiene fails closed: a sprint that ran no cleanup checks scores 0.
	if in.HygieneChecksTotal > 0 {
		card.Components["hygiene"] = scorecardClamp01(
			float64(in.HygieneChecksPassed) / float64(in.HygieneChecksTotal))
	} else {
		card.Components["hygiene"] = 0
		card.Notes = append(card.Notes, "no hygiene checks recorded; hygiene fails closed to 0")
	}

	if len(in.AutoFails) > 0 {
		card.Score = 0
		return card, nil
	}
	card.Score = w.Delivery*card.Components["delivery"] +
		w.Review*card.Components["review"] +
		w.Breakdown*card.Components["breakdown"] +
		w.Efficiency*card.Components["efficiency"] +
		w.Hygiene*card.Components["hygiene"]
	return card, nil
}

// SprintOpponent is the sprint's difficulty expressed as one Glicko opponent
// for the manage match: the points-weighted mean story rating, shifted by
// (mean story rating − mean agent rating) so a hard sprint run with a weak
// team is worth more than the same sprint with a strong one. RD is fixed at
// 50: the sprint is a settled, low-uncertainty opponent.
func SprintOpponent(assignments []Assignment) Rating {
	opp := Rating{R: InitialR, RD: 50, Vol: InitialVol}
	if len(assignments) == 0 {
		return opp
	}
	var weighted, points, storySum, agentSum float64
	for _, a := range assignments {
		weighted += float64(a.Points) * a.StoryRating
		points += float64(a.Points)
		storySum += a.StoryRating
		agentSum += a.AgentRating.R
	}
	n := float64(len(assignments))
	base := storySum / n
	if points > 0 {
		base = weighted / points
	}
	opp.R = base + (storySum/n - agentSum/n)
	return opp
}

// scorecardRatio maps expected/actual onto [0,1] (on-budget or better = 1).
// A missing expectation cannot be scored either way; it lands neutral at 0.5
// with a note. A non-positive actual means the measurement was never
// recorded, not that the sprint was free: it also lands neutral at 0.5 —
// a perfect 1 would reward the sprint that lost its meter.
func scorecardRatio(expected, actual float64, name string, notes *[]string) float64 {
	if expected <= 0 {
		*notes = append(*notes, fmt.Sprintf("no expected %s per point; %s ratio neutral at 0.5", name, name))
		return 0.5
	}
	if actual <= 0 {
		*notes = append(*notes, fmt.Sprintf("actual %s per point not recorded; %s ratio neutral at 0.5", name, name))
		return 0.5
	}
	return scorecardClamp01(expected / actual)
}

func scorecardClamp01(v float64) float64 {
	return math.Min(1, math.Max(0, v))
}
