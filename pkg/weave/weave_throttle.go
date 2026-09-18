package weave

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const weaveThrottleTailBytes = 16 * 1024

// These are substring heuristics for provider/subscription throttles. Many
// subscription-billed CLIs report rolling-window caps in terminal text instead
// of surfacing a clean HTTP status.
var weaveThrottleGenericSignals = []string{
	"usage limit",
	"rate limit",
	"rate_limit",
	"rate-limited",
	"quota",
	"too many requests",
	"429",
	"you've reached your",
	"weekly limit",
	"daily limit",
	"limit reached",
	"try again later",
	"overloaded",
	"insufficient_quota",
	"resource_exhausted",
}

var weaveThrottleToolSignals = map[string][]string{}

// Cooldown causes, recorded alongside the reset instant so `weave fleet` can
// say WHY a member is unassignable, not just until when.
const (
	weaveCooldownQuota = "quota-exhausted" // a spent usage cap / credits — resets on the provider's schedule
	weaveCooldownRate  = "cooling-down"    // a transient rate limit / overload
)

// weaveThrottleCause classifies a matched throttle signal for the cooldown
// record: a spent quota (usage cap, credits) is QUOTA-EXHAUSTED, distinct from
// a transient rate-limit COOLING-DOWN.
func weaveThrottleCause(signal string) string {
	s := strings.ToLower(signal)
	for _, q := range []string{"usage limit", "quota", "weekly limit", "daily limit",
		"limit reached", "you've reached your", "credit", "exhausted"} {
		if strings.Contains(s, q) {
			return weaveCooldownQuota
		}
	}
	return weaveCooldownRate
}

// weaveClassifyThrottle reports whether a worker's termination looks like a
// provider/subscription throttle (vs a normal finish or a crash), and the
// matched signal string for the audit trail. tool is the argv[0] basename
// (claude/codex/opencode/aider/gemini/bash/...); exitCode is the worker's
// exit; logTail is the last chunk of its PTY log.
func weaveClassifyThrottle(tool string, exitCode int, logTail string) (throttled bool, signal string) {
	_ = exitCode // A phrase match is required; bare non-zero exits are crashes.
	if logTail == "" {
		return false, ""
	}
	haystack := strings.ToLower(weaveThrottleTail(logTail, weaveThrottleTailBytes))
	for _, phrase := range weaveThrottleGenericSignals {
		if strings.Contains(haystack, phrase) {
			return true, phrase
		}
	}
	tool = filepath.Base(strings.ToLower(strings.TrimSpace(tool)))
	for _, phrase := range weaveThrottleToolSignals[tool] {
		if strings.Contains(haystack, strings.ToLower(phrase)) {
			return true, phrase
		}
	}
	return false, ""
}

// Reset-time extractors. Subscription-billed CLIs phrase the "try again"
// time as a wall-clock time ("at 11:41 AM"), a relative duration ("in 5
// minutes"), or a Retry-After seconds value. We parse the first match of
// each shape against the (lowercased) message and return the soonest
// usable instant. Stdlib-only — no new dependency.
var (
	// "at 11:41 am" / "at 11:41 pm" / "at 11 am" (am/pm required so we
	// don't swallow unrelated colon-separated numbers like a URL port).
	reThrottleClock = regexp.MustCompile(`\bat\s+(\d{1,2})(?::(\d{2}))?\s*([ap])\.?m\.?`)
	// "at Jul 24th, 2026 9:45 PM" — a DATED reset (codex phrases multi-day
	// quota resets this way; the bare-clock form above cannot see it, so a
	// 3-day quota outage used to record no cooldown at all). Month by its
	// first three letters, ordinal suffix and year optional.
	reThrottleDate = regexp.MustCompile(`\bat\s+(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s*(\d{4})?\s+(\d{1,2})(?::(\d{2}))?\s*([ap])\.?m\.?`)

	throttleMonths = map[string]time.Month{
		"jan": time.January, "feb": time.February, "mar": time.March,
		"apr": time.April, "may": time.May, "jun": time.June,
		"jul": time.July, "aug": time.August, "sep": time.September,
		"oct": time.October, "nov": time.November, "dec": time.December,
	}
	// "in 5 minutes" / "in 30 seconds" / "in 2 hours" (+ singular forms).
	reThrottleRelative = regexp.MustCompile(`\bin\s+(\d+)\s*(second|minute|hour|sec|min|hr)s?\b`)
	// "retry-after: 120" / "retry after 120" (seconds, per RFC 9110).
	reThrottleRetryAfter = regexp.MustCompile(`retry[\s-]*after:?\s+(\d+)`)
)

// parseThrottleReset extracts a "next available" instant from a throttle
// message, relative to now. It handles wall-clock times ("try again at
// 11:41 AM" → today, or tomorrow if already past), relative durations
// ("in N seconds|minutes|hours"), and "Retry-After: N" (seconds). It is
// tolerant of surrounding text and case. Returns (zero, false) when no
// reset is parseable. now's location is used for wall-clock resolution.
func parseThrottleReset(msg string, now time.Time) (time.Time, bool) {
	lower := strings.ToLower(msg)

	// Retry-After (seconds) — most explicit, check first.
	if m := reThrottleRetryAfter.FindStringSubmatch(lower); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n >= 0 {
			return now.Add(time.Duration(n) * time.Second), true
		}
	}

	// Relative duration ("in N <unit>").
	if m := reThrottleRelative.FindStringSubmatch(lower); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n >= 0 {
			var unit time.Duration
			switch m[2] {
			case "second", "sec":
				unit = time.Second
			case "minute", "min":
				unit = time.Minute
			case "hour", "hr":
				unit = time.Hour
			}
			if unit != 0 {
				return now.Add(time.Duration(n) * unit), true
			}
		}
	}

	// Dated wall-clock ("at Jul 24th, 2026 9:45 PM") in now's local zone.
	// Checked before the bare-clock form: it names the day, so it must win
	// over a today/tomorrow guess. A dateless year resolves to now's year,
	// rolling forward one if that instant is already past.
	if m := reThrottleDate.FindStringSubmatch(lower); m != nil {
		day, derr := strconv.Atoi(m[2])
		hour, herr := strconv.Atoi(m[4])
		if derr == nil && herr == nil && day >= 1 && day <= 31 && hour >= 1 && hour <= 12 {
			minute := 0
			if m[5] != "" {
				minute, _ = strconv.Atoi(m[5])
			}
			if minute >= 0 && minute < 60 {
				month := throttleMonths[m[1]]
				hour = throttleHour24(hour, m[6])
				if m[3] != "" {
					year, _ := strconv.Atoi(m[3])
					return time.Date(year, month, day, hour, minute, 0, 0, now.Location()), true
				}
				reset := time.Date(now.Year(), month, day, hour, minute, 0, 0, now.Location())
				if !reset.After(now) {
					reset = reset.AddDate(1, 0, 0)
				}
				return reset, true
			}
		}
	}

	// Wall-clock ("at HH[:MM] am|pm") in now's local zone. Resolve to
	// today; if that instant is already in the past, roll to tomorrow.
	if m := reThrottleClock.FindStringSubmatch(lower); m != nil {
		hour, err := strconv.Atoi(m[1])
		if err == nil && hour >= 1 && hour <= 12 {
			minute := 0
			if m[2] != "" {
				minute, _ = strconv.Atoi(m[2])
			}
			if minute >= 0 && minute < 60 {
				reset := time.Date(now.Year(), now.Month(), now.Day(), throttleHour24(hour, m[3]), minute, 0, 0, now.Location())
				if !reset.After(now) {
					reset = reset.AddDate(0, 0, 1)
				}
				return reset, true
			}
		}
	}

	return time.Time{}, false
}

// throttleHour24 converts a 1-12 hour + "a"/"p" meridiem to 24h clock
// (12 AM == 00:00; 12 PM == 12:00).
func throttleHour24(hour int, meridiem string) int {
	switch meridiem {
	case "a":
		if hour == 12 {
			return 0
		}
	case "p":
		if hour != 12 {
			return hour + 12
		}
	}
	return hour
}

func weaveThrottleTail(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	return s[len(s)-maxBytes:]
}

func weaveReadThrottleLogTail(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	start := int64(0)
	if st.Size() > weaveThrottleTailBytes {
		start = st.Size() - weaveThrottleTailBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(f, weaveThrottleTailBytes))
	if err != nil {
		return ""
	}
	return string(b)
}
