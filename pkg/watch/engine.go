package watch

import (
	"context"
	"time"
)

// Exit codes.
const (
	ExitOK            = 0
	ExitTimeout       = 1
	ExitUsage         = 2
	ExitCommandFailed = 8
	ExitInterrupted   = 130
)

// Config configures an engine run.
type Config struct {
	Sources   []Source
	Rules     []Rule
	Sinks     []Sink
	Interval  time.Duration
	Renotify  time.Duration
	Until     bool
	Timeout   time.Duration
	MaxCycles int
	Now       func() time.Time
	Sleep     func(context.Context, time.Duration) bool
	OnError   func(source string, err error)
}

// Engine runs sources into rules into sinks, and owns delivery policy.
type Engine struct{}

// New builds an engine.
func New(cfg Config) *Engine { return &Engine{} }

// Observe runs the rules over one event and delivers what survives policy.
func (e *Engine) Observe(ev Event) []Alert { return nil }

// Tick runs the time-driven rules and delivers what survives policy.
func (e *Engine) Tick(now time.Time) []Alert { return nil }

// Run is the loop: poll every source, dispatch, tick, sleep.
func (e *Engine) Run(ctx context.Context) int { return ExitOK }

// Fired reports how many alerts have been delivered.
func (e *Engine) Fired() int { return 0 }
