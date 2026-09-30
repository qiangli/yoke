package ladder

import (
	"errors"
	"time"
)

// Points is a story estimate bucket.
type Points int

// ErrSplit reports an estimate too large for one story.
var ErrSplit = errors.New("must be split")

// ValidPoints reports whether p is an available story estimate bucket.
func ValidPoints(p Points) bool {
	switch p {
	case 1, 2, 3, 5, 8:
		return true
	default:
		return false
	}
}

// StoryInitialRating returns the initial rating assigned to a story estimate.
func StoryInitialRating(p Points) (float64, error) {
	switch p {
	case 1:
		return 1300, nil
	case 2:
		return 1450, nil
	case 3:
		return 1550, nil
	case 5:
		return 1700, nil
	case 8:
		return 1850, nil
	default:
		if p >= 13 {
			return 0, ErrSplit
		}
		return 0, errors.New("invalid story points")
	}
}

// Cap bounds the turns and wall time allocated to a story.
type Cap struct {
	Turns int
	Wall  time.Duration
}

// CapFor returns the cap for p. Per the 2026-09-30 owner decision, these caps
// are refit from evidence.
func CapFor(p Points) (Cap, bool) {
	switch p {
	case 1:
		return Cap{Turns: 15, Wall: 5 * time.Minute}, true
	case 2:
		return Cap{Turns: 25, Wall: 8 * time.Minute}, true
	case 3:
		return Cap{Turns: 35, Wall: 12 * time.Minute}, true
	case 5:
		return Cap{Turns: 55, Wall: 20 * time.Minute}, true
	case 8:
		return Cap{Turns: 80, Wall: 30 * time.Minute}, true
	default:
		return Cap{}, false
	}
}

// OverCapFailure reports whether usage exceeds twice the point cap.
// Environment blame is applied by the caller.
func OverCapFailure(points Points, wall time.Duration, turns int) bool {
	cap, ok := CapFor(points)
	return ok && (wall > 2*cap.Wall || turns > 2*cap.Turns)
}

// OutcomeKind classifies a completed story delivery.
type OutcomeKind int

const (
	OutcomeAccepted OutcomeKind = iota
	OutcomeAcceptedOverCap
	OutcomeReworked
	OutcomeFailed
	OutcomeAbandoned
	OutcomeFalseDone
)

// WithinCap reports whether actual usage is within p's inclusive cap.
func WithinCap(p Points, turns int, wall time.Duration) bool {
	limit, ok := CapFor(p)
	return ok && turns <= limit.Turns && wall <= limit.Wall
}

// OutcomeScore returns the rating score assigned to an outcome.
func OutcomeScore(kind OutcomeKind) float64 {
	switch kind {
	case OutcomeAccepted:
		return 1
	case OutcomeAcceptedOverCap, OutcomeReworked:
		return .5
	default:
		return 0
	}
}

// ClassifyDelivery classifies a delivery using the outcome precedence rules.
func ClassifyDelivery(p Points, turns int, wall time.Duration, accepted bool, reworkRounds int, falseDone bool) OutcomeKind {
	switch {
	case falseDone:
		return OutcomeFalseDone
	case !accepted:
		return OutcomeFailed
	case reworkRounds == 1:
		return OutcomeReworked
	case reworkRounds >= 2:
		// The design scores one rework round only; refit with the caps.
		return OutcomeFailed
	case !WithinCap(p, turns, wall):
		return OutcomeAcceptedOverCap
	default:
		return OutcomeAccepted
	}
}

// EstimateMiss returns the number of estimate buckets between estimate and actual usage.
func EstimateMiss(estimated Points, turns int, wall time.Duration) int {
	estimateIndex := storyBucket(estimated)
	if estimateIndex < 0 {
		return 5
	}
	actualIndex := storyActualBucket(turns, wall)
	miss := estimateIndex - actualIndex
	if miss < 0 {
		miss = -miss
	}
	return miss
}

// EstimatePenalty returns the penalty corresponding to a bucket miss.
func EstimatePenalty(miss int) float64 {
	switch {
	case miss <= 0:
		return 0
	case miss == 1:
		return .5
	default:
		return 1
	}
}

func storyBucket(p Points) int {
	switch p {
	case 1:
		return 0
	case 2:
		return 1
	case 3:
		return 2
	case 5:
		return 3
	case 8:
		return 4
	default:
		return -1
	}
}

func storyActualBucket(turns int, wall time.Duration) int {
	for index, p := range []Points{1, 2, 3, 5, 8} {
		if WithinCap(p, turns, wall) {
			return index
		}
	}
	return 5
}
