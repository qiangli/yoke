package ladder

import (
	"fmt"
	"math/rand"
)

// PlanStep is one scheduled attempt. Points cap its duration in days.
type PlanStep struct {
	Story       string   `json:"story"`
	Points      int      `json:"points"`
	StoryRating float64  `json:"story_rating"`
	Agent       Rating   `json:"agent"`
	DependsOn   []string `json:"depends_on,omitempty"`
}

// PlanValue averages delivered points within the horizon and elapsed days.
type PlanValue struct {
	ExpectedPoints   float64 `json:"expected_points"`
	ExpectedMakespan float64 `json:"expected_makespan"`
}

const simulateTrials = 4096
const simulateSeed int64 = 3311206

// SimulatePlan runs a reproducible Monte Carlo schedule. A failed attempt
// consumes its slot and unlocks dependents, but does not deliver points.
func SimulatePlan(plan []PlanStep, days, slots int) (PlanValue, error) {
	if days <= 0 || slots <= 0 {
		return PlanValue{}, fmt.Errorf("days and slots must be positive")
	}
	indexes := make(map[string]int, len(plan))
	for i, s := range plan {
		if s.Story == "" || s.Points <= 0 || s.Points > 8 {
			return PlanValue{}, fmt.Errorf("invalid story or point cap at step %d", i)
		}
		if _, ok := indexes[s.Story]; ok {
			return PlanValue{}, fmt.Errorf("duplicate story %q", s.Story)
		}
		indexes[s.Story] = i
	}
	for i, s := range plan {
		for _, dep := range s.DependsOn {
			j, ok := indexes[dep]
			if !ok || j >= i {
				return PlanValue{}, fmt.Errorf("dependency %q must precede %q", dep, s.Story)
			}
		}
	}
	var out PlanValue
	rng := rand.New(rand.NewSource(simulateSeed))
	for trial := 0; trial < simulateTrials; trial++ {
		ends := make([]int, len(plan))
		started := make([]bool, len(plan))
		succeeded := make([]bool, len(plan))
		time, done := 0, 0
		for done < len(plan) {
			active := 0
			for _, end := range ends {
				if end > time {
					active++
				}
			}
			for i, s := range plan {
				if started[i] || active >= slots {
					continue
				}
				ready := true
				for _, dep := range s.DependsOn {
					if !started[indexes[dep]] || ends[indexes[dep]] > time {
						ready = false
						break
					}
				}
				if !ready {
					continue
				}
				started[i] = true
				active++
				ends[i] = time + 1 + rng.Intn(s.Points)
				succeeded[i] = rng.Float64() < Expected(s.Agent, Rating{R: s.StoryRating, RD: 30})
			}
			next := int(^uint(0) >> 1)
			for _, end := range ends {
				if end > time && end < next {
					next = end
				}
			}
			if next == int(^uint(0)>>1) {
				return PlanValue{}, fmt.Errorf("plan cannot advance")
			}
			time = next
			for i, end := range ends {
				if end == time {
					done++
					if succeeded[i] && time <= days {
						out.ExpectedPoints += float64(plan[i].Points)
					}
				}
			}
		}
		out.ExpectedMakespan += float64(time)
	}
	out.ExpectedPoints /= simulateTrials
	out.ExpectedMakespan /= simulateTrials
	return out, nil
}

// ComparePlans returns a minus b under the same fixed simulation seed.
func ComparePlans(a, b []PlanStep, days, slots int) (PlanValue, error) {
	x, err := SimulatePlan(a, days, slots)
	if err != nil {
		return PlanValue{}, err
	}
	y, err := SimulatePlan(b, days, slots)
	if err != nil {
		return PlanValue{}, err
	}
	return PlanValue{ExpectedPoints: x.ExpectedPoints - y.ExpectedPoints, ExpectedMakespan: x.ExpectedMakespan - y.ExpectedMakespan}, nil
}
