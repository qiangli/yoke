package ladder

import "sort"

// EstimateInput describes the information used to choose an estimation panel.
type EstimateInput struct {
	Story     string
	Expected  Points
	CrossRepo bool
	Design    bool
}

// EstimatePanelSize returns the required number of blind estimators.
func EstimatePanelSize(in EstimateInput) int {
	if in.Expected >= 3 || in.CrossRepo || in.Design {
		return 3
	}
	return 1
}

// EstimateRound is one panelist's blind estimate and optional revote.
type EstimateRound struct {
	Agent      string
	First      Points
	Confidence float64
	Revote     Points
}

// DelphiResult is the outcome of a Delphi estimate round.
type DelphiResult struct {
	Points    Points
	Split     bool
	Escalated bool
	Reason    string
}

// DelphiMedian settles a one- or three-panel Delphi estimate.
func DelphiMedian(rounds []EstimateRound) DelphiResult {
	for _, round := range rounds {
		if !estimateValid(round.First) || (round.Revote != 0 && !estimateValid(round.Revote)) {
			return DelphiResult{Reason: "invalid estimate from " + round.Agent}
		}
	}

	switch len(rounds) {
	case 1:
		result := DelphiResult{Points: rounds[0].First}
		if rounds[0].Confidence < .5 || rounds[0].First >= 8 {
			result.Escalated = true
			result.Reason = "escalate to 3"
		}
		return result
	case 3:
		values := make([]Points, len(rounds))
		split := false
		for i, round := range rounds {
			values[i] = round.First
			if round.Revote != 0 {
				values[i] = round.Revote
			}
			if values[i] >= 13 {
				split = true
			}
		}
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		result := DelphiResult{Points: values[1], Split: split || values[1] >= 13}
		if result.Split {
			result.Reason = "must be split"
		}
		return result
	default:
		return DelphiResult{Reason: "need 1 or 3 estimates"}
	}
}

// EstimatorScore records how closely one panelist's blind estimate matched settlement.
type EstimatorScore struct {
	Agent    string
	Estimate Points
	Miss     int
	Score    float64
}

// ScoreEstimators scores each panelist's first estimate against settled points.
func ScoreEstimators(rounds []EstimateRound, settled Points) []EstimatorScore {
	scores := make([]EstimatorScore, len(rounds))
	settledBucket := estimateBucket(settled)
	for i, round := range rounds {
		miss := estimateBucket(round.First) - settledBucket
		if miss < 0 {
			miss = -miss
		}
		scores[i] = EstimatorScore{
			Agent:    round.Agent,
			Estimate: round.First,
			Miss:     miss,
			Score:    1 - EstimatePenalty(miss),
		}
	}
	return scores
}

func estimateValid(points Points) bool {
	return ValidPoints(points) || points == 13
}

func estimateBucket(points Points) int {
	if points == 13 {
		return 5
	}
	return storyBucket(points)
}
