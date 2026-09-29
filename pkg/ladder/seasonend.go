package ladder

import (
	"maps"
	"sort"
)

// HysteresisSeasons is how many seasons a demotion from band n blocks
// re-promotion to n (design §8: "cannot re-promote to n in the next season").
const HysteresisSeasons = 1

// SeasonState is the per-agent ladder state carried from one season to the
// next by SeasonEnd.
type SeasonState struct {
	Agent string
	// Band is the band held this season.
	Band int
	// Demoted maps a band to the season it was lost, for hysteresis.
	Demoted map[int]int
	// ProvisionalSince is the season a provisional seat (or operator peg,
	// which travels the same way) was granted; zero when the band is real.
	ProvisionalSince int
	ModelVersion     string
	// PredecessorOf names the previous model version whose band this agent
	// inherited via VersionBump; while set, SeasonEnd grants one season of
	// grace before demoting on the voided certificates, then clears it.
	PredecessorOf string
}

// BandChange records one band transition decided at season end.
type BandChange struct {
	Agent  string
	From   int
	To     int
	Reason string
}

// SeasonEndOptions parameterizes SeasonEnd.
type SeasonEndOptions struct {
	// Season is the season now ending.
	Season int
	Lines  Lines
	// ProvisionalDeadlineSeasons is how long a provisional seat is kept
	// before the agent drops to its derived band (default 2).
	ProvisionalDeadlineSeasons int
	// PegExpirySeasons is the same deadline for operator pegs (default 2).
	// Pegs are passed as Profile.Provisional, so SeasonEnd cannot tell the
	// two apart; keep the values equal.
	PegExpirySeasons int
}

// SeasonEnd applies the design §8 season-end rules to every agent in states:
// promotion to the derived band (gates hold AND the standings the new gates
// need are established), direct demotion to the highest band whose gates
// hold, one-season hysteresis after a demotion, provisional-seat confirmation
// and expiry, and the one-season version-bump grace. It returns the next
// season's states and the changes made; inputs are not mutated.
func SeasonEnd(states map[string]SeasonState, profiles map[string]Profile, opt SeasonEndOptions) (map[string]SeasonState, []BandChange) {
	next := make(map[string]SeasonState, len(states))
	var changes []BandChange

	names := make([]string, 0, len(states))
	for name := range states {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		st := seasonendCopyState(states[name])
		if st.Agent == "" {
			st.Agent = name
		}
		p := profiles[name]
		derived := seasonendDerivedBand(p, opt)

		switch {
		case p.Provisional > 0:
			st, changes = seasonendProvisional(st, p, derived, opt, changes)
		case st.PredecessorOf != "":
			// The grace covers exactly one season end.
			st.PredecessorOf = ""
			if target, reason := seasonendPromotionTarget(st, p, derived, opt); target > st.Band {
				changes = append(changes, BandChange{Agent: st.Agent, From: st.Band, To: target, Reason: reason})
				st.Band = target
			} else if derived < st.Band {
				changes = append(changes, BandChange{Agent: st.Agent, From: st.Band, To: st.Band, Reason: "version grace"})
			}
		default:
			if target, reason := seasonendPromotionTarget(st, p, derived, opt); target > st.Band {
				changes = append(changes, BandChange{Agent: st.Agent, From: st.Band, To: target, Reason: reason})
				st.Band = target
			} else if derived < st.Band {
				changes = append(changes, BandChange{Agent: st.Agent, From: st.Band, To: derived, Reason: "demotion"})
				if st.Demoted == nil {
					st.Demoted = make(map[int]int)
				}
				st.Demoted[st.Band] = opt.Season
				st.Band = derived
			}
		}
		next[name] = st
	}
	return next, changes
}

// VersionBump derives a new model version's starting state from its
// predecessor (design §8): ratings R are inherited with RD reset to the
// new-player width and event counts kept, and certificates are voided.
// The caller keeps the predecessor's band for one season by setting
// SeasonState.PredecessorOf on the new version's state.
func VersionBump(prev map[Duty]DutyStanding, certs []Certificate) (map[Duty]DutyStanding, []Certificate) {
	next := make(map[Duty]DutyStanding, len(prev))
	for duty, s := range prev {
		next[duty] = DutyStanding{R: s.R, RD: InitialRD, Events: s.Events}
	}
	return next, nil
}

// seasonendProvisional applies the provisional-seat rules: confirm when the
// derived band reaches the seat, expire after the deadline, keep otherwise.
func seasonendProvisional(st SeasonState, p Profile, derived int, opt SeasonEndOptions, changes []BandChange) (SeasonState, []BandChange) {
	if derived >= p.Provisional {
		// The gates confirm the seat; promotion past it still requires
		// established standings.
		target := derived
		for target > p.Provisional && !seasonendEstablishedFor(p, target) {
			target--
		}
		changes = append(changes, BandChange{Agent: st.Agent, From: st.Band, To: target, Reason: "confirmed"})
		st.Band = target
		st.ProvisionalSince = 0
		return st, changes
	}
	deadline := opt.ProvisionalDeadlineSeasons
	if deadline <= 0 {
		deadline = 2
	}
	if opt.Season >= st.ProvisionalSince+deadline {
		changes = append(changes, BandChange{Agent: st.Agent, From: st.Band, To: derived, Reason: "provisional expired"})
		st.Band = derived
		st.ProvisionalSince = 0
		return st, changes
	}
	st.Band = p.Provisional
	return st, changes
}

// seasonendPromotionTarget returns the band the agent may rise to this season
// end: the derived band, lowered until the standings its gates need are all
// established, then capped by hysteresis. Never below the current band.
func seasonendPromotionTarget(st SeasonState, p Profile, derived int, opt SeasonEndOptions) (int, string) {
	target := derived
	for target > st.Band && !seasonendEstablishedFor(p, target) {
		target--
	}
	reason := "promotion"
	for band, lost := range st.Demoted {
		if opt.Season == lost+HysteresisSeasons && target >= band && band-1 < target {
			target = band - 1
			reason = "hysteresis"
		}
	}
	if target < st.Band {
		target = st.Band
	}
	return target, reason
}

// seasonendDerivedBand computes the real derived band, ignoring any
// provisional seat.
func seasonendDerivedBand(p Profile, opt SeasonEndOptions) int {
	p.Provisional = 0
	band, _ := DeriveBand(p, opt.Lines, opt.Season)
	return band
}

// seasonendEstablishedFor reports whether every duty standing the gates up to
// band rely on is established (>= EstablishedEvents rated events).
func seasonendEstablishedFor(p Profile, band int) bool {
	var duties []Duty
	if band >= 3 {
		duties = append(duties, DutyCode)
	}
	if band >= 4 {
		duties = append(duties, DutyManage)
	}
	if band >= 5 {
		duties = append(duties, DutyJudge)
	}
	for _, duty := range duties {
		s, ok := p.Standings[duty]
		if !ok || !s.Established() {
			return false
		}
	}
	return true
}

func seasonendCopyState(st SeasonState) SeasonState {
	st.Demoted = maps.Clone(st.Demoted)
	return st
}
