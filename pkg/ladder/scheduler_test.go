package ladder

import (
	"fmt"
	"math"
	"testing"
)

func schedulerEntrant(name string, band int, r, rd, cost float64) Entrant {
	return Entrant{Agent: name, Band: band, Free: true, CostPerPoint: cost, Standings: map[Duty]DutyStanding{DutyCode: {R: r, RD: rd}}}
}

func TestScheduleStoryMatchingAndTies(t *testing.T) {
	task := StoryTask{Duty: DutyCode, Band: 3, Rating: 1500}
	pool := []Entrant{schedulerEntrant("poor", 3, 1550, 50, 1), schedulerEntrant("fit", 3, 1650, 50, 2)}
	got := ScheduleStory(task, pool, Lines{})
	if got.Agent != "fit" || got.Reason != "match" || math.Abs(got.Expected-.7) > .08 {
		t.Fatalf("matching pick: %+v", got)
	}
	pool = []Entrant{schedulerEntrant("expensive", 3, 1650, 50, 3), schedulerEntrant("cheap", 3, 1650, 50, 1)}
	if got := ScheduleStory(task, pool, Lines{}); got.Agent != "cheap" {
		t.Fatalf("cost tie: %+v", got)
	}
	pool = []Entrant{schedulerEntrant("narrow", 3, 1650, 30, 1), schedulerEntrant("wide", 3, 1650, 100, 1)}
	if got := ScheduleStory(task, pool, Lines{}); got.Agent != "wide" {
		t.Fatalf("RD tie: %+v", got)
	}
}

func TestScheduleStoryCurrencyAndPlayUp(t *testing.T) {
	task := StoryTask{Duty: DutyCode, Band: 3, Rating: 1500}
	pool := []Entrant{schedulerEntrant("l3", 3, 1650, 50, 1), schedulerEntrant("l4", 4, 1700, 50, 10), schedulerEntrant("l5", 5, 1700, 50, 20)}
	pool[1].CodingStoriesThisSeason = 1
	if got := ScheduleStory(task, pool, Lines{}); got.Agent != "l5" || got.Reason != "currency" {
		t.Fatalf("currency: %+v", got)
	}
	pool[2].CodingStoriesThisSeason = 2
	if got := ScheduleStory(task, pool, Lines{}); got.Agent != "l4" {
		t.Fatalf("currency count: %+v", got)
	}
	pool[1].Free = false
	if got := ScheduleStory(task, pool, Lines{}); got.Agent != "l3" {
		t.Fatalf("busy currency: %+v", got)
	}

	manage := StoryTask{Duty: DutyManage, Band: 4, Rating: 1600}
	near := schedulerEntrant("near", 3, 1500, 50, 1)
	near.Standings[DutyManage] = DutyStanding{R: 1700, RD: 100}
	far := schedulerEntrant("far", 3, 1500, 50, 1)
	far.Standings[DutyManage] = DutyStanding{R: 1800, RD: 100}
	if got := ScheduleStory(manage, []Entrant{near, far}, Lines{L4Manage: 1550}); got.Agent != "far" || got.Reason != "play-up" || !got.ChallengeMatch {
		t.Fatalf("play-up: %+v", got)
	}
	if got := ScheduleStory(manage, []Entrant{near}, Lines{L4Manage: 0}); got.Reason != "wait" || got.Agent != "" {
		t.Fatalf("unfitted line: %+v", got)
	}
	if got := ScheduleStory(manage, []Entrant{near}, Lines{L4Manage: 1601}); got.Reason != "wait" {
		t.Fatalf("outside one RD: %+v", got)
	}
	if got := ScheduleStory(StoryTask{Duty: DutyJudge, Band: 5, Rating: 1600}, []Entrant{near}, Lines{L5Judge: 1500}); got.Reason != "wait" {
		t.Fatalf("two bands down: %+v", got)
	}
}

func TestScheduleStoryCurrencyOnlyForCodingBandAndViableMatch(t *testing.T) {
	for _, band := range []int{4, 5} {
		t.Run(fmt.Sprintf("band-%d", band), func(t *testing.T) {
			task := StoryTask{Duty: DutyCode, Band: band, Rating: 1500}
			currency := schedulerEntrant("currency", band, 1800, 50, 1)
			fit := schedulerEntrant("fit", band, 1650, 50, 2)
			fit.CodingStoriesThisSeason = CurrencyStoriesPerSeason
			if got := ScheduleStory(task, []Entrant{currency, fit}, Lines{}); got.Agent != "fit" || got.Reason != "match" {
				t.Fatalf("higher-band work should use rating fit: %+v", got)
			}
		})
	}
	task := StoryTask{Duty: DutyCode, Band: 3, Rating: 1800}
	hopeless := schedulerEntrant("hopeless", 5, 1000, 50, 1)
	fit := schedulerEntrant("fit", 3, 1900, 50, 2)
	if got := ScheduleStory(task, []Entrant{hopeless, fit}, Lines{}); got.Agent != "fit" || got.Reason != "match" {
		t.Fatalf("currency must skip hopeless mismatch: %+v", got)
	}
}

func TestScheduleReviewer(t *testing.T) {
	author := DutyStanding{R: 1600, RD: 50}
	base := schedulerEntrant("author", 3, 1600, 50, 1)
	base.Vendor = "same"
	weak := schedulerEntrant("weak", 3, 1699, 50, 1)
	weak.Vendor = "other"
	strong := schedulerEntrant("strong", 3, 1700, 50, 2)
	strong.Vendor = "same"
	up := schedulerEntrant("up", 4, 1800, 50, 1)
	up.Vendor = "other"
	if got, ok := ScheduleReviewer(author, 3, "same", []Entrant{base, weak, strong, up}); !ok || got.Agent != "strong" {
		t.Fatalf("same-band dominance: %+v %v", got, ok)
	}
	if got, ok := ScheduleReviewer(author, 3, "same", []Entrant{base, weak, up}); !ok || got.Agent != "up" {
		t.Fatalf("escalation: %+v %v", got, ok)
	}
	if _, ok := ScheduleReviewer(author, 3, "same", []Entrant{base, weak}); ok {
		t.Fatal("review should refuse uphill")
	}
	other := schedulerEntrant("other", 3, 1700, 50, 10)
	other.Vendor = "other"
	if got, ok := ScheduleReviewer(author, 3, "same", []Entrant{base, strong, other}); !ok || got.Agent != "other" {
		t.Fatalf("vendor preference: %+v %v", got, ok)
	}
}

func TestScheduleReviewerUsesAuthorBandWithoutPoolEntry(t *testing.T) {
	author := DutyStanding{R: 1600, RD: 50}
	lower := schedulerEntrant("lower", 2, 1900, 50, 1)
	same := schedulerEntrant("same", 3, 1700, 50, 2)
	upper := schedulerEntrant("upper", 4, 1800, 50, 1)
	if got, ok := ScheduleReviewer(author, 3, "author-vendor", []Entrant{lower, same, upper}); !ok || got.Agent != "same" {
		t.Fatalf("absent author should prefer own band in mixed pool: %+v %v", got, ok)
	}
	if got, ok := ScheduleReviewer(author, 3, "author-vendor", []Entrant{lower, upper}); !ok || got.Agent != "upper" {
		t.Fatalf("absent author should escalate one band: %+v %v", got, ok)
	}
	if got, ok := ScheduleReviewer(author, 3, "author-vendor", []Entrant{lower}); ok || got.Agent != "" {
		t.Fatalf("must never choose below author band: %+v %v", got, ok)
	}
}

func TestSchedulerSampling(t *testing.T) {
	var challenges, heads int
	for i := 0; i < 10000; i++ {
		id := fmt.Sprintf("story-%d", i)
		if SampleChallenge(id, 42) {
			challenges++
		}
		if SampleHeadToHead(id, 42) {
			heads++
		}
		if SampleChallenge(id, 42) != SampleChallenge(id, 42) || SampleHeadToHead(id, 42) != SampleHeadToHead(id, 42) {
			t.Fatal("non-deterministic sampling")
		}
	}
	if challenges < 1300 || challenges > 1700 || heads < 400 || heads > 600 {
		t.Fatalf("rates challenge=%d head=%d", challenges, heads)
	}
}
