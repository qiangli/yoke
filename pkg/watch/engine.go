package watch

import (
	"context"
	"strings"
	"time"
)

// Exit codes. 0 and 1 are the pair an agent blocks on, and they must never be
// confusable: "the thing I waited for happened" is not "I ran out of time".
const (
	ExitOK            = 0
	ExitTimeout       = 1
	ExitUsage         = 2
	ExitCommandFailed = 8 // classic GNU watch -e
	ExitInterrupted   = 130
)

// DefaultInterval matches classic watch(1).
const DefaultInterval = 2 * time.Second

// Config configures an engine run.
type Config struct {
	Sources []Source
	Rules   []Rule
	Sinks   []Sink
	// Interval is how often every source is polled and every time rule ticked.
	Interval time.Duration
	// Renotify restates a condition that persists. Zero means never.
	Renotify time.Duration
	// Until exits 0 on the first delivered alert — the replacement for a
	// hand-written until-loop.
	Until bool
	// Timeout bounds the whole run and exits ExitTimeout.
	Timeout   time.Duration
	MaxCycles int
	Now       func() time.Time
	Sleep     func(context.Context, time.Duration) bool
	OnError   func(source string, err error)
}

// Engine runs sources into rules into sinks, and owns DELIVERY POLICY —
// transitions only, re-notify, clear-only-what-was-warned. Policy lives here
// and not in the rules so that every rule, including one an embedder adds,
// behaves the same way without implementing anything.
type Engine struct {
	cfg    Config
	fired  map[string]time.Time
	firing map[string]bool
	count  int
}

// New builds an engine.
func New(cfg Config) *Engine {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = SleepContext
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	return &Engine{cfg: cfg, fired: map[string]time.Time{}, firing: map[string]bool{}}
}

// alertID is the identity a transition is measured against. The LEVEL is part
// of it: an escalation from warning to critical is not a repeat of the warning,
// and a dedupe that swallowed it would leave an operator looking at "idle" for
// something that has been dead for an hour.
func alertID(a Alert) string {
	return a.Rule + "|" + a.Locator + "|" + a.Reason + "|" + a.Level
}

// alertGroup is the same identity WITHOUT the level, because a recovery is not
// level-specific: "it came back" clears whichever rung was raised, and nobody
// wants the critical cleared while the warning stays behind.
func alertGroup(a Alert) string {
	return a.Rule + "|" + a.Locator + "|" + a.Reason + "|"
}

// Observe runs the rules over one event and delivers what survives policy.
func (e *Engine) Observe(ev Event) []Alert {
	var raised []Alert
	for _, r := range e.cfg.Rules {
		raised = append(raised, r.On(ev, e.cfg.Now())...)
	}
	return e.deliver(raised)
}

// Raise puts an alert the ENGINE did not produce through the same delivery
// policy as one it did — dedupe, re-notify, clear-what-was-warned.
//
// It exists for the hook alarms: a probe or action script that exited non-zero
// has to be heard about in the alert stream (never silent), and a failing hook
// on a two-second interval must be deduped like any other persisting
// condition rather than printed thirty times a minute.
func (e *Engine) Raise(a Alert) []Alert { return e.deliver([]Alert{a}) }

// Tick runs the time-driven rules and delivers what survives policy.
func (e *Engine) Tick(now time.Time) []Alert {
	var raised []Alert
	for _, r := range e.cfg.Rules {
		raised = append(raised, r.Tick(now)...)
	}
	return e.deliver(raised)
}

func (e *Engine) deliver(alerts []Alert) []Alert {
	var out []Alert
	for _, a := range alerts {
		if a.At.IsZero() {
			a.At = e.cfg.Now()
		}
		id := alertID(a)
		if a.State == StateClear {
			// Never clear what nobody was warned about: "X is fine again" for an
			// X that never alarmed is noise that teaches a reader to ignore the
			// channel.
			group := alertGroup(a)
			cleared := false
			for id := range e.firing {
				if strings.HasPrefix(id, group) {
					delete(e.firing, id)
					delete(e.fired, id)
					cleared = true
				}
			}
			if !cleared {
				continue
			}
		} else {
			if last, seen := e.fired[id]; seen {
				if e.cfg.Renotify <= 0 || a.At.Sub(last) < e.cfg.Renotify {
					continue
				}
			}
			e.firing[id] = true
			e.fired[id] = a.At
		}
		for _, s := range e.cfg.Sinks {
			if err := s.Deliver(a); err != nil && e.cfg.OnError != nil {
				e.cfg.OnError("sink", err)
			}
		}
		e.count++
		out = append(out, a)
	}
	return out
}

// Run is the loop: poll every source, dispatch, tick, sleep.
func (e *Engine) Run(ctx context.Context) int {
	if ctx == nil {
		ctx = context.Background()
	}
	start := e.cfg.Now()
	var expected []string
	var tasks []Task
	for _, src := range e.cfg.Sources {
		if ex, ok := src.(Expecter); ok {
			expected = append(expected, ex.Expected()...)
		}
		// What a source knows about its task — class, quiet window, start
		// time — reaches the rules BEFORE the first cycle, because all three
		// decide whether a locator may be flagged at all.
		if td, ok := src.(TaskDeclarer); ok {
			tasks = append(tasks, td.Tasks()...)
		}
	}
	for _, r := range e.cfg.Rules {
		if a, ok := r.(Armer); ok && len(expected) > 0 {
			a.Arm(expected)
		}
		if d, ok := r.(Declarer); ok {
			for _, t := range tasks {
				d.Declare(t)
			}
		}
		if s, ok := r.(Starter); ok {
			s.Start(start)
		}
	}

	for cycle := 0; e.cfg.MaxCycles <= 0 || cycle < e.cfg.MaxCycles; cycle++ {
		if ctx.Err() != nil {
			return ExitInterrupted
		}
		now := e.cfg.Now()
		for _, src := range e.cfg.Sources {
			evs, err := src.Poll(ctx, now)
			if err != nil {
				// A source that cannot read its store is a fact to report, not a
				// reason to stop watching. The hand-written script exited here,
				// and then watched nothing at all for the rest of the night.
				if e.cfg.OnError != nil {
					e.cfg.OnError(src.Name(), err)
				}
				continue
			}
			for _, ev := range evs {
				if ev.Source == "" {
					ev.Source = src.Name()
				}
				if ev.At.IsZero() {
					ev.At = now
				}
				e.Observe(ev)
				if e.done() {
					return ExitOK
				}
			}
		}
		e.Tick(e.cfg.Now())
		if e.done() {
			return ExitOK
		}
		if e.cfg.Timeout > 0 && e.cfg.Now().Sub(start) >= e.cfg.Timeout {
			return ExitTimeout
		}
		if e.cfg.MaxCycles > 0 && cycle == e.cfg.MaxCycles-1 {
			break
		}
		if !e.cfg.Sleep(ctx, e.cfg.Interval) {
			return ExitInterrupted
		}
	}
	return ExitOK
}

func (e *Engine) done() bool { return e.cfg.Until && e.count > 0 }

// Fired reports how many alerts have been delivered.
func (e *Engine) Fired() int { return e.count }

// SleepContext waits for d, or reports false if the context ended first.
func SleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
