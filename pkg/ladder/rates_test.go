package ladder

import (
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ladder/blame"
)

// DeliveryRates must agree with Replay event for event: a delivery it calls
// rated moves the agent's code standing, one it calls unrated is counted
// Unrated, and one Replay skips entirely (no story, bad points, no agent) is
// neither.
func TestDeliveryRatesAgreesWithReplay(t *testing.T) {
	agentBlame := blame.Attribution{Class: blame.ClassAgent, By: "reviewer", Evidence: []blame.Evidence{{Kind: blame.EvidenceReview, Ref: "review-1"}}}
	base := Event{At: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC), Season: 1, Kind: EventKindDelivery, Agent: "agent-a", Story: "s1", Points: 3}
	cases := []struct {
		name string
		edit func(*Event)
		want bool
	}{
		{"accepted", func(e *Event) { e.Outcome = 1 }, true},
		{"rework", func(e *Event) { e.Outcome = 0.5 }, true},
		{"failed with agent blame", func(e *Event) { e.Outcome = 0; e.Blame = agentBlame }, true},
		{"failed without blame", func(e *Event) { e.Outcome = 0 }, false},
		{"failed with environment blame", func(e *Event) {
			e.Blame = blame.Attribution{Class: blame.ClassEnvironment, By: "x", Evidence: []blame.Evidence{{Kind: blame.EvidenceHost, Ref: "log"}}}
		}, false},
		{"odd outcome", func(e *Event) { e.Outcome = 0.3 }, false},
		{"no agent", func(e *Event) { e.Outcome = 1; e.Agent = "" }, false},
		{"no story", func(e *Event) { e.Outcome = 1; e.Story = "" }, false},
		{"invalid points", func(e *Event) { e.Outcome = 1; e.Points = 4 }, false},
		{"not a delivery", func(e *Event) { e.Outcome = 1; e.Kind = EventKindEstimate }, false},
	}
	for _, c := range cases {
		e := base
		c.edit(&e)
		if got := DeliveryRates(e); got != c.want {
			t.Errorf("%s: DeliveryRates = %v, want %v", c.name, got, c.want)
		}
		if e.Kind != EventKindDelivery || e.Agent == "" {
			continue
		}
		rep := Replay([]Event{e}, 1)
		a := rep.Agents[e.Agent]
		replayRated := a != nil && a.Standings[DutyCode].Events == 1
		if replayRated != c.want {
			t.Errorf("%s: Replay rated = %v, DeliveryRates = %v", c.name, replayRated, c.want)
		}
	}
}
