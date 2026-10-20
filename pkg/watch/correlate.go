package watch

import (
	"fmt"
	"time"
)

// AckLevel is one rung of acknowledgment.
//
// Match is a JOIN predicate: it sees the candidate event AND the trigger it
// would answer, which is what makes the rule generic. "A replied to B" is
// "from == trigger.to && to == trigger.from" — a static predicate cannot say
// that, so a rule built on one ends up hard-coding a pair of names.
type AckLevel struct {
	Name  string
	Match func(ev, trigger Event) bool
}

// Correlation configures request -> response tracking: a trigger event, an
// expected answer, a deadline, and the key that joins them.
//
// The levels are ordered weakest to strongest and ONLY THE LAST ONE SATISFIES
// the rule, because a queued message is never an acknowledgment. For the
// message board that ladder is delivered -> read -> replied, so "unanswered"
// can report how far it got: "they never looked" and "they read it and said
// nothing" are different problems with different follow-ups.
//
// Absence is a special case of this rule (trigger = the last event, expected =
// any event) — which is why the inverse operator and this one are the whole
// time-based vocabulary.
type Correlation struct {
	RuleName string
	Trigger  Predicate
	// Key joins a trigger and its answers. It is evaluated on EVERY event, so
	// the same function must work on both sides of the join.
	Key    func(Event) string
	Levels []AckLevel
	Within time.Duration
}

type pending struct {
	trigger Event
	at      time.Time
	level   int // strongest level index reached; -1 = none
	overdue bool
}

// CorrelationRule tracks each open conversation and the answer it is owed.
type CorrelationRule struct {
	cfg   Correlation
	open  map[string]*pending
	order []string // insertion order, so alerts are deterministic
}

// NewCorrelation builds the correlation rule.
func NewCorrelation(c Correlation) *CorrelationRule {
	if c.RuleName == "" {
		c.RuleName = "correlation"
	}
	if c.Within <= 0 {
		c.Within = DefaultReplyWithin
	}
	return &CorrelationRule{cfg: c, open: map[string]*pending{}}
}

// DefaultReplyWithin is the documented default answer deadline.
const DefaultReplyWithin = 15 * time.Minute

// Name implements Rule.
func (r *CorrelationRule) Name() string { return r.cfg.RuleName }

// On implements Rule.
//
// An OPEN conversation absorbs the event: it advances or settles, and it never
// also opens a new obligation. Without that, two agents answering each other
// would each turn the other's reply into a fresh unanswered request and the
// rule would alarm forever on a healthy exchange.
func (r *CorrelationRule) On(ev Event, now time.Time) []Alert {
	key := ""
	if r.cfg.Key != nil {
		key = r.cfg.Key(ev)
	}
	if key == "" {
		return nil
	}
	if p, ok := r.open[key]; ok {
		return r.advance(key, p, ev, now)
	}
	if r.cfg.Trigger != nil && r.cfg.Trigger(ev) {
		r.openPending(key, ev, now)
	}
	return nil
}

func (r *CorrelationRule) openPending(key string, ev Event, now time.Time) {
	at := ev.At
	if at.IsZero() {
		at = now
	}
	p := &pending{trigger: ev, at: at, level: -1}
	// Seed the level from the trigger itself: a post that is on the board IS
	// delivered. The satisfying level is deliberately excluded — a trigger that
	// appeared to answer itself would close instantly and silently.
	for i := len(r.cfg.Levels) - 2; i >= 0; i-- {
		if m := r.cfg.Levels[i].Match; m != nil && m(ev, ev) {
			p.level = i
			break
		}
	}
	r.open[key] = p
	r.order = append(r.order, key)
}

func (r *CorrelationRule) advance(key string, p *pending, ev Event, now time.Time) []Alert {
	at := ev.At
	if at.IsZero() {
		at = now
	}
	last := len(r.cfg.Levels) - 1
	for i := last; i >= 0; i-- {
		lvl := r.cfg.Levels[i]
		if lvl.Match == nil || !lvl.Match(ev, p.trigger) {
			continue
		}
		if i <= p.level {
			return nil // not progress: a repeat of a rung already reached
		}
		p.level = i
		if i != last {
			return nil // progress, not an answer
		}
		latency := at.Sub(p.at)
		reason := ReasonResponded
		if p.overdue || latency > r.cfg.Within {
			reason = ReasonLate
		}
		delete(r.open, key)
		e := ev
		return []Alert{{
			At: at, Rule: r.cfg.RuleName, State: StateFire, Reason: reason,
			Locator: p.trigger.Locator,
			Message: fmt.Sprintf("%s: %s after %s (%s)", p.trigger.Locator, lvl.Name, latency, reason),
			Fields: map[string]any{
				"level": lvl.Name, "latency": latency.String(), "latency_seconds": latency.Seconds(),
				"within": r.cfg.Within.String(),
			},
			Event: &e,
		}}
	}
	return nil
}

// Tick implements Rule: the deadline is time, so only a tick can report it.
func (r *CorrelationRule) Tick(now time.Time) []Alert {
	var out []Alert
	for _, key := range r.order {
		p := r.open[key]
		if p == nil {
			continue
		}
		waited := now.Sub(p.at)
		if waited < r.cfg.Within {
			continue
		}
		p.overdue = true
		reached := r.levelName(p.level)
		out = append(out, Alert{
			At: now, Rule: r.cfg.RuleName, State: StateFire, Reason: ReasonUnanswered,
			Locator: p.trigger.Locator,
			Message: fmt.Sprintf("%s: no answer after %s (reached %s, deadline %s)",
				p.trigger.Locator, waited, reached, r.cfg.Within),
			Fields: map[string]any{
				"level": reached, "waited": waited.String(), "waited_seconds": waited.Seconds(),
				"within": r.cfg.Within.String(),
			},
			Event: &p.trigger,
		})
	}
	return out
}

func (r *CorrelationRule) levelName(i int) string {
	if i < 0 || i >= len(r.cfg.Levels) {
		return "sent"
	}
	return r.cfg.Levels[i].Name
}
