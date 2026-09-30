package ladder

import (
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/ladder/blame"
)

// BandMove is a band change derived from five qualifying deliveries.
type BandMove struct {
	At       time.Time
	From, To int
	Reason   string
}

// BandState is the current band and signed progress toward the next move.
type BandState struct {
	Agent  string
	Seed   int
	Band   int
	Streak int
	Moves  []BandMove
}

// CurrentBand recomputes an agent's band from active delivery events. A zero
// seed means the model is unknown: band stays zero and no deliveries can move it.
func CurrentBand(seed int, events []Event, agent string) BandState {
	state := BandState{Agent: agent, Seed: seed, Band: seed}
	if seed == 0 {
		return state
	}
	if state.Band < 1 {
		state.Band = 1
	}
	if state.Band > 5 {
		state.Band = 5
	}
	ordered := append([]Event(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].At.Before(ordered[j].At) })
	dropped := make(map[string]bool)
	for _, e := range ordered {
		if e.Kind == EventKindCorrection && e.Supersedes != "" {
			dropped[e.Supersedes] = true
		}
	}
	version := ""
	for _, e := range ordered {
		if e.Kind != EventKindDelivery || e.Agent != agent || dropped[e.ID] || strings.Contains(strings.ToLower(e.Note), "shadow") {
			continue
		}
		if e.ModelVersion != "" {
			if version != "" && version != e.ModelVersion {
				state.Streak = 0
			}
			version = e.ModelVersion
		}
		direction := 0
		switch {
		case e.Outcome == 1 || e.Outcome == 0.5:
			direction = 1
		case e.Outcome == 0 && blame.Consequence(e.Blame) == blame.ActionRate:
			direction = -1
		default:
			continue
		}
		if direction > 0 {
			if state.Streak < 0 {
				state.Streak = 0
			}
			state.Streak++
		} else {
			if state.Streak > 0 {
				state.Streak = 0
			}
			state.Streak--
		}
		if state.Streak == 5 || state.Streak == -5 {
			from := state.Band
			to := from + direction
			if to >= 1 && to <= 5 {
				state.Band = to
				reason := "promote"
				if direction < 0 {
					reason = "relegate"
				}
				state.Moves = append(state.Moves, BandMove{At: e.At, From: from, To: to, Reason: reason})
			}
			state.Streak = 0
		}
	}
	return state
}
