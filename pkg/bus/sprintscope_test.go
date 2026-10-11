package bus

import (
	"testing"
	"time"
)

func TestSprintScopeSelectsRelevantMessagesWithoutLooseDigitMatches(t *testing.T) {
	scope := SprintScope{
		ID: 419, UUID: "11111111-2222-4333-8444-555555555555",
		RoomRef: "meet-room-uuid", ContactTopic: "meet:sprint-419",
		StoryIDs: []string{"a1b2c3d4e5f6"}, StoryNumbers: []int{73},
		Since: time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC),
	}
	tests := []struct {
		name string
		msg  SprintMessage
		want bool
	}{
		{"conductor recipient", SprintMessage{To: "conductor:419"}, true},
		{"sprint topic", SprintMessage{Topic: "conductor.419"}, true},
		{"meet room", SprintMessage{Room: "meet-room-uuid"}, true},
		{"meet topic", SprintMessage{Topic: "meet:sprint-419"}, true},
		{"uuid", SprintMessage{Body: "coordination " + scope.UUID}, true},
		{"story id", SprintMessage{Body: "Story-ID: a1b2c3d4e5f6"}, true},
		{"story number", SprintMessage{Body: "Story: #73"}, true},
		{"sprint form", SprintMessage{Body: "Sprint #419 is active"}, true},
		{"short form", SprintMessage{Body: "S419 handoff"}, true},
		{"digit in timestamp", SprintMessage{TS: "2026-10-11T10:19:00Z"}, false},
		{"id contains digits", SprintMessage{Body: "run-4198"}, false},
		{"title keywords only", SprintMessage{Body: "inbox sprint manager backlog"}, false},
		{"wrong sprint", SprintMessage{Body: "Sprint 418"}, false},
		{"before start", SprintMessage{TS: "2026-10-11T09:59:59Z", To: "conductor:419"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scope.Relevant(tt.msg); got != tt.want {
				t.Errorf("Relevant(%+v) = %t, want %t", tt.msg, got, tt.want)
			}
		})
	}
}
