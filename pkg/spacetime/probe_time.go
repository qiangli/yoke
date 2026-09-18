package spacetime

import (
	"fmt"
	"strings"
	"time"
)

var timeNow = time.Now

func probeTimeHour() (string, error) {
	return fmt.Sprintf("%02d", timeNow().Hour()), nil
}

func probeTimeWeekday() (string, error) {
	return strings.ToLower(timeNow().Weekday().String()), nil
}

func probeTimeZone() (string, error) {
	now := timeNow()
	name := now.Location().String()
	// time.Local commonly stringifies as the generic label "Local". Prefer
	// the OS-resolved abbreviation in that case; it is less geographically
	// specific than an IANA name but still expresses the active timezone.
	if name == "" || name == "Local" {
		name, _ = now.Zone()
	}
	if name == "" {
		return "", ErrNotApplicable
	}
	return normalizeZone(name), nil
}

// attended is a deliberately coarse local-time hint, not a presence claim.
// 08:00–21:59 is likely attended; weekday is separate so policies can compose
// their own work-hours rule without this probe guessing a culture's weekend.
func probeTimeAttended() (string, error) {
	hour := timeNow().Hour()
	if hour >= 8 && hour < 22 {
		return "true", nil
	}
	return "false", nil
}

func normalizeZone(s string) string {
	s = strings.NewReplacer("/", "-", "_", "-", " ", "-").Replace(s)
	return normalize(s)
}
