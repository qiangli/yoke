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
	// Within is the WARNING threshold: quiet for this long is a warning.
	Within time.Duration
	// Critical is the second rung: quiet for this long is not a slow task, it
	// is a stalled one. Zero takes the default (DefaultStalled when Within is
	// the default, otherwise twice Within — the same ratio either way, so a
	// rule that sets only one rung still gets a ladder rather than a cliff).
	Critical time.Duration
	// NoCritical keeps a single rung, for a condition where "worse" means
	// nothing (a heartbeat either arrived or it did not).
	NoCritical bool
	// Reason labels the alert: idle (a session, a run, a file), missing (a
	// heartbeat or probe that did not occur), absent (a source or queue that
	// went quiet).
	Reason string
	// Class is the task class assumed for a locator that declares none —
	// which is how `--idle` on an explicit resource stays eligible while a
	// locator declared ClassCommand never is. See LongRunning.
	Class string
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
	cfg Inverse
	// last is the clock: when the condition last held for a locator.
	last map[string]time.Time
	// born is when the task BEGAN, which is not when we first heard about it:
	// a back-dated journal entry tells us the task is at least that old, a
	// declared Started tells us exactly, and the watcher's own start is the
	// floor for a locator we armed and never heard from.
	born map[string]time.Time
	// firing records the level currently reported per locator, so a resume
	// clears the rung that was actually raised.
	firing map[string]string
	// quiet, class and started are the per-locator declarations.
	quiet    map[string]time.Duration
	quietErr map[string]string
	class    map[string]string
	started  map[string]time.Time
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
	if c.Critical <= 0 {
		if c.Within == DefaultIdle {
			c.Critical = DefaultStalled
		} else {
			c.Critical = 2 * c.Within
		}
	}
	return &InverseRule{
		cfg:  c,
		last: map[string]time.Time{}, born: map[string]time.Time{},
		firing: map[string]string{}, quiet: map[string]time.Duration{},
		quietErr: map[string]string{}, class: map[string]string{},
		started: map[string]time.Time{},
	}
}

// THE IDLE POLICY, IN QUARTER-HOUR STEPS.
//
// Quarter-hours because the numbers have to be sayable: an operator reads
// "15 minutes quiet, 30 minutes stalled, remind me every 15" once and knows
// the whole policy. The two rungs are a ladder on purpose — a single
// threshold either cries wolf at 30 minutes of normal quiet or notices a dead
// run an hour late, and which of the two it does depends on a number nobody
// can pick correctly for every task.
const (
	// DefaultIdle is the warning threshold: quiet for this long is worth a look.
	DefaultIdle = 15 * time.Minute
	// DefaultStalled is the critical threshold: quiet for this long is a stall.
	DefaultStalled = 30 * time.Minute
	// DefaultRenotify is how often a persisting condition is restated.
	DefaultRenotify = 15 * time.Minute
)

// Name implements Rule.
func (r *InverseRule) Name() string { return r.cfg.RuleName }

// Arm adds locators a source said it expects to report on.
func (r *InverseRule) Arm(locators []string) {
	r.cfg.Expect = append(r.cfg.Expect, locators...)
}

// Declare implements Declarer: what a source knows about its task.
//
// A declared task is also an ARMED one — a source that says "I watch
// run:repo-7" has named the locator, so silence from it is reportable without
// the rule file repeating the name.
func (r *InverseRule) Declare(t Task) {
	if t.Locator == "" {
		return
	}
	r.cfg.Expect = append(r.cfg.Expect, t.Locator)
	if t.Class != "" {
		r.class[t.Locator] = t.Class
	}
	if t.Quiet > 0 {
		r.quiet[t.Locator] = t.Quiet
	}
	if !t.Started.IsZero() {
		// A DECLARED start wins over anything we INFER, and that is the whole
		// point of carrying it: it is the only fact that distinguishes "quiet
		// for an hour" from "one minute old with an hour of backfilled
		// history". It does NOT seed the clock — a watcher that has only just
		// begun cannot claim the task was already stale before it looked.
		r.started[t.Locator] = t.Started
	}
}

// Start arms the locators that are expected to report. The window starts now:
// a watcher that has only just begun cannot claim a lease was already stale.
func (r *InverseRule) Start(now time.Time) {
	for _, loc := range r.cfg.Expect {
		if _, known := r.last[loc]; !known {
			r.last[loc] = now
		}
		r.noteBorn(loc, now)
	}
}

// noteBorn keeps the EARLIEST time we have any evidence the locator existed.
// A back-dated journal entry is evidence the task is at least that old; it is
// never evidence that it is NOT older.
func (r *InverseRule) noteBorn(loc string, at time.Time) {
	if at.IsZero() {
		return
	}
	if prev, known := r.born[loc]; !known || at.Before(prev) {
		r.born[loc] = at
	}
}

// ageFrom is when the task began, as well as we know: declared if declared,
// earliest observed otherwise.
func (r *InverseRule) ageFrom(loc string) (time.Time, bool) {
	if t, ok := r.started[loc]; ok && !t.IsZero() {
		return t, true
	}
	t, ok := r.born[loc]
	return t, ok
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
	r.learn(loc, ev)
	r.noteBorn(loc, at)
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
	level := r.firing[loc]
	if level == "" {
		return nil
	}
	delete(r.firing, loc)
	if r.cfg.NoClear {
		return nil
	}
	idle := time.Duration(0)
	if known {
		idle = at.Sub(prev)
	}
	e := ev
	return []Alert{{
		At: at, Rule: r.cfg.RuleName, State: StateClear, Reason: r.cfg.Reason, Level: level, Locator: loc,
		Message: fmt.Sprintf("%s: activity resumed after %s", loc, idle),
		Fields:  r.fields(loc, idle, r.windowOf(loc), level),
		Event:   &e,
	}}
}

// learn records a task's own declarations off the event. A source that knows
// nothing about tasks says nothing, and the defaults apply.
func (r *InverseRule) learn(loc string, ev Event) {
	if c := ev.Attr(AttrTask); c != "" {
		r.class[loc] = c
	}
	q := ev.Attr(AttrQuiet)
	if q == "" {
		return
	}
	d, err := time.ParseDuration(q)
	if err != nil || d <= 0 {
		// Not silently ignored: the default applies, and the alert says the
		// declaration was unreadable. A window that quietly did nothing is how
		// a watcher ends up measuring something other than what it was told.
		r.quietErr[loc] = q
		return
	}
	delete(r.quietErr, loc)
	r.quiet[loc] = d
}

// windowOf is the warning threshold for a locator: its declared quiet window
// if it has one, the rule's otherwise.
func (r *InverseRule) windowOf(loc string) time.Duration {
	if d, ok := r.quiet[loc]; ok && d > 0 {
		return d
	}
	return r.cfg.Within
}

// criticalOf is the critical threshold. A declared window scales both rungs by
// the rule's own ratio, so declaring a 45m window gives 45m/90m rather than
// collapsing the two rungs onto the same instant.
func (r *InverseRule) criticalOf(loc string) time.Duration {
	if r.cfg.NoCritical {
		return 0
	}
	w := r.windowOf(loc)
	if w == r.cfg.Within {
		return r.cfg.Critical
	}
	if r.cfg.Within <= 0 {
		return 2 * w
	}
	return time.Duration(float64(w) * (float64(r.cfg.Critical) / float64(r.cfg.Within)))
}

// classOf is the locator's task class: declared, or the rule's assumption.
func (r *InverseRule) classOf(loc string) string {
	if c, ok := r.class[loc]; ok && c != "" {
		return c
	}
	return r.cfg.Class
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
		// Idle applies to long-running work only. A short foreground command
		// is not flagged no matter how quiet it is.
		if !LongRunning(r.classOf(loc)) {
			continue
		}
		window := r.windowOf(loc)
		// A task YOUNGER than its window has not had time to be quiet for it.
		if born, known := r.ageFrom(loc); known && now.Sub(born) < window {
			continue
		}
		idle := now.Sub(r.last[loc])
		if idle < window {
			continue
		}
		level := LevelWarning
		if crit := r.criticalOf(loc); crit > 0 && idle >= crit {
			level = LevelCritical
		}
		r.firing[loc] = level
		msg := r.cfg.Message
		if msg == "" {
			msg = r.render(loc, level, idle, window)
		}
		out = append(out, Alert{
			At: now, Rule: r.cfg.RuleName, State: StateFire, Reason: r.cfg.Reason, Level: level,
			Locator: loc, Message: msg, Fields: r.fields(loc, idle, window, level),
		})
	}
	return out
}

// render is the human line. The critical rung says STALLED, because a task
// that is alive and has produced nothing for twice its window is not a slow
// task — and there is deliberately no way to spell "healthy because the pid
// exists".
func (r *InverseRule) render(loc, level string, idle, window time.Duration) string {
	if level == LevelCritical {
		return fmt.Sprintf("%s: stalled — no %s for %s (critical at %s)",
			loc, r.subject(), idle, r.criticalOf(loc))
	}
	return fmt.Sprintf("%s: idle — no %s for %s (warning at %s)", loc, r.subject(), idle, window)
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

func (r *InverseRule) fields(loc string, idle, window time.Duration, level string) map[string]any {
	f := map[string]any{
		"idle_for":     idle.String(),
		"idle_seconds": idle.Seconds(),
		"within":       window.String(),
		"level":        level,
	}
	if crit := r.criticalOf(loc); crit > 0 {
		f["critical"] = crit.String()
	}
	if c := r.classOf(loc); c != "" {
		f["task"] = c
	}
	if bad, ok := r.quietErr[loc]; ok {
		f["quiet_window_error"] = bad
	}
	return f
}
