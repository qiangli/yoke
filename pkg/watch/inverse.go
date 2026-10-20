package watch

import "time"

// Inverse configures the "not ... within T" operator.
type Inverse struct {
	RuleName  string
	Within    time.Duration
	Reason    string
	Activity  Predicate
	LocatorOf func(Event) string
	Expect    []string
	NoClear   bool
	Message   string
}

// InverseRule is the negation of any property over a window.
type InverseRule struct{}

// NewInverse builds the inverse rule.
func NewInverse(c Inverse) *InverseRule { return &InverseRule{} }

// Name implements Rule.
func (r *InverseRule) Name() string { return "" }

// Start arms the locators that must report.
func (r *InverseRule) Start(now time.Time) {}

// On implements Rule.
func (r *InverseRule) On(ev Event, now time.Time) []Alert { return nil }

// Tick implements Rule.
func (r *InverseRule) Tick(now time.Time) []Alert { return nil }
