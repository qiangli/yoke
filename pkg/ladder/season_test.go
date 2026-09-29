package ladder

import (
	"testing"
	"time"
)

func TestSeasonOf(t *testing.T) {
	t.Setenv("BASHY_LADDER_SEASON", "")
	epoch := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		at   time.Time
		want int
	}{
		{epoch.Add(-time.Hour), 1}, {epoch, 1}, {epoch.Add(7*24*time.Hour - time.Nanosecond), 1},
		{epoch.Add(7 * 24 * time.Hour), 2}, {epoch.Add(14 * 24 * time.Hour).In(time.FixedZone("offset", -7*3600)), 3},
	} {
		if got := SeasonOf(tc.at); got != tc.want {
			t.Errorf("%s: %d != %d", tc.at, got, tc.want)
		}
	}
	if !SeasonEpoch.Equal(epoch) {
		t.Fatal(SeasonEpoch)
	}
	for _, v := range []string{"0", "-1", "bad"} {
		t.Setenv("BASHY_LADDER_SEASON", v)
		if SeasonOf(epoch) != 1 {
			t.Fatal(v)
		}
	}
	t.Setenv("BASHY_LADDER_SEASON", "17")
	if SeasonOf(epoch) != 17 {
		t.Fatal("override")
	}
}
