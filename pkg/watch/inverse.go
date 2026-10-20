package watch

import (
	"fmt"
	"sort"
	"time"
)

// Inverse configures the "not ... within T" operator — the negation of any
// property over a window, and the reason idle, liveness and absence are ONE
// implementation rather than three rules that drift apart.
//
// It fires when the positive condition has NOT held for Within. What counts as
// the positive condition is Activity; what it is tracked per is LocatorOf.
// Everything else is naming.
type Inverse struct {
	RuleName string
	Within   time.Duration
	// Reason labels the alert: idle (a session, a run, a file), missing (a
	// heartbeat or probe that did not occur), absent (a source or queue that
	// went quiet).
	Reason string
	// Scope selects which events' locators this rule TRACKS AT ALL. nil means
	// every event's.
	//
	// It is separate from Activity because the two questions are different, and
	// conflating them produces either noise or silence. "A sprint thread has
	// gone quiet" must not start a 45-minute timer on every message id the mb
	// source happens to mention (noise); "a session is alive but idle" needs the
	// liveness sample to ARM a clock that only real output may reset (silence
	// otherwise). Scope says what to watch; Activity says what counts as
	// progress on it.
	Scope Predicate
	// Activity selects the events that count as the condition holding. nil
	// means every event does.
	//
	// This is where "alive but idle is stalled, never healthy" lives: a pid
	// check or a coach nudge is not progress, so a rule that wants progress
	// gives a predicate that excludes them, and the clock is never reset by
	// evidence of mere existence.
	Activity Predicate
	// LocatorOf groups activity. nil means the event's own locator.
	LocatorOf func(Event) string
	// Expect arms locators at Start, so a heartbeat that NEVER arrives fires.
	// Without it a rule can only notice silence that FOLLOWED a signal, which
	// is the one case a hand-written watcher already half-handles.
	Expect []string
	// NoClear suppresses the recovery event. Default is to emit it, with the
	// idle duration, because "it came back" is as actionable as "it stopped".
	NoClear bool
	// Message overrides the rendered line.
	Message string
}

// InverseRule is the inverse operator over one Inverse config.
type InverseRule struct {
	cfg    Inverse
	last   map[string]time.Time
	firing map[string]bool
}

// NewInverse builds the inverse rule.
func NewInverse(c Inverse) *InverseRule {
	if c.Reason == "" {
		c.Reason = ReasonIdle
	}
	if c.RuleName == "" {
		c.RuleName = c.Reason
	}
	if c.Within <= 0 {
		c.Within = DefaultIdle
	}
	return &InverseRule{cfg: c, last: map[string]time.Time{}, firing: map[string]bool{}}
}

// DefaultIdle is the operator's documented default window for idleness.
const DefaultIdle = 14 * time.Minute

// Name implements Rule.
func (r *InverseRule) Name() string { return r.cfg.RuleName }

// Arm adds locators a source said it expects to report on.
func (r *InverseRule) Arm(locators []string) {
	r.cfg.Expect = append(r.cfg.Expect, locators...)
}

// Start arms the locators that are expected to report. The window starts now:
// a watcher that has only just begun cannot claim a lease was already stale.
func (r *InverseRule) Start(now time.Time) {
	for _, loc := range r.cfg.Expect {
		if _, known := r.last[loc]; !known {
			r.last[loc] = now
		}
	}
}

// On implements Rule: activity resets the clock, and un-stalls a locator that
// was reported stalled.
func (r *InverseRule) On(ev Event, now time.Time) []Alert {
	loc := ev.Locator
	if r.cfg.LocatorOf != nil {
		loc = r.cfg.LocatorOf(ev)
	}
	if loc == "" {
		return nil
	}
	at := ev.At
	if at.IsZero() {
		at = now
	}
	if r.cfg.Scope != nil && !r.cfg.Scope(ev) {
		return nil
	}
	if r.cfg.Activity != nil && !r.cfg.Activity(ev) {
		// NOT activity — but now we know the locator EXISTS, which is worth
		// recording: "alive but idle is stalled, never healthy" only works if a
		// liveness sample arms the clock it must not reset.
		if _, known := r.last[loc]; !known {
			r.last[loc] = at
		}
		return nil
	}
	prev, known := r.last[loc]
	r.last[loc] = at
	if !r.firing[loc] {
		return nil
	}
	r.firing[loc] = false
	if r.cfg.NoClear {
		return nil
	}
	idle := time.Duration(0)
	if known {
		idle = at.Sub(prev)
	}
	e := ev
	return []Alert{{
		At: at, Rule: r.cfg.RuleName, State: StateClear, Reason: r.cfg.Reason, Locator: loc,
		Message: fmt.Sprintf("%s: activity resumed after %s", loc, idle),
		Fields:  r.fields(idle),
		Event:   &e,
	}}
}

// Tick implements Rule: the half no event can provide.
//
// A persisting condition is restated on every tick rather than reported once.
// Dedupe and re-notify are the ENGINE's policy; a rule that went quiet on its
// own would take that choice away from it and there would be no way to ask for
// a reminder.
func (r *InverseRule) Tick(now time.Time) []Alert {
	locs := make([]string, 0, len(r.last))
	for loc := range r.last {
		locs = append(locs, loc)
	}
	sort.Strings(locs)

	var out []Alert
	for _, loc := range locs {
		idle := now.Sub(r.last[loc])
		if idle < r.cfg.Within {
			continue
		}
		r.firing[loc] = true
		msg := r.cfg.Message
		if msg == "" {
			msg = fmt.Sprintf("%s: stalled — no %s for %s (threshold %s)", loc, r.subject(), idle, r.cfg.Within)
		}
		out = append(out, Alert{
			At: now, Rule: r.cfg.RuleName, State: StateFire, Reason: r.cfg.Reason, Locator: loc,
			Message: msg, Fields: r.fields(idle),
		})
	}
	return out
}

func (r *InverseRule) subject() string {
	switch r.cfg.Reason {
	case ReasonMissing:
		return "heartbeat"
	case ReasonAbsent:
		return "records"
	default:
		return "activity"
	}
}

func (r *InverseRule) fields(idle time.Duration) map[string]any {
	return map[string]any{
		"idle_for":     idle.String(),
		"idle_seconds": idle.Seconds(),
		"within":       r.cfg.Within.String(),
	}
}
