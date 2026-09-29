package ladder

import (
	"errors"
	"testing"
	"time"
)

func TestValidPointsAndInitialRating(t *testing.T) {
	tests := []struct {
		points Points
		valid  bool
		rating float64
		split  bool
		err    bool
	}{
		{1, true, 1300, false, false},
		{2, true, 1450, false, false},
		{3, true, 1550, false, false},
		{5, true, 1700, false, false},
		{8, true, 1850, false, false},
		{0, false, 0, false, true},
		{4, false, 0, false, true},
		{13, false, 0, true, true},
		{21, false, 0, true, true},
	}
	for _, test := range tests {
		t.Run("points", func(t *testing.T) {
			if got := ValidPoints(test.points); got != test.valid {
				t.Fatalf("ValidPoints(%d) = %t, want %t", test.points, got, test.valid)
			}
			got, err := StoryInitialRating(test.points)
			if errors.Is(err, ErrSplit) != test.split || (err != nil) != test.err || got != test.rating {
				t.Fatalf("StoryInitialRating(%d) = (%v, %v), want rating %v, split %t, error %t", test.points, got, err, test.rating, test.split, test.err)
			}
		})
	}
}

func TestCapsAndWithinCap(t *testing.T) {
	tests := []struct {
		points Points
		cap    Cap
	}{
		{1, Cap{20, 15 * time.Minute}},
		{2, Cap{35, 30 * time.Minute}},
		{3, Cap{50, 45 * time.Minute}},
		{5, Cap{80, 90 * time.Minute}},
		{8, Cap{120, 180 * time.Minute}},
	}
	for _, test := range tests {
		got, ok := CapFor(test.points)
		if !ok || got != test.cap {
			t.Fatalf("CapFor(%d) = (%#v, %t), want (%#v, true)", test.points, got, ok, test.cap)
		}
		if !WithinCap(test.points, test.cap.Turns, test.cap.Wall) {
			t.Fatalf("WithinCap(%d, cap boundary) = false", test.points)
		}
		if WithinCap(test.points, test.cap.Turns+1, test.cap.Wall) || WithinCap(test.points, test.cap.Turns, test.cap.Wall+time.Nanosecond) {
			t.Fatalf("WithinCap(%d) accepted an over-cap value", test.points)
		}
	}
	if _, ok := CapFor(4); ok || WithinCap(4, 0, 0) {
		t.Fatal("invalid points have a cap")
	}
}

func TestOutcomeScoresAndClassification(t *testing.T) {
	tests := []struct {
		name         string
		turns        int
		wall         time.Duration
		accepted     bool
		reworkRounds int
		falseDone    bool
		want         OutcomeKind
		score        float64
	}{
		{"accepted", 20, 15 * time.Minute, true, 0, false, OutcomeAccepted, 1},
		{"over turns", 21, 15 * time.Minute, true, 0, false, OutcomeAcceptedOverCap, .5},
		{"over wall", 20, 15*time.Minute + time.Nanosecond, true, 0, false, OutcomeAcceptedOverCap, .5},
		{"one rework wins over cap", 21, 15 * time.Minute, true, 1, false, OutcomeReworked, .5},
		{"two reworks fails", 20, 15 * time.Minute, true, 2, false, OutcomeFailed, 0},
		{"not accepted", 20, 15 * time.Minute, false, 0, false, OutcomeFailed, 0},
		{"false done wins", 20, 15 * time.Minute, true, 0, true, OutcomeFalseDone, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ClassifyDelivery(1, test.turns, test.wall, test.accepted, test.reworkRounds, test.falseDone)
			if got != test.want || OutcomeScore(got) != test.score {
				t.Fatalf("ClassifyDelivery = %v / score %v, want %v / %v", got, OutcomeScore(got), test.want, test.score)
			}
		})
	}
	if OutcomeScore(OutcomeAbandoned) != 0 {
		t.Fatal("abandoned outcome must score zero")
	}
}

func TestEstimateMissAndPenalty(t *testing.T) {
	tests := []struct {
		name      string
		estimated Points
		turns     int
		wall      time.Duration
		want      int
	}{
		{"one point hit", 1, 20, 15 * time.Minute, 0},
		{"one point just over", 1, 21, 15 * time.Minute, 1},
		{"two point lower-bound turn", 2, 21, 30 * time.Minute, 0},
		{"two point lower-bound wall", 2, 35, 15*time.Minute + time.Nanosecond, 0},
		{"two point fits one", 2, 20, 15 * time.Minute, 1},
		{"five point fits two", 5, 35, 30 * time.Minute, 2},
		{"three point to five", 3, 51, 45 * time.Minute, 1},
		{"eight point beyond", 8, 121, 180 * time.Minute, 1},
		{"one point beyond eight", 1, 121, 180 * time.Minute, 5},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := EstimateMiss(test.estimated, test.turns, test.wall); got != test.want {
				t.Fatalf("EstimateMiss(%d, %d, %s) = %d, want %d", test.estimated, test.turns, test.wall, got, test.want)
			}
		})
	}
	for _, test := range []struct {
		miss int
		want float64
	}{{0, 0}, {1, .5}, {2, 1}, {8, 1}} {
		if got := EstimatePenalty(test.miss); got != test.want {
			t.Fatalf("EstimatePenalty(%d) = %v, want %v", test.miss, got, test.want)
		}
	}
}
