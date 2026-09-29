package ladder

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ladder/blame"
)

func eventTestAt(n int) time.Time { return time.Date(2026, 1, n, 0, 0, 0, 0, time.UTC) }
func eventTestAgentBlame() blame.Attribution {
	return blame.Attribution{Class: blame.ClassAgent, By: "reviewer", Evidence: []blame.Evidence{{Kind: blame.EvidenceReview, Ref: "review-1"}}}
}
func eventTestDelivery(id, agent, story string, season int, outcome float64) Event {
	return Event{ID: id, At: eventTestAt(season), Season: season, Kind: EventKindDelivery, Agent: agent, Story: story, Points: 2, Outcome: outcome, Duty: DutyCode}
}
func eventTestClose(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-8 {
		t.Fatalf("got %.9f, want %.9f", got, want)
	}
}

func TestReplayGoldenSeasonAndUnrated(t *testing.T) {
	a := eventTestDelivery("a", "agent-a", "story-a", 1, 1)
	b := eventTestDelivery("b", "agent-a", "story-b", 1, 0)
	b.Blame = blame.Attribution{Class: blame.ClassEnvironment}
	got := Replay([]Event{a, b}, 1)
	want := Update(NewRating(), []Result{{Opponent: Rating{R: 1450, RD: InitialRD, Vol: InitialVol}, Score: 1}}, DefaultTau)
	standing := got.Agents["agent-a"].Standings[DutyCode]
	eventTestClose(t, standing.R, want.R)
	eventTestClose(t, standing.RD, want.RD)
	if standing.Events != 1 || got.Agents["agent-a"].Unrated != 1 {
		t.Fatalf("standing=%+v agent=%+v", standing, got.Agents["agent-a"])
	}
	storyWant := Update(Rating{R: 1450, RD: InitialRD, Vol: InitialVol}, []Result{{Opponent: NewRating(), Score: 0}}, DefaultTau)
	eventTestClose(t, got.Stories["story-a"].Rating.R, storyWant.R)
	if got.Stories["story-b"].Attempts != 1 {
		t.Fatal(got.Stories["story-b"])
	}
}
func TestReplayRegressionAndDecay(t *testing.T) {
	a := eventTestDelivery("a", "agent-a", "story-a", 1, 1)
	base := Replay([]Event{a}, 3)
	regression := Event{At: eventTestAt(3), Season: 3, Kind: EventKindRegression, Story: "story-a"}
	changed := Replay([]Event{a, regression}, 3)
	if changed.Agents["agent-a"].Standings[DutyCode].R >= base.Agents["agent-a"].Standings[DutyCode].R {
		t.Fatal("regression did not lower rating")
	}
	if changed.Agents["agent-a"].Standings[DutyCode].Events != 1 {
		t.Fatal("regression added result")
	}
	late := regression
	late.Season = 4
	late.At = eventTestAt(4)
	ignored := Replay([]Event{a, late}, 4)
	clean := Replay([]Event{a}, 4)
	if !reflect.DeepEqual(ignored.Agents, clean.Agents) || len(ignored.Ignored) != 1 {
		t.Fatalf("late regression changed rating: %+v", ignored)
	}
	one := Replay([]Event{a}, 1).Agents["agent-a"].Standings[DutyCode]
	two := Replay([]Event{a}, 2).Agents["agent-a"].Standings[DutyCode]
	if two.RD <= one.RD {
		t.Fatalf("inactive RD did not rise: %v -> %v", one.RD, two.RD)
	}
}
func TestReplayJudgeSettledRatingAndCorrection(t *testing.T) {
	a := eventTestDelivery("a", "agent-a", "story-a", 1, 1)
	judge := Event{At: eventTestAt(1), Season: 1, Kind: EventKindEstimate, Agent: "judge-a", Story: "story-a", Estimate: 2}
	withJudge := Replay([]Event{judge, a}, 1)
	settled := withJudge.Stories["story-a"].Rating
	bucket := Points(1)
	best := math.Inf(1)
	for _, p := range []Points{1, 2, 3, 5, 8} {
		r, _ := StoryInitialRating(p)
		if d := math.Abs(settled.R - r); d < best {
			best = d
			bucket = p
		}
	}
	miss := storyBucket(2) - storyBucket(bucket)
	if miss < 0 {
		miss = -miss
	}
	want := Update(NewRating(), []Result{{Opponent: Rating{R: settled.R, RD: 50}, Score: 1 - EstimatePenalty(miss)}}, DefaultTau)
	eventTestClose(t, withJudge.Agents["judge-a"].Standings[DutyJudge].R, want.R)
	correction := Event{At: eventTestAt(2), Season: 2, Kind: EventKindCorrection, Agent: "agent-a", Supersedes: "a"}
	dropped := Replay([]Event{a, correction}, 2)
	if len(dropped.Stories) != 0 || dropped.Agents["agent-a"] != nil {
		t.Fatalf("correction failed: %+v", dropped)
	}
}
func TestReplayEqualTimeOrder(t *testing.T) {
	a := eventTestDelivery("a", "agent-a", "story-a", 1, 1)
	b := eventTestDelivery("b", "agent-a", "story-b", 1, .5)
	b.At = a.At
	x := Replay([]Event{a, b}, 1)
	y := Replay([]Event{b, a}, 1)
	if !reflect.DeepEqual(x, y) {
		t.Fatalf("order changed replay: %+v vs %+v", x, y)
	}
}
