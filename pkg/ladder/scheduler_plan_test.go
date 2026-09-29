package ladder

import "testing"

func planEntrant(name string, r, cost float64, rank int) Entrant {
	e := schedulerEntrant(name, 4, r, 50, cost)
	e.PlanRank = rank
	e.Standings[DutyManage] = DutyStanding{R: r, RD: 50}
	return e
}

// Story 5fec08b11a5f: heavy and manager work prefers the entrant billed through
// the higher plan tier, but only among near-equal matches — plan tier never
// buys a clearly worse fit, and light work still goes to the cheapest match.
func TestScheduleStoryHeavyWorkPrefersHigherPlanTier(t *testing.T) {
	pool := []Entrant{
		planEntrant("pro-cheap", 1650, 1, 3),
		planEntrant("max-seat", 1660, 5, 4),
		planEntrant("unknown", 1650, 1, 0),
	}
	heavy := StoryTask{Duty: DutyCode, Band: 4, Points: 8, Rating: 1500}
	if got := ScheduleStory(heavy, pool, Lines{}); got.Agent != "max-seat" {
		t.Fatalf("heavy code pick = %+v, want max-seat", got)
	}
	manage := StoryTask{Duty: DutyManage, Band: 4, Points: 1, Rating: 1500}
	if got := ScheduleStory(manage, pool, Lines{}); got.Agent != "max-seat" {
		t.Fatalf("manager pick = %+v, want max-seat", got)
	}
	light := StoryTask{Duty: DutyCode, Band: 4, Points: 2, Rating: 1500}
	if got := ScheduleStory(light, pool, Lines{}); got.Agent == "max-seat" {
		t.Fatalf("light work followed plan tier: %+v", got)
	}

	// A max seat that is a clearly worse fit stays out of reach.
	far := []Entrant{planEntrant("pro-fit", 1650, 1, 3), planEntrant("max-far", 2100, 1, 4)}
	if got := ScheduleStory(heavy, far, Lines{}); got.Agent != "pro-fit" {
		t.Fatalf("heavy pick drifted outside the plan window: %+v", got)
	}

	// Unknown tiers are no preference: among equal unknowns, cost decides.
	unknown := []Entrant{planEntrant("dear", 1650, 3, 0), planEntrant("cheap", 1650, 1, 0)}
	if got := ScheduleStory(heavy, unknown, Lines{}); got.Agent != "cheap" {
		t.Fatalf("unknown-tier heavy pick = %+v, want cheap", got)
	}
}

func TestStoryTaskHeavy(t *testing.T) {
	for _, tc := range []struct {
		task StoryTask
		want bool
	}{
		{StoryTask{Duty: DutyCode, Points: 3}, false},
		{StoryTask{Duty: DutyCode, Points: 5}, true},
		{StoryTask{Duty: DutyManage, Points: 1}, true},
		{StoryTask{Duty: DutyJudge, Points: 2}, false},
	} {
		if got := tc.task.Heavy(); got != tc.want {
			t.Errorf("%+v Heavy = %v, want %v", tc.task, got, tc.want)
		}
	}
}

// Currency (L4/L5 agents on L3 code) keeps coding-count fairness first; among
// equal counts a heavy story prefers the higher plan tier over the cheaper seat.
func TestScheduleStoryCurrencyHeavyPrefersPlanTier(t *testing.T) {
	pool := []Entrant{planEntrant("pro", 1700, 1, 3), planEntrant("max", 1700, 5, 4)}
	for i := range pool {
		pool[i].Band = 5
	}
	heavy := StoryTask{Duty: DutyCode, Band: 3, Points: 5, Rating: 1500}
	if got := ScheduleStory(heavy, pool, Lines{}); got.Agent != "max" || got.Reason != "currency" {
		t.Fatalf("heavy currency pick = %+v", got)
	}
	light := StoryTask{Duty: DutyCode, Band: 3, Points: 1, Rating: 1500}
	if got := ScheduleStory(light, pool, Lines{}); got.Agent != "pro" {
		t.Fatalf("light currency pick = %+v", got)
	}
}
