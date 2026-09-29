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
)

// Entrant is an available agent and its current duty standings.
type Entrant struct {
	Agent                   string
	Vendor                  string
	Band                    int
	Standings               map[Duty]DutyStanding
	CostPerPoint            float64
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

// Pick records the selected agent and why it was selected.
type Pick struct {
	Agent          string
	Expected       float64
	Reason         string
	ChallengeMatch bool
}

// ScheduleStory assigns a free entrant using currency, rating fit, then play-up.
func ScheduleStory(task StoryTask, pool []Entrant, lines Lines) Pick {
	if task.Duty == DutyCode {
		best := -1
		for i := range pool {
			e := &pool[i]
			if !e.Free || (e.Band != 4 && e.Band != 5) || e.CodingStoriesThisSeason >= CurrencyStoriesPerSeason {
				continue
			}
			if _, ok := e.Standings[DutyCode]; !ok {
				continue
			}
			if best < 0 || e.CodingStoriesThisSeason < pool[best].CodingStoriesThisSeason ||
				(e.CodingStoriesThisSeason == pool[best].CodingStoriesThisSeason && e.CostPerPoint < pool[best].CostPerPoint) {
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
// If the author is in pool, that entrant establishes the author's band.
func ScheduleReviewer(author DutyStanding, authorVendor string, pool []Entrant) (Pick, bool) {
	authorBand := 0
	for _, e := range pool {
		if e.Vendor == authorVendor && e.Standings[DutyCode] == author {
			authorBand = e.Band
			break
		}
	}
	if authorBand == 0 {
		for _, e := range pool {
			if e.Band > 0 && (authorBand == 0 || e.Band < authorBand) {
				authorBand = e.Band
			}
		}
	}
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
