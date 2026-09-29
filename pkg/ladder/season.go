package ladder

import (
	"os"
	"strconv"
	"time"
)

// SeasonEpoch is the UTC Monday starting the first shared fleet season.
var SeasonEpoch = time.Date(2026, time.September, 28, 0, 0, 0, 0, time.UTC)

// SeasonOf returns the calendar week since SeasonEpoch, clamped to season 1.
// A positive BASHY_LADDER_SEASON overrides the clock for replay and tests.
func SeasonOf(t time.Time) int {
	if season, err := strconv.Atoi(os.Getenv("BASHY_LADDER_SEASON")); err == nil && season > 0 {
		return season
	}
	if t.Before(SeasonEpoch) {
		return 1
	}
	return 1 + int((t.Unix()-SeasonEpoch.Unix())/(7*24*60*60))
}
