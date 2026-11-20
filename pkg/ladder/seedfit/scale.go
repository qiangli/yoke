package seedfit

import (
	"fmt"
	"math"
	"regexp"
)

// A metric may declare how a non-proportion score maps onto [0,1], so Elo
// ratings, dollar balances and bounded indexes are fitted instead of skipped:
//
//	[range LO..HI]   linear: (score − LO) / (HI − LO)
//	[elo anchor A]   logistic: 1 / (1 + 10^((A − score)/400))
//
// The mapping is fixed by the matrix row, never by the models that happen to
// be present, and the fit's per-item discrimination and difficulty absorb
// its scale. Metrics without a declaration parse as before.
var (
	declaredRange = regexp.MustCompile(`\[range\s+([-+]?[0-9.]+)\s*\.\.\s*([-+]?[0-9.]+)\]`)
	declaredElo   = regexp.MustCompile(`\[elo\s+anchor\s+([-+]?[0-9.]+)\]`)
	plainNumber   = regexp.MustCompile(`^\$?\s*([-+]?[0-9][0-9,]*(?:\.[0-9]+)?)\s*(?:\(\s*±\s*[0-9.,$]+\s*\)|±\s*[$0-9.,]+)?$`)
)

// parseDeclaredScale handles a metric carrying a scale declaration. ok is
// false when the metric declares none.
func parseDeclaredScale(metric, score string) (v float64, reason string, ok bool) {
	var conv func(float64) (float64, bool)
	if sm := declaredRange.FindStringSubmatch(metric); sm != nil {
		var lo, hi float64
		fmt.Sscanf(sm[1], "%g", &lo)
		fmt.Sscanf(sm[2], "%g", &hi)
		if hi <= lo {
			return 0, SkipOutOfRange, true
		}
		conv = func(x float64) (float64, bool) {
			if x < lo || x > hi {
				return 0, false
			}
			return (x - lo) / (hi - lo), true
		}
	} else if sm := declaredElo.FindStringSubmatch(metric); sm != nil {
		var anchor float64
		fmt.Sscanf(sm[1], "%g", &anchor)
		conv = func(x float64) (float64, bool) {
			return 1 / (1 + math.Pow(10, (anchor-x)/400)), true
		}
	} else {
		return 0, "", false
	}
	sm := plainNumber.FindStringSubmatch(score)
	if sm == nil {
		return 0, SkipUnparseable, true
	}
	var x float64
	clean := regexp.MustCompile(`,`).ReplaceAllString(sm[1], "")
	if _, err := fmt.Sscanf(clean, "%g", &x); err != nil {
		return 0, SkipUnparseable, true
	}
	y, inRange := conv(x)
	if !inRange {
		return 0, SkipOutOfRange, true
	}
	return math.Min(ScoreClampHi, math.Max(ScoreClampLo, y)), "", true
}
