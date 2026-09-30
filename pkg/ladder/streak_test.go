package ladder

import (
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ladder/blame"
)

func TestCurrentBandStreak(t *testing.T) {
	agentFailure := blame.Attribution{Class: blame.ClassAgent, By: "reviewer", Evidence: []blame.Evidence{{Kind: blame.EvidenceGate, Ref: "gate"}}}
	environmentFailure := blame.Attribution{Class: blame.ClassEnvironment, By: "reviewer", Evidence: []blame.Evidence{{Kind: blame.EvidenceHost, Ref: "host"}}}
	specFailure := blame.Attribution{Class: blame.ClassSpec, By: "reviewer", Evidence: []blame.Evidence{{Kind: blame.EvidenceAmbiguity, Ref: "spec", Note: "unclear"}}}
	mk := func(outcome float64, a blame.Attribution) Event {
		return Event{Kind: EventKindDelivery, Agent: "agent-a", Outcome: outcome, Blame: a}
	}
	success := mk(1, blame.Attribution{})
	failure := mk(0, agentFailure)
	many := func(e Event, n int) []Event {
		out := make([]Event, n)
		for i := range out {
			out[i] = e
		}
		return out
	}
	tests := []struct {
		name                string
		seed                int
		events              []Event
		band, streak, moves int
	}{
		{"five successes promote", 3, many(success, 5), 4, 0, 1},
		{"accepted outcome promotes", 3, append(many(success, 4), mk(0.5, blame.Attribution{})), 4, 0, 1},
		{"four do not", 3, many(success, 4), 3, 4, 0},
		{"five agent failures relegate", 3, many(failure, 5), 2, 0, 1},
		{"environment and spec skip", 3, append(append(many(success, 2), mk(0, environmentFailure), mk(0, specFailure)), many(success, 3)...), 4, 0, 1},
		{"unclassified skips", 3, append(append(many(success, 2), mk(0, blame.Attribution{})), many(success, 3)...), 4, 0, 1},
		{"success breaks failure", 3, append(append(many(failure, 4), success), many(failure, 4)...), 3, -4, 0},
		{"upper clamp", 5, many(success, 5), 5, 0, 0},
		{"lower clamp", 1, many(failure, 5), 1, 0, 0},
		{"reset after move", 2, many(success, 10), 4, 0, 2},
		{"unknown seed", 0, many(success, 5), 0, 0, 0},
		{"audit is not input", 3, append(many(success, 4), Event{Kind: EventKindBand, Agent: "agent-a", FromBand: 3, ToBand: 4, Note: "promote"}), 3, 4, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := CurrentBand(tt.seed, tt.events, "agent-a")
			if s.Band != tt.band || s.Streak != tt.streak || len(s.Moves) != tt.moves {
				t.Fatalf("got %+v", s)
			}
		})
	}
	t.Run("superseded delivery", func(t *testing.T) {
		events := many(success, 5)
		for i := range events {
			events[i].ID = string(rune('a' + i))
		}
		events = append(events, Event{Kind: EventKindCorrection, Supersedes: "a", Season: 1})
		s := CurrentBand(3, events, "agent-a")
		if s.Band != 3 || s.Streak != 4 {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("version resets", func(t *testing.T) {
		events := many(success, 6)
		for i := range events {
			events[i].ModelVersion = "v1"
		}
		events[4].ModelVersion = "v2"
		events[5].ModelVersion = "v2"
		s := CurrentBand(3, events, "agent-a")
		if s.Band != 3 || s.Streak != 2 {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("shadow and other agent", func(t *testing.T) {
		events := many(success, 4)
		shadow := success
		shadow.Note = "blind shadow attempt"
		other := success
		other.Agent = "agent-b"
		events = append(events, shadow, other)
		s := CurrentBand(3, events, "agent-a")
		if s.Band != 3 || s.Streak != 4 {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("ordered by time", func(t *testing.T) {
		events := many(success, 5)
		for i := range events {
			events[i].At = time.Unix(int64(10+i), 0)
		}
		f := failure
		f.At = time.Unix(5, 0)
		events = append(events, f)
		s := CurrentBand(3, events, "agent-a")
		if s.Band != 4 {
			t.Fatalf("got %+v", s)
		}
	})
}
