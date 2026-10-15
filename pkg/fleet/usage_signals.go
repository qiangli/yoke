package fleet

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Usage-signal kinds — what a matched vendor message MEANS for the seat.
//
// The distinction is not cosmetic: an exhausted quota wants the seat SKIPPED
// until its reset, a rate limit wants a short WAIT, and a warning is reported
// without either. See pkg/weave/weave_throttle.go's quota/cooling-down split
// for the operational consequences.
const (
	UsageSignalKindExhausted   = "exhausted"    // a spent quota; retrying is futile until the reset
	UsageSignalKindRateLimited = "rate-limited" // too fast; the same seat succeeds after a wait
	UsageSignalKindWarning     = "warning"      // approaching a limit; reported, never acted on
)

// UsageSignalKinds lists the kind vocabulary.
func UsageSignalKinds() []string {
	return []string{UsageSignalKindExhausted, UsageSignalKindRateLimited, UsageSignalKindWarning}
}

// Named reset layouts. A ResetLayout that is not one of these is a Go
// reference-time layout ("2006-01-02 15:04:05") applied verbatim.
const (
	UsageSignalResetEpoch    = "epoch"    // unix seconds
	UsageSignalResetRFC3339  = "rfc3339"  // an RFC 3339 timestamp
	UsageSignalResetRelative = "relative" // "3h20m" / "6m0s" / "in 45 minutes" / "Retry-After: 120"
	UsageSignalResetClock    = "clock"    // "at 9:45 PM" (next occurrence, local time), or a dated "at Jul 24th, 2026 9:45 PM"
)

// usageSignalWindows is the window vocabulary: the plan windows that name a
// PERIOD (never "concurrent", which is an instantaneous ceiling) plus "".
var usageSignalWindows = []string{"", PlanWindow5h, PlanWindowDay, PlanWindowWeek, PlanWindowMonth}

// UsageSignal is one deterministic vendor-message matcher, as data. The
// vendor's own words about a quota — the error excerpt a CLI prints, or a
// rate-limit response header — are matched by a Go regexp and parsed into a
// ParsedSignal. Rod not fish: vendor message knowledge lives in YAML records,
// never in Go branching.
type UsageSignal struct {
	ID     string `yaml:"id" json:"id" doc:"unique signal id on this record"`
	Match  string `yaml:"match" json:"match" doc:"Go regexp matched against the excerpt (or the header value when header is set); named capture groups feed reset/used_percent"`
	Header string `yaml:"header,omitempty" json:"header,omitempty" doc:"header name read instead of the excerpt, e.g. anthropic-ratelimit-unified-reset (case-insensitive)"`
	Kind   string `yaml:"kind" json:"kind" doc:"exhausted, rate-limited, or warning"`
	Window string `yaml:"window,omitempty" json:"window,omitempty" doc:"quota window the signal reports: 5h, day, week, month, or empty"`

	// UsedPercent is a capture-group name of Match (its captured text is the
	// number) or a literal percentage. Percent, not a fraction.
	UsedPercent string `yaml:"used_percent,omitempty" json:"used_percent,omitempty" doc:"capture group name or literal used percentage"`

	// Reset is a capture-group name of Match holding the vendor's reset
	// spelling, parsed per ResetLayout. ResetLayout is one of epoch, rfc3339,
	// relative, clock — or a Go reference-time layout applied in local time.
	Reset       string `yaml:"reset,omitempty" json:"reset,omitempty" doc:"capture group name holding the reset instant or delay"`
	ResetLayout string `yaml:"reset_layout,omitempty" json:"reset_layout,omitempty" doc:"epoch, rfc3339, relative, clock, or a Go time layout"`
}

// ParsedSignal is what a matched UsageSignal yields. Small and exported on
// purpose: the observation journal (pkg/llmbudget) records these without fleet
// importing it or vice versa; keep the fields stable and add, never rename.
type ParsedSignal struct {
	Signal      string         `json:"signal"`                 // the UsageSignal.ID that matched
	Kind        string         `json:"kind"`                   // exhausted | rate-limited | warning
	Window      string         `json:"window,omitempty"`       // 5h | day | week | month | ""
	UsedPercent *float64       `json:"used_percent,omitempty"` // percent, when the message carried one
	ResetAt     *time.Time     `json:"reset_at,omitempty"`     // when the limit resets; nil when unknown
	RetryAfter  *time.Duration `json:"retry_after,omitempty"`  // set when the reset was a DELAY, never an instant
}

// Reset-time parsers, ported from pkg/weave/weave_throttle.go parseThrottleReset
// (copied, not imported: fleet must not depend on weave). The weave regexes
// anchor on "at …" inside a full message; here the capture group already holds
// just the reset spelling, so a leading "at " is stripped and the shapes anchor
// at the start of the value. Lowercased input, am/pm required on clocks (so a
// URL port never reads as a time), month by its first three letters, ordinal
// suffix and year optional.
var (
	reSignalClock = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?\s*([ap])\.?m\.?`)
	reSignalDate  = regexp.MustCompile(`^(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s*(\d{4})?\s+(\d{1,2})(?::(\d{2}))?\s*([ap])\.?m\.?`)
	// "in 5 minutes" / "in 30 seconds" / "in 2 hours" (+ singular, abbreviated).
	reSignalRelative = regexp.MustCompile(`^(?:in\s+)?(\d+)\s*(second|minute|hour|sec|min|hr)s?\b`)
	// "retry-after: 120" / "retry after 120" (seconds, per RFC 9110).
	reSignalRetryAfter = regexp.MustCompile(`^retry[\s-]*after:?\s+(\d+)`)

	signalMonths = map[string]time.Month{
		"jan": time.January, "feb": time.February, "mar": time.March,
		"apr": time.April, "may": time.May, "jun": time.June,
		"jul": time.July, "aug": time.August, "sep": time.September,
		"oct": time.October, "nov": time.November, "dec": time.December,
	}
)

// MatchSignals evaluates every signal against one observation: an error
// excerpt and/or a response-header map. A signal with Header set matches the
// named header's value (case-insensitive lookup) instead of the excerpt. Each
// match yields one ParsedSignal in declaration order; a declared reset or
// percentage that fails to parse still yields the match with the field nil —
// the vendor's word that the limit hit is the signal, the numbers are
// best-effort. now anchors relative and clock-shaped resets.
func MatchSignals(signals []UsageSignal, excerpt string, headers map[string]string, now time.Time) []ParsedSignal {
	var out []ParsedSignal
	for _, s := range signals {
		re, err := regexp.Compile(s.Match)
		if err != nil {
			continue
		}
		hay := excerpt
		if s.Header != "" {
			hay = headerValue(headers, s.Header)
			if hay == "" {
				continue
			}
		}
		m := re.FindStringSubmatch(hay)
		if m == nil {
			continue
		}
		ps := ParsedSignal{Signal: s.ID, Kind: s.Kind, Window: s.Window}
		if p, ok := captureNumber(re, m, s.UsedPercent); ok {
			ps.UsedPercent = p
		}
		if s.Reset != "" {
			v := groupCapture(re, m, s.Reset)
			if at, after, ok := parseSignalReset(v, s.ResetLayout, now); ok {
				ps.ResetAt, ps.RetryAfter = at, after
			}
		}
		out = append(out, ps)
	}
	return out
}

// headerValue reads one header case-insensitively; "" when absent.
func headerValue(headers map[string]string, name string) string {
	if v, ok := headers[name]; ok {
		return v
	}
	want := strings.ToLower(name)
	for k, v := range headers {
		if strings.ToLower(k) == want {
			return v
		}
	}
	return ""
}

// groupCapture returns the text of one named capture group of the LAST match
// m ("" when absent or the group did not participate).
func groupCapture(re *regexp.Regexp, m []string, name string) string {
	for i, n := range re.SubexpNames() {
		if n == name && i < len(m) {
			return m[i]
		}
	}
	return ""
}

// hasGroupName reports whether the regex declares a named group.
func hasGroupName(re *regexp.Regexp, name string) bool {
	for _, n := range re.SubexpNames() {
		if n == name {
			return true
		}
	}
	return false
}

// captureNumber resolves UsedPercent: a capture-group name wins, else the
// declaration itself is a literal. Trailing % tolerated.
func captureNumber(re *regexp.Regexp, m []string, decl string) (*float64, bool) {
	if decl == "" {
		return nil, false
	}
	v := decl
	if g := groupCapture(re, m, decl); g != "" {
		v = g
	}
	f, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(v), "%"), 64)
	if err != nil {
		return nil, false
	}
	return &f, true
}

// parseSignalReset parses one reset spelling per layout. A relative result
// returns (now+d, &d); an instant returns (t, nil). ok is false when the value
// does not parse — which leaves the ParsedSignal's reset fields unset, not the
// match dropped.
func parseSignalReset(v, layout string, now time.Time) (*time.Time, *time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" || layout == "" {
		return nil, nil, false
	}
	switch layout {
	case UsageSignalResetEpoch:
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			t := time.Unix(secs, 0)
			return &t, nil, true
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			frac := f - math.Trunc(f)
			t := time.Unix(int64(f), int64(frac*float64(time.Second)))
			return &t, nil, true
		}
		return nil, nil, false
	case UsageSignalResetRFC3339:
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, nil, false
		}
		return &t, nil, true
	case UsageSignalResetRelative:
		if d, ok := parseSignalDelay(v); ok {
			at := now.Add(d)
			return &at, &d, true
		}
		return nil, nil, false
	case UsageSignalResetClock:
		if t, ok := parseSignalClock(v, now); ok {
			return &t, nil, true
		}
		return nil, nil, false
	default:
		// A Go reference-time layout, applied in now's location: the vendor's
		// wall-clock spelling ("2026-10-12 17:34:44") read as local time, the
		// same assumption the clock parser makes.
		t, err := time.ParseInLocation(layout, v, now.Location())
		if err != nil {
			return nil, nil, false
		}
		return &t, nil, true
	}
}

// parseSignalDelay handles the delay spellings: a Go duration ("3h20m",
// "6m0s", "37s"), "in N <unit>" (the weave relative shape), "Retry-After: N"
// (seconds, per RFC 9110), and a bare integer (seconds, the Retry-After body).
func parseSignalDelay(v string) (time.Duration, bool) {
	lower := strings.ToLower(strings.TrimSpace(v))
	if lower == "" {
		return 0, false
	}
	if d, err := time.ParseDuration(lower); err == nil && d >= 0 {
		return d, true
	}
	if m := reSignalRetryAfter.FindStringSubmatch(lower); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n >= 0 {
			return time.Duration(n) * time.Second, true
		}
	}
	if m := reSignalRelative.FindStringSubmatch(lower); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n < 0 {
			return 0, false
		}
		switch m[2] {
		case "second", "sec":
			return time.Duration(n) * time.Second, true
		case "minute", "min":
			return time.Duration(n) * time.Minute, true
		case "hour", "hr":
			return time.Duration(n) * time.Hour, true
		}
	}
	if n, err := strconv.Atoi(lower); err == nil && n >= 0 {
		return time.Duration(n) * time.Second, true
	}
	return 0, false
}

// parseSignalClock resolves a wall-clock reset in now's local zone: a dated
// form ("Jul 24th, 2026 9:45 PM"; year optional, rolling forward one when
// already past) wins over the bare clock, which lands today or tomorrow.
// A leading "at " is tolerated so full phrases parse too.
func parseSignalClock(v string, now time.Time) (time.Time, bool) {
	lower := strings.ToLower(strings.TrimSpace(v))
	lower = strings.TrimPrefix(lower, "at ")

	if m := reSignalDate.FindStringSubmatch(lower); m != nil {
		day, derr := strconv.Atoi(m[2])
		hour, herr := strconv.Atoi(m[4])
		if derr != nil || herr != nil || day < 1 || day > 31 || hour < 1 || hour > 12 {
			return time.Time{}, false
		}
		minute := 0
		if m[5] != "" {
			minute, _ = strconv.Atoi(m[5])
		}
		if minute < 0 || minute >= 60 {
			return time.Time{}, false
		}
		month := signalMonths[m[1]]
		hour = signalHour24(hour, m[6])
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

	if m := reSignalClock.FindStringSubmatch(lower); m != nil {
		hour, err := strconv.Atoi(m[1])
		if err != nil || hour < 1 || hour > 12 {
			return time.Time{}, false
		}
		minute := 0
		if m[2] != "" {
			minute, _ = strconv.Atoi(m[2])
		}
		if minute < 0 || minute >= 60 {
			return time.Time{}, false
		}
		reset := time.Date(now.Year(), now.Month(), now.Day(), signalHour24(hour, m[3]), minute, 0, 0, now.Location())
		if !reset.After(now) {
			reset = reset.AddDate(0, 0, 1)
		}
		return reset, true
	}
	return time.Time{}, false
}

// signalHour24 converts a 1-12 hour + "a"/"p" meridiem to 24h clock
// (12 AM == 00:00; 12 PM == 12:00).
func signalHour24(hour int, meridiem string) int {
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

// validateUsageSignals checks a record's signal declarations: ids unique, the
// regex compiles and names its declared groups, kinds and windows are in the
// vocabulary, and a reset names its layout (a layout without a reset, or a
// reset without a layout, is a typo that must not read as data).
func validateUsageSignals(rec string, signals []UsageSignal) error {
	seen := map[string]bool{}
	for _, s := range signals {
		if s.ID == "" || seen[s.ID] || strings.ContainsAny(s.ID, "/\\:\x00") {
			return fmt.Errorf("%s: invalid or duplicate usage signal id %q", rec, s.ID)
		}
		seen[s.ID] = true
		if s.Match == "" {
			return fmt.Errorf("%s: usage signal %q: empty match", rec, s.ID)
		}
		re, err := regexp.Compile(s.Match)
		if err != nil {
			return fmt.Errorf("%s: usage signal %q: bad regexp: %w", rec, s.ID, err)
		}
		switch s.Kind {
		case UsageSignalKindExhausted, UsageSignalKindRateLimited, UsageSignalKindWarning:
		default:
			return fmt.Errorf("%s: usage signal %q: unknown kind %q (want one of %v)", rec, s.ID, s.Kind, UsageSignalKinds())
		}
		if !contains(usageSignalWindows, s.Window) {
			return fmt.Errorf("%s: usage signal %q: unknown window %q", rec, s.ID, s.Window)
		}
		if s.UsedPercent != "" && !hasGroupName(re, s.UsedPercent) {
			if _, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s.UsedPercent), "%"), 64); err != nil {
				return fmt.Errorf("%s: usage signal %q: used_percent %q is neither a capture group nor a number", rec, s.ID, s.UsedPercent)
			}
		}
		if (s.Reset == "") != (s.ResetLayout == "") {
			return fmt.Errorf("%s: usage signal %q: reset and reset_layout must be declared together", rec, s.ID)
		}
		if s.Reset != "" {
			if !hasGroupName(re, s.Reset) {
				return fmt.Errorf("%s: usage signal %q: reset names no capture group of match", rec, s.ID)
			}
			switch s.ResetLayout {
			case UsageSignalResetEpoch, UsageSignalResetRFC3339, UsageSignalResetRelative, UsageSignalResetClock:
			default:
				// A raw Go reference-time layout. Anything non-empty is
				// accepted as one: a mistyped layout never fires at match
				// time (the reset is left unset), which is fail-closed, and
				// no load-time check can tell a constant layout from a typo.
			}
		}
	}
	return nil
}
