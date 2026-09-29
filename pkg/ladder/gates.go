package ladder

import (
	"fmt"
	"sort"
)

// Duty represents a rated agent duty.
type Duty string

const (
	DutyCode   Duty = "code"
	DutyManage Duty = "manage"
	DutyJudge  Duty = "judge"
)

// EstablishedEvents is the minimum number of rated events required for a
// duty standing to be considered established.
const EstablishedEvents = 8

// DutyStanding represents an agent's rating and uncertainty on a duty.
type DutyStanding struct {
	R      float64
	RD     float64
	Events int
}

// Lower returns the conservative lower bound of the rating (R - 2*RD).
func (s DutyStanding) Lower() float64 {
	return s.R - 2*s.RD
}

// Established reports whether the standing has sufficient history (Events >= EstablishedEvents).
func (s DutyStanding) Established() bool {
	return s.Events >= EstablishedEvents
}

// CertKind identifies a certification suite.
type CertKind string

const (
	CertL1      CertKind = "l1"
	CertL2      CertKind = "l2"
	CertL3      CertKind = "l3"
	CertSteer   CertKind = "steer"
	CertManager CertKind = "manager"
	CertReview  CertKind = "review"
	CertJudge   CertKind = "judge"
	CertL5      CertKind = "l5"
)

// Certificate records an agent passing a certification suite for a model version and season.
type Certificate struct {
	Kind         CertKind
	ModelVersion string
	Season       int
}

// CertExpirySeasons is the number of seasons after which a certificate expires.
const CertExpirySeasons = 3

// CertValid reports whether certificate c is valid for modelVersion in season.
// It returns false if c.ModelVersion != modelVersion or season - c.Season >= CertExpirySeasons.
func CertValid(c Certificate, modelVersion string, season int) bool {
	if c.ModelVersion != modelVersion {
		return false
	}
	if season < c.Season || season-c.Season >= CertExpirySeasons {
		return false
	}
	return true
}

// Lines holds the rating thresholds required to pass gate checks.
type Lines struct {
	L3Code   float64
	L4Code   float64
	L4Manage float64
	L5Code   float64
	L5Manage float64
	L5Judge  float64
}

// Profile holds an agent's current duty standings, certificates, model version,
// and optional provisional seat.
type Profile struct {
	Standings    map[Duty]DutyStanding
	Certs        []Certificate
	ModelVersion string
	Provisional  int
}

// GateMiss describes a single condition blocking an agent at a gate.
type GateMiss struct {
	Gate   int
	Reason string
}

// DeriveBand derives the highest band (1..5, or 0 if G1 fails) whose gates all hold.
//
// Invariants:
//   - Strict cumulative progression: Ln requires every gate at levels 1..n.
//   - A missing duty standing fails any gate that requires it.
//   - A zero line (0) means "line not yet fitted" and FAILS closed (not passes).
//     Documented rationale: fleet evidence invariant — no success by absence.
//   - missing lists what blocks band+1 for the derived band.
//   - Profile.Provisional > 0 returns max(derived, Provisional) while missing still
//     reports the real gaps for the derived band.
func DeriveBand(p Profile, lines Lines, season int) (int, []GateMiss) {
	hasCert := func(kind CertKind) bool {
		for _, c := range p.Certs {
			if c.Kind == kind && CertValid(c, p.ModelVersion, season) {
				return true
			}
		}
		return false
	}

	checkDuty := func(duty Duty, gate int, gateLabel string, line float64) *GateMiss {
		if p.Standings == nil {
			return &GateMiss{
				Gate:   gate,
				Reason: fmt.Sprintf("missing %s standing", duty),
			}
		}
		standing, ok := p.Standings[duty]
		if !ok {
			return &GateMiss{
				Gate:   gate,
				Reason: fmt.Sprintf("missing %s standing", duty),
			}
		}
		// A zero (or non-positive) line means "line not yet fitted" and fails closed.
		// Fleet evidence invariant: no success by absence.
		if line <= 0 {
			return &GateMiss{
				Gate:   gate,
				Reason: fmt.Sprintf("%s %s line not yet fitted", gateLabel, duty),
			}
		}
		if standing.Lower() < line {
			return &GateMiss{
				Gate:   gate,
				Reason: fmt.Sprintf("conservative %s %g < %s line %g", duty, standing.Lower(), gateLabel, line),
			}
		}
		return nil
	}

	// G1 = valid CertL1
	var g1Misses []GateMiss
	if !hasCert(CertL1) {
		g1Misses = append(g1Misses, GateMiss{Gate: 1, Reason: "no valid l1 certificate"})
	}
	if len(g1Misses) > 0 {
		band := 0
		if p.Provisional > band {
			band = p.Provisional
		}
		return band, g1Misses
	}

	// G2 = G1 + CertL2
	var g2Misses []GateMiss
	if !hasCert(CertL2) {
		g2Misses = append(g2Misses, GateMiss{Gate: 2, Reason: "no valid l2 certificate"})
	}
	if len(g2Misses) > 0 {
		band := 1
		if p.Provisional > band {
			band = p.Provisional
		}
		return band, g2Misses
	}

	// G3 = G2 + CertL3 + CertSteer + code.Lower() >= L3Code
	var g3Misses []GateMiss
	if !hasCert(CertL3) {
		g3Misses = append(g3Misses, GateMiss{Gate: 3, Reason: "no valid l3 certificate"})
	}
	if !hasCert(CertSteer) {
		g3Misses = append(g3Misses, GateMiss{Gate: 3, Reason: "no valid steer certificate"})
	}
	if m := checkDuty(DutyCode, 3, "L3", lines.L3Code); m != nil {
		g3Misses = append(g3Misses, *m)
	}
	if len(g3Misses) > 0 {
		band := 2
		if p.Provisional > band {
			band = p.Provisional
		}
		return band, g3Misses
	}

	// G4 = G3 + CertManager + CertReview + code.Lower() >= L4Code + manage.Lower() >= L4Manage
	var g4Misses []GateMiss
	if !hasCert(CertManager) {
		g4Misses = append(g4Misses, GateMiss{Gate: 4, Reason: "no valid manager certificate"})
	}
	if !hasCert(CertReview) {
		g4Misses = append(g4Misses, GateMiss{Gate: 4, Reason: "no valid review certificate"})
	}
	if m := checkDuty(DutyCode, 4, "L4", lines.L4Code); m != nil {
		g4Misses = append(g4Misses, *m)
	}
	if m := checkDuty(DutyManage, 4, "L4", lines.L4Manage); m != nil {
		g4Misses = append(g4Misses, *m)
	}
	if len(g4Misses) > 0 {
		band := 3
		if p.Provisional > band {
			band = p.Provisional
		}
		return band, g4Misses
	}

	// G5 = G4 + CertJudge + CertL5 + code.Lower() >= L5Code + manage.Lower() >= L5Manage + judge.Lower() >= L5Judge
	var g5Misses []GateMiss
	if !hasCert(CertJudge) {
		g5Misses = append(g5Misses, GateMiss{Gate: 5, Reason: "no valid judge certificate"})
	}
	if !hasCert(CertL5) {
		g5Misses = append(g5Misses, GateMiss{Gate: 5, Reason: "no valid l5 certificate"})
	}
	if m := checkDuty(DutyCode, 5, "L5", lines.L5Code); m != nil {
		g5Misses = append(g5Misses, *m)
	}
	if m := checkDuty(DutyManage, 5, "L5", lines.L5Manage); m != nil {
		g5Misses = append(g5Misses, *m)
	}
	if m := checkDuty(DutyJudge, 5, "L5", lines.L5Judge); m != nil {
		g5Misses = append(g5Misses, *m)
	}
	if len(g5Misses) > 0 {
		band := 4
		if p.Provisional > band {
			band = p.Provisional
		}
		return band, g5Misses
	}

	band := 5
	if p.Provisional > band {
		band = p.Provisional
	}
	return band, nil
}

// DominanceOK verifies that the reviewer dominates the author on a duty.
// Invariant: Nobody reviews uphill (reviewer.Lower() >= author.R).
func DominanceOK(reviewer, author DutyStanding) bool {
	return reviewer.Lower() >= author.R
}

func medianOf(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2.0
}

// MedianCode computes the median code rating R over established standings only.
// It returns 0 if there are no established standings.
func MedianCode(standings []DutyStanding) float64 {
	var vals []float64
	for _, s := range standings {
		if s.Established() {
			vals = append(vals, s.R)
		}
	}
	if len(vals) == 0 {
		return 0
	}
	return medianOf(vals)
}
