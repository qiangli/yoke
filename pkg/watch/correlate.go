package watch

import "time"

// AckLevel is one rung of acknowledgment. Match is a JOIN predicate: it sees
// the candidate event and the trigger it would answer.
type AckLevel struct {
	Name  string
	Match func(ev, trigger Event) bool
}

// Correlation configures request -> response tracking.
type Correlation struct {
	RuleName string
	Trigger  Predicate
	Key      func(Event) string
	Levels   []AckLevel
	Within   time.Duration
}

// CorrelationRule tracks a trigger and the answer it is owed.
type CorrelationRule struct{}

// NewCorrelation builds the correlation rule.
func NewCorrelation(c Correlation) *CorrelationRule { return &CorrelationRule{} }

// Name implements Rule.
func (r *CorrelationRule) Name() string { return "" }

// On implements Rule.
func (r *CorrelationRule) On(ev Event, now time.Time) []Alert { return nil }

// Tick implements Rule.
func (r *CorrelationRule) Tick(now time.Time) []Alert { return nil }
