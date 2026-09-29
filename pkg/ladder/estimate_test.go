package ladder

import "testing"

func TestEstimatePanelSize(t *testing.T) {
	for _, test := range []struct {
		name string
		in   EstimateInput
		want int
	}{
		{"routine one", EstimateInput{Expected: 1}, 1},
		{"routine two", EstimateInput{Expected: 2}, 1},
		{"larger", EstimateInput{Expected: 3}, 3},
		{"cross repository", EstimateInput{Expected: 1, CrossRepo: true}, 3},
		{"design", EstimateInput{Expected: 2, Design: true}, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := EstimatePanelSize(test.in); got != test.want {
				t.Fatalf("EstimatePanelSize(%+v) = %d, want %d", test.in, got, test.want)
			}
		})
	}
}

func TestDelphiMedianSingle(t *testing.T) {
	for _, test := range []struct {
		name  string
		round EstimateRound
		want  DelphiResult
	}{
		{"stands", EstimateRound{Agent: "agent-a", First: 3, Confidence: .5}, DelphiResult{Points: 3}},
		{"low confidence", EstimateRound{Agent: "agent-a", First: 3, Confidence: .49}, DelphiResult{Points: 3, Escalated: true, Reason: "escalate to 3"}},
		{"large", EstimateRound{Agent: "agent-a", First: 8, Confidence: 1}, DelphiResult{Points: 8, Escalated: true, Reason: "escalate to 3"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := DelphiMedian([]EstimateRound{test.round}); got != test.want {
				t.Fatalf("DelphiMedian() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestDelphiMedianThreeUsesRevotes(t *testing.T) {
	rounds := []EstimateRound{
		{Agent: "agent-a", First: 1, Revote: 5},
		{Agent: "agent-b", First: 3},
		{Agent: "agent-c", First: 8, Revote: 5},
	}
	if got, want := DelphiMedian(rounds), (DelphiResult{Points: 5}); got != want {
		t.Fatalf("DelphiMedian() = %+v, want %+v", got, want)
	}
}

func TestDelphiMedianSplitOnThirteen(t *testing.T) {
	rounds := []EstimateRound{
		{Agent: "agent-a", First: 8},
		{Agent: "agent-b", First: 13},
		{Agent: "agent-c", First: 8},
	}
	if got, want := DelphiMedian(rounds), (DelphiResult{Points: 8, Split: true, Reason: "must be split"}); got != want {
		t.Fatalf("DelphiMedian() = %+v, want %+v", got, want)
	}
}

func TestDelphiMedianInvalidValueNamesAgent(t *testing.T) {
	got := DelphiMedian([]EstimateRound{{Agent: "agent-b", First: 4, Confidence: 1}})
	if got.Reason == "" || got.Points != 0 {
		t.Fatalf("DelphiMedian() = %+v, want invalid result", got)
	}
	if got.Reason != "invalid estimate from agent-b" {
		t.Fatalf("DelphiMedian() reason = %q, want agent name", got.Reason)
	}
}

func TestScoreEstimatorsUsesFirstEstimate(t *testing.T) {
	rounds := []EstimateRound{{Agent: "agent-a", First: 1, Revote: 8}}
	got := ScoreEstimators(rounds, 3)
	want := []EstimatorScore{{Agent: "agent-a", Estimate: 1, Miss: 2, Score: 0}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("ScoreEstimators() = %+v, want %+v", got, want)
	}
}

func TestScoreEstimatorsThirteenIsOneBeyondEight(t *testing.T) {
	got := ScoreEstimators([]EstimateRound{{Agent: "agent-a", First: 13}}, 8)
	want := EstimatorScore{Agent: "agent-a", Estimate: 13, Miss: 1, Score: .5}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("ScoreEstimators() = %+v, want %+v", got, want)
	}
}
