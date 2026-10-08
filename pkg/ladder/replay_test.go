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

func TestReplayAttributedInstancesAccumulateByFamily(t *testing.T) {
	a := eventTestDelivery("a", "alias-one", "story-a", 1, 1)
	a.InstanceUUID, a.FamilyID, a.SelectedBinding = "uuid-1", "family:v1", "tool:model-v1"
	b := eventTestDelivery("b", "reused-label", "story-b", 1, .5)
	b.InstanceUUID, b.FamilyID, b.SelectedBinding = "uuid-2", "family:v1", "tool:model-v2"
	got := Replay([]Event{a, b}, 1)
	if got.Agents["alias-one"] != nil || got.Agents["reused-label"] != nil {
		t.Fatalf("aliases split family standing: %+v", got.Agents)
	}
	if family := got.Agents["family:v1"]; family == nil || family.Standings[DutyCode].Events != 2 {
		t.Fatalf("two instances did not accumulate: %+v", family)
	}
}

func TestReplayNewFamilyDoesNotInheritAndLegacyIsUnchanged(t *testing.T) {
	legacy := eventTestDelivery("legacy", "tool:model", "story-a", 1, 1)
	configured := eventTestDelivery("configured", "tool:model", "story-b", 1, 1)
	configured.FamilyID, configured.InstanceUUID = "family:v2", "uuid-2"
	got := Replay([]Event{legacy, configured}, 1)
	if got.Agents["tool:model"].Standings[DutyCode].Events != 1 || got.Agents["family:v2"].Standings[DutyCode].Events != 1 {
		t.Fatalf("legacy and new configuration blended: %+v", got.Agents)
	}
}

func TestReplayInvalidEstimateChargesFamilyIdentity(t *testing.T) {
	e := Event{ID: "estimate", At: eventTestAt(1), Season: 1, Kind: EventKindEstimate, Agent: "reused-label", FamilyID: "family:v1", InstanceUUID: "uuid-1", Story: "missing", Estimate: 2}
	got := Replay([]Event{e}, 1)
	if got.Agents["reused-label"] != nil || got.Agents["family:v1"] == nil || got.Agents["family:v1"].Unrated != 1 {
		t.Fatalf("invalid estimate attributed to wrong identity: %+v", got.Agents)
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

// Story #1197, design section 7: spec failures charge the estimator and author.
func eventTestSpecBlame() blame.Attribution {
	return blame.Attribution{Class: blame.ClassSpec, By: "reviewer", Evidence: []blame.Evidence{{Kind: blame.EvidenceAmbiguity, Ref: "thread-1", Note: "acceptance contradicts the spec-ref"}}}
}
func eventTestEnvBlame() blame.Attribution {
	return blame.Attribution{Class: blame.ClassEnvironment, By: "reviewer", Evidence: []blame.Evidence{{Kind: blame.EvidenceQuota, Ref: "quota-1"}}}
}

// eventTestSpecFixture is a spec-story attempt plus an exact estimate on a
// control story, so the judge's estimate on story-x is scored against an
// otherwise-identical baseline.
func eventTestSpecFixture(attr blame.Attribution) []Event {
	d := eventTestDelivery("d", "agent-a", "story-x", 1, 0)
	d.Blame = attr
	d.Author = "author-m"
	est := Event{ID: "e", At: eventTestAt(1), Season: 1, Kind: EventKindEstimate, Agent: "judge-a", Story: "story-x", Estimate: 2}
	return []Event{d, est}
}

func TestReplaySpecFailureChargesAuthorAndZeroesEstimates(t *testing.T) {
	got := Replay(eventTestSpecFixture(eventTestSpecBlame()), 1)
	impl := got.Agents["agent-a"]
	if impl.Standings[DutyCode].Events != 0 || impl.Unrated != 1 || impl.SpecCharges != 0 {
		t.Fatalf("spec failure must not rate the implementer: %+v", impl)
	}
	author := got.Agents["author-m"]
	if author == nil || author.SpecCharges != 1 {
		t.Fatalf("spec failure must charge the author: %+v", author)
	}
	s := got.Stories["story-x"]
	want := Update(NewRating(), []Result{{Opponent: Rating{R: s.Rating.R, RD: 50}, Score: 0}}, DefaultTau)
	judge := got.Agents["judge-a"].Standings[DutyJudge]
	eventTestClose(t, judge.R, want.R)
	if judge.Events != 1 {
		t.Fatalf("estimate must still be rated (at full penalty): %+v", judge)
	}
}

func TestReplaySpecFailureWithoutAuthorStillZeroesEstimates(t *testing.T) {
	events := eventTestSpecFixture(eventTestSpecBlame())
	events[0].Author = ""
	got := Replay(events, 1)
	for name, a := range got.Agents {
		if a.SpecCharges != 0 {
			t.Fatalf("no author: nobody is charged, got %s=%+v", name, a)
		}
	}
	s := got.Stories["story-x"]
	want := Update(NewRating(), []Result{{Opponent: Rating{R: s.Rating.R, RD: 50}, Score: 0}}, DefaultTau)
	eventTestClose(t, got.Agents["judge-a"].Standings[DutyJudge].R, want.R)
}

func TestReplayEnvironmentFailureChargesNobody(t *testing.T) {
	got := Replay(eventTestSpecFixture(eventTestEnvBlame()), 1)
	if a := got.Agents["author-m"]; a != nil {
		t.Fatalf("environment failure must not charge the author: %+v", a)
	}
	if impl := got.Agents["agent-a"]; impl.Standings[DutyCode].Events != 0 || impl.Unrated != 1 {
		t.Fatalf("environment failure must not rate the implementer: %+v", impl)
	}
	// Estimate 2 on a 2-point story with no rated attempts: exact, no penalty.
	s := got.Stories["story-x"]
	want := Update(NewRating(), []Result{{Opponent: Rating{R: s.Rating.R, RD: 50}, Score: 1}}, DefaultTau)
	eventTestClose(t, got.Agents["judge-a"].Standings[DutyJudge].R, want.R)
}

func TestReplayAgentFailureRatesImplementerOnly(t *testing.T) {
	got := Replay(eventTestSpecFixture(eventTestAgentBlame()), 1)
	if a := got.Agents["author-m"]; a != nil {
		t.Fatalf("agent failure must not charge the author: %+v", a)
	}
	impl := got.Agents["agent-a"]
	if impl.Standings[DutyCode].Events != 1 || impl.Unrated != 0 || impl.SpecCharges != 0 {
		t.Fatalf("agent failure must rate the implementer: %+v", impl)
	}
	// The estimate is scored against the settled bucket, not forced to 0.
	s := got.Stories["story-x"]
	bucket := Points(1)
	best := math.Inf(1)
	for _, p := range []Points{1, 2, 3, 5, 8} {
		r, _ := StoryInitialRating(p)
		if d := math.Abs(s.Rating.R - r); d < best {
			best, bucket = d, p
		}
	}
	miss := storyBucket(2) - storyBucket(bucket)
	if miss < 0 {
		miss = -miss
	}
	want := Update(NewRating(), []Result{{Opponent: Rating{R: s.Rating.R, RD: 50}, Score: 1 - EstimatePenalty(miss)}}, DefaultTau)
	eventTestClose(t, got.Agents["judge-a"].Standings[DutyJudge].R, want.R)
}

func eventTestSeed(id, agent string, duty Duty, at time.Time, r, rd float64) Event {
	return Event{ID: id, At: at, Season: 1, Kind: EventKindSeed, Agent: agent, Duty: duty, SeedR: r, SeedRD: rd}
}

// A seed replaces NewRating as the agent's starting point for its duty, with
// the RD floored at 150, and never counts as a rated event.
func TestReplaySeedIsStartingRating(t *testing.T) {
	seed := eventTestSeed("s", "agent-a", DutyCode, eventTestAt(1), 1700, 60)
	a := eventTestDelivery("a", "agent-a", "story-a", 1, 1)
	a.At = eventTestAt(2)
	got := Replay([]Event{seed, a}, 1)
	start := Rating{R: 1700, RD: SeedRDFloor, Vol: InitialVol}
	want := Update(start, []Result{{Opponent: Rating{R: 1450, RD: InitialRD, Vol: InitialVol}, Score: 1}}, DefaultTau)
	standing := got.Agents["agent-a"].Standings[DutyCode]
	eventTestClose(t, standing.R, want.R)
	eventTestClose(t, standing.RD, want.RD)
	if standing.Events != 1 {
		t.Fatalf("seed counted as an event: %+v", standing)
	}
	// The story played the seeded agent, not a default one.
	storyWant := Update(Rating{R: 1450, RD: InitialRD, Vol: InitialVol}, []Result{{Opponent: start, Score: 0}}, DefaultTau)
	eventTestClose(t, got.Stories["story-a"].Rating.R, storyWant.R)
	// Other duties are untouched by a code seed.
	if m := got.Agents["agent-a"].Standings[DutyManage]; m.R != Update(NewRating(), nil, DefaultTau).R {
		t.Fatalf("manage standing moved by a code seed: %+v", m)
	}
}

func TestReplaySeedAloneAndRDAboveFloor(t *testing.T) {
	seed := eventTestSeed("s", "agent-a", DutyJudge, eventTestAt(1), 1600, 200)
	got := Replay([]Event{seed}, 2)
	a := got.Agents["agent-a"]
	if a == nil {
		t.Fatal("seeded agent missing")
	}
	want := Update(Update(Rating{R: 1600, RD: 200, Vol: InitialVol}, nil, DefaultTau), nil, DefaultTau)
	s := a.Standings[DutyJudge]
	eventTestClose(t, s.R, want.R)
	eventTestClose(t, s.RD, want.RD)
	if s.Events != 0 || a.Unrated != 0 {
		t.Fatalf("seed counted: %+v", a)
	}
}

func TestReplayLaterSeedWins(t *testing.T) {
	late := eventTestSeed("late", "agent-a", DutyCode, eventTestAt(3), 1800, 150)
	early := eventTestSeed("early", "agent-a", DutyCode, eventTestAt(2), 1300, 150)
	// Input order must not matter: the latest At wins.
	got := Replay([]Event{late, early}, 1)
	want := Update(Rating{R: 1800, RD: 150, Vol: InitialVol}, nil, DefaultTau)
	eventTestClose(t, got.Agents["agent-a"].Standings[DutyCode].R, want.R)
	// A corrected seed is gone, and the earlier one stands.
	fix := Event{ID: "fix", At: eventTestAt(4), Season: 1, Kind: EventKindCorrection, Agent: "reviewer", Supersedes: "late"}
	got = Replay([]Event{late, early, fix}, 1)
	want = Update(Rating{R: 1300, RD: 150, Vol: InitialVol}, nil, DefaultTau)
	eventTestClose(t, got.Agents["agent-a"].Standings[DutyCode].R, want.R)
}
