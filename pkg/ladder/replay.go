package ladder

import (
	"fmt"
	"math"
	"sort"

	"github.com/qiangli/yoke/pkg/ladder/blame"
)

// AgentRecord contains the ratings and evidence derived from active events.
type AgentRecord struct {
	Agent       string
	Standings   map[Duty]DutyStanding
	Certs       []Certificate
	Provisional int
	Unrated     int
	// SpecCharges counts delivery failures with a valid spec-class
	// attribution on stories this agent authored.
	SpecCharges int
}

// StoryRecord contains a story's settled difficulty and attempt count.
type StoryRecord struct {
	Story    string
	Points   Points
	Rating   Rating
	Attempts int
}

type ReplayResult struct {
	Agents  map[string]*AgentRecord
	Stories map[string]*StoryRecord
	Ignored []string
}

type replayItem struct {
	event Event
	index int
}
type replayRated struct {
	key    string
	result Result
}

func replayAgent(out *ReplayResult, name string) *AgentRecord {
	if a := out.Agents[name]; a != nil {
		return a
	}
	a := &AgentRecord{Agent: name, Standings: make(map[Duty]DutyStanding)}
	out.Agents[name] = a
	return a
}
func replayStory(out *ReplayResult, e Event) *StoryRecord {
	if s := out.Stories[e.Story]; s != nil {
		return s
	}
	r, _ := StoryInitialRating(e.Points)
	s := &StoryRecord{Story: e.Story, Points: e.Points, Rating: Rating{R: r, RD: InitialRD, Vol: InitialVol}}
	out.Stories[e.Story] = s
	return s
}
func replayAdd(m map[string]map[Duty][]replayRated, agent string, duty Duty, key string, r Result) {
	if m[agent] == nil {
		m[agent] = make(map[Duty][]replayRated)
	}
	m[agent][duty] = append(m[agent][duty], replayRated{key: key, result: r})
}
func replaySortedResults(items []replayRated) []Result {
	sort.SliceStable(items, func(i, j int) bool { return items[i].key < items[j].key })
	results := make([]Result, len(items))
	for i, item := range items {
		results[i] = item.result
	}
	return results
}

// SeedRDFloor is the smallest deviation a seed may start with. Public-data
// seeds are priors that must lose to evidence quickly, so a seed never claims
// more certainty than this (manager decision recorded on Sprint #331).
const SeedRDFloor = 150.0

// replaySeeds collects each agent's starting rating per duty from the active
// seed events. Items are in At order, so a later seed replaces an earlier one.
func replaySeeds(active []replayItem) map[string]map[Duty]Rating {
	seeds := make(map[string]map[Duty]Rating)
	for _, item := range active {
		e := item.event
		name := e.RatingAgent()
		if e.Kind != EventKindSeed || name == "" || e.SeedR <= 0 {
			continue
		}
		if e.Duty != DutyCode && e.Duty != DutyManage && e.Duty != DutyJudge {
			continue
		}
		if seeds[name] == nil {
			seeds[name] = make(map[Duty]Rating)
		}
		seeds[name][e.Duty] = Rating{R: e.SeedR, RD: math.Max(e.SeedRD, SeedRDFloor), Vol: InitialVol}
	}
	return seeds
}

// Replay derives current standings from immutable events in season order.
func Replay(events []Event, currentSeason int) ReplayResult {
	out := ReplayResult{Agents: make(map[string]*AgentRecord), Stories: make(map[string]*StoryRecord)}
	items := make([]replayItem, len(events))
	for i, e := range events {
		items[i] = replayItem{event: e, index: i}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].event.At.Before(items[j].event.At) })
	dropped := make(map[string]bool)
	for _, item := range items {
		e := item.event
		if e.Kind == EventKindCorrection && e.Season <= currentSeason && e.Supersedes != "" {
			dropped[e.Supersedes] = true
		}
	}
	active := make([]replayItem, 0, len(items))
	for _, item := range items {
		e := item.event
		if e.Season < 1 || e.Season > currentSeason || dropped[e.ID] || e.Kind == EventKindCorrection {
			continue
		}
		active = append(active, item)
	}
	// Regressions rewrite their delivery before any rating period is applied.
	for _, item := range active {
		e := item.event
		if e.Kind != EventKindRegression {
			continue
		}
		match := -1
		for i, candidate := range active {
			d := candidate.event
			if d.Kind == EventKindDelivery && d.Story == e.Story && d.Season <= e.Season && (match < 0 || d.Season >= active[match].event.Season) {
				match = i
			}
		}
		if match < 0 {
			out.Ignored = append(out.Ignored, fmt.Sprintf("regression %s: delivery missing", e.Story))
			continue
		}
		if e.Season-active[match].event.Season > 2 {
			out.Ignored = append(out.Ignored, fmt.Sprintf("regression %s: beyond two seasons", e.Story))
			continue
		}
		active[match].event.Outcome = 0
		active[match].event.Blame = blame.Attribution{Class: blame.ClassAgent, By: active[match].event.Agent, Evidence: []blame.Evidence{{Kind: blame.EvidenceReview, Ref: fmt.Sprintf("regression:%s:%d", e.Story, item.index)}}}
	}
	// A seed is the ONLY effect of a seed event: it replaces NewRating as the
	// agent's starting point for its duty and is never a rated result.
	agentRatings := replaySeeds(active)
	storyRatings := make(map[string]Rating)
	// A failed delivery with a valid spec-class attribution (design section
	// 7) charges the story's author and every estimator; the implementer
	// stays unrated below. specStories is keyed after regressions rewrite
	// their delivery, so a regressed delivery is agent-class, never spec.
	specStories := make(map[string]bool)
	for _, item := range active {
		e := item.event
		ratingAgent := e.RatingAgent()
		if ratingAgent != "" {
			replayAgent(&out, ratingAgent)
		}
		if e.Kind == EventKindDelivery && e.Outcome == 0 && blame.Consequence(e.Blame) == blame.ActionChargeEstimatorAndAuthor {
			specStories[e.Story] = true
			if e.Author != "" {
				replayAgent(&out, e.Author).SpecCharges++
			}
		}
		if e.Kind == EventKindDelivery && e.Story != "" && ValidPoints(e.Points) {
			s := replayStory(&out, e)
			s.Attempts++
			storyRatings[e.Story] = s.Rating
		}
	}
	for season := 1; season <= currentSeason; season++ {
		agentResults := make(map[string]map[Duty][]replayRated)
		storyResults := make(map[string][]replayRated)
		for _, item := range active {
			e := item.event
			ratingAgent := e.RatingAgent()
			if e.Season != season {
				continue
			}
			switch e.Kind {
			case EventKindDelivery:
				s := out.Stories[e.Story]
				if s == nil || ratingAgent == "" {
					continue
				}
				if !(e.Outcome == 1 || e.Outcome == .5 || (e.Outcome == 0 && blame.Rates(e.Blame))) {
					out.Agents[ratingAgent].Unrated++
					continue
				}
				opponent := storyRatings[e.Story]
				replayAdd(agentResults, ratingAgent, DutyCode, e.Story+e.ID, Result{Opponent: opponent, Score: e.Outcome})
				agentBefore := NewRating()
				if duties := agentRatings[ratingAgent]; duties != nil && duties[DutyCode].R != 0 {
					agentBefore = duties[DutyCode]
				}
				storyResults[e.Story] = append(storyResults[e.Story], replayRated{key: ratingAgent + e.ID, result: Result{Opponent: agentBefore, Score: 1 - e.Outcome}})
			case EventKindManage:
				replayAdd(agentResults, ratingAgent, DutyManage, fmt.Sprintf("%09d:%s", e.Sprint, e.ID), Result{Opponent: e.Opponent, Score: e.Score})
			case EventKindCert:
				if a := out.Agents[ratingAgent]; a != nil {
					c := e.Cert
					if c.Season == 0 {
						c.Season = e.Season
					}
					a.Certs = append(a.Certs, c)
				}
			case EventKindSeat:
				if a := out.Agents[ratingAgent]; a != nil {
					a.Provisional = e.Provisional
				}
			}
		}
		for name, a := range out.Agents {
			if agentRatings[name] == nil {
				agentRatings[name] = make(map[Duty]Rating)
			}
			for _, duty := range []Duty{DutyCode, DutyManage} {
				before := agentRatings[name][duty]
				if before.R == 0 {
					before = NewRating()
				}
				results := replaySortedResults(agentResults[name][duty])
				after := Update(before, results, DefaultTau)
				agentRatings[name][duty] = after
				a.Standings[duty] = DutyStanding{R: after.R, RD: after.RD, Events: a.Standings[duty].Events + len(results)}
			}
		}
		for name, s := range out.Stories {
			before := storyRatings[name]
			results := replaySortedResults(storyResults[name])
			after := Update(before, results, DefaultTau)
			storyRatings[name] = after
			s.Rating = after
		}
	}
	// Blind estimates use the final story rating, including subsequent attempts.
	for season := 1; season <= currentSeason; season++ {
		results := make(map[string]map[Duty][]replayRated)
		for _, item := range active {
			e := item.event
			if e.Kind != EventKindEstimate || e.Season != season {
				continue
			}
			s := out.Stories[e.Story]
			if s == nil || !ValidPoints(e.Estimate) {
				if a := out.Agents[e.Agent]; a != nil {
					a.Unrated++
				}
				continue
			}
			bucket := Points(1)
			distance := math.Inf(1)
			for _, p := range []Points{1, 2, 3, 5, 8} {
				r, _ := StoryInitialRating(p)
				if d := math.Abs(s.Rating.R - r); d < distance {
					distance = d
					bucket = p
				}
			}
			miss := storyBucket(e.Estimate) - storyBucket(bucket)
			if miss < 0 {
				miss = -miss
			}
			score := 1 - EstimatePenalty(miss)
			if specStories[e.Story] {
				// The story could not be built as written: full penalty.
				score = 0
			}
			replayAdd(results, e.RatingAgent(), DutyJudge, e.Story+e.ID, Result{Opponent: Rating{R: s.Rating.R, RD: 50}, Score: score})
		}
		for name, a := range out.Agents {
			before := agentRatings[name][DutyJudge]
			if before.R == 0 {
				before = NewRating()
			}
			rated := replaySortedResults(results[name][DutyJudge])
			after := Update(before, rated, DefaultTau)
			agentRatings[name][DutyJudge] = after
			a.Standings[DutyJudge] = DutyStanding{R: after.R, RD: after.RD, Events: a.Standings[DutyJudge].Events + len(rated)}
		}
	}
	return out
}
