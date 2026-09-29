package ladder

import (
	"encoding/binary"
	"hash/fnv"
	"math"
)

const (
	ChallengeRate            = 0.15
	HeadToHeadRate           = 0.05
	CurrencyStoriesPerSeason = 2
	ScheduleTargetExpected   = 0.7

	// HeavyPoints is the estimate at and above which a story is heavy work.
	HeavyPoints Points = 5
	// PlanPreferenceWindow is how far (in expected score) a heavy or manager
	// pick may drift from the closest match to land on a higher plan tier.
	PlanPreferenceWindow = 0.05
)

// Entrant is an available agent and its current duty standings.
type Entrant struct {
	Agent        string
	Vendor       string
	Band         int
	Standings    map[Duty]DutyStanding
	CostPerPoint float64
	// PlanRank is the normalized subscription tier of the seat the agent bills
	// through (fleet.PlanTierRank: 1 free .. 4 max; 0 unknown). Heavy and manager
	// work prefers the higher tier within PlanPreferenceWindow, because a top
	// plan has the quota to absorb it; it never excludes anyone.
	PlanRank                int
	Free                    bool
	CodingStoriesThisSeason int
}

// StoryTask describes a story to place in its owning band.
type StoryTask struct {
	ID         string
	Duty       Duty
	Band       int
	Points     Points
	Rating     float64
	Author     string
	AuthorCode DutyStanding
}

// Heavy reports whether a task should prefer higher-tier plans: every
// manager task, and any story estimated at HeavyPoints or more.
func (t StoryTask) Heavy() bool { return t.Duty == DutyManage || t.Points >= HeavyPoints }

// Pick records the selected agent and why it was selected.
type Pick struct {
	Agent          string
	Expected       float64
	Reason         string
	ChallengeMatch bool
}

// ScheduleStory assigns a free entrant using currency, rating fit, then play-up.
// Currency gives L4/L5 agents their two coding stories on L3-owned work only;
// it cannot preempt L4/L5-owned work or assign a code matchup below 0.5 expected.
func ScheduleStory(task StoryTask, pool []Entrant, lines Lines) Pick {
	if task.Duty == DutyCode && task.Band == 3 {
		best := -1
		for i := range pool {
			e := &pool[i]
			if !e.Free || (e.Band != 4 && e.Band != 5) || e.CodingStoriesThisSeason >= CurrencyStoriesPerSeason {
				continue
			}
			standing, ok := e.Standings[DutyCode]
			if !ok || schedulerExpected(task, standing) < 0.5 {
				continue
			}
			same := best >= 0 && e.CodingStoriesThisSeason == pool[best].CodingStoriesThisSeason
			if best < 0 || e.CodingStoriesThisSeason < pool[best].CodingStoriesThisSeason ||
				(same && task.Heavy() && e.PlanRank > pool[best].PlanRank) ||
				(same && (!task.Heavy() || e.PlanRank == pool[best].PlanRank) && e.CostPerPoint < pool[best].CostPerPoint) {
				best = i
			}
		}
		if best >= 0 {
			return schedulerPick(task, pool[best], "currency", false)
		}
	}

	best := -1
	bestDistance := math.Inf(1)
	for i := range pool {
		e := &pool[i]
		if !e.Free || e.Band != task.Band {
			continue
		}
		standing, ok := e.Standings[task.Duty]
		if !ok {
			continue
		}
		distance := math.Abs(schedulerExpected(task, standing) - ScheduleTargetExpected)
		if best < 0 || (distance < bestDistance && math.Abs(distance-bestDistance) >= .01) ||
			(math.Abs(distance-bestDistance) < .01 && (e.CostPerPoint < pool[best].CostPerPoint ||
				(e.CostPerPoint == pool[best].CostPerPoint && standing.RD > pool[best].Standings[task.Duty].RD))) {
			best, bestDistance = i, distance
		}
	}
	if best >= 0 && task.Heavy() {
		best = schedulerPlanPreference(task, pool, best, bestDistance)
	}
	if best >= 0 {
		return schedulerPick(task, pool[best], "match", false)
	}

	line := schedulerLine(task.Band, task.Duty, lines)
	if line > 0 {
		best = -1
		for i := range pool {
			e := &pool[i]
			if !e.Free || e.Band != task.Band-1 {
				continue
			}
			standing, ok := e.Standings[task.Duty]
			if !ok || standing.RD < 0 || standing.Lower() < line-standing.RD {
				continue
			}
			if best < 0 || standing.Lower() > pool[best].Standings[task.Duty].Lower() {
				best = i
			}
		}
		if best >= 0 {
			return schedulerPick(task, pool[best], "play-up", true)
		}
	}
	return Pick{Reason: "wait"}
}

// SchedulePick is the scheduling entry point for a single story.
func SchedulePick(task StoryTask, pool []Entrant, lines Lines) Pick {
	return ScheduleStory(task, pool, lines)
}

// ScheduleReviewer selects a free reviewer that dominates the author on code.
// It checks the author's band first, then one band higher, never below it.
func ScheduleReviewer(author DutyStanding, authorBand int, authorVendor string, pool []Entrant) (Pick, bool) {
	for band := authorBand; band <= authorBand+1; band++ {
		best := -1
		for i := range pool {
			e := &pool[i]
			standing, ok := e.Standings[DutyCode]
			if !e.Free || e.Band != band || !ok || !DominanceOK(standing, author) {
				continue
			}
			if best < 0 || (e.Vendor != authorVendor && pool[best].Vendor == authorVendor) ||
				((e.Vendor != authorVendor) == (pool[best].Vendor != authorVendor) && e.CostPerPoint < pool[best].CostPerPoint) {
				best = i
			}
		}
		if best >= 0 {
			return Pick{Agent: pool[best].Agent, Reason: "match"}, true
		}
	}
	return Pick{}, false
}

// schedulerPlanPreference re-picks a heavy task's match among the entrants
// within PlanPreferenceWindow of the closest one: highest plan tier first, then
// the closest match, then cost. Plan tier is data (the fleet's plans), so no
// vendor is named here.
func schedulerPlanPreference(task StoryTask, pool []Entrant, best int, bestDistance float64) int {
	pick, pickDistance := best, bestDistance
	for i := range pool {
		e := &pool[i]
		if !e.Free || e.Band != task.Band {
			continue
		}
		standing, ok := e.Standings[task.Duty]
		if !ok {
			continue
		}
		distance := math.Abs(schedulerExpected(task, standing) - ScheduleTargetExpected)
		if distance > bestDistance+PlanPreferenceWindow {
			continue
		}
		p := &pool[pick]
		switch {
		case e.PlanRank > p.PlanRank:
		case e.PlanRank < p.PlanRank:
			continue
		case distance < pickDistance && math.Abs(distance-pickDistance) >= .01:
		case math.Abs(distance-pickDistance) < .01 && e.CostPerPoint < p.CostPerPoint:
		default:
			continue
		}
		pick, pickDistance = i, distance
	}
	return pick
}

func schedulerExpected(task StoryTask, standing DutyStanding) float64 {
	return Expected(Rating{R: standing.R, RD: standing.RD}, Rating{R: task.Rating, RD: 50})
}

func schedulerPick(task StoryTask, e Entrant, reason string, challenge bool) Pick {
	return Pick{Agent: e.Agent, Expected: schedulerExpected(task, e.Standings[task.Duty]), Reason: reason, ChallengeMatch: challenge}
}

func schedulerLine(band int, duty Duty, lines Lines) float64 {
	switch band {
	case 3:
		if duty == DutyCode {
			return lines.L3Code
		}
	case 4:
		switch duty {
		case DutyCode:
			return lines.L4Code
		case DutyManage:
			return lines.L4Manage
		}
	case 5:
		switch duty {
		case DutyCode:
			return lines.L5Code
		case DutyManage:
			return lines.L5Manage
		case DutyJudge:
			return lines.L5Judge
		}
	}
	return 0
}

// SampleChallenge deterministically selects approximately 15% of stories.
func SampleChallenge(storyID string, seed int64) bool {
	return schedulerSample(storyID, seed, ChallengeRate)
}

// SampleHeadToHead deterministically selects approximately 5% of stories.
func SampleHeadToHead(storyID string, seed int64) bool {
	return schedulerSample(storyID, seed, HeadToHeadRate)
}

func schedulerSample(storyID string, seed int64, rate float64) bool {
	h := fnv.New64a()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(seed))
	_, _ = h.Write(b[:])
	_, _ = h.Write([]byte(storyID))
	// FNV's neighboring input hashes are correlated; avalanche its bits before
	// turning the hash into a sampling interval.
	v := h.Sum64()
	v ^= v >> 33
	v *= 0xff51afd7ed558ccd
	v ^= v >> 33
	v *= 0xc4ceb9fe1a85ec53
	v ^= v >> 33
	return float64(v)/float64(math.MaxUint64) < rate
}
