package acp

import (
	"context"
	"strings"
	"sync"
)

// ToolCallEvent is a recorded tool-call status transition: an announcement or
// update that carried a status (pending/in_progress/completed/failed).
type ToolCallEvent struct {
	// ID is the tool call this event pertains to.
	ID string
	// Title is the human-readable title, when the update carried one.
	Title string
	// Status is the status reported by this event.
	Status ToolCallStatus
}

// RecordingHandler embeds BaseHandler and records what a prompt turn does:
// the agent's streamed text, every tool-call status transition, and — the
// load-bearing capability — the set of files the agent reported touching via
// ToolCall.Locations. This is what lets a conductor hand out a task and learn
// the working set from the agent's own after-the-fact reporting.
//
// It is safe for concurrent use; the SDK delivers session updates from its
// own goroutine.
type RecordingHandler struct {
	BaseHandler

	mu     sync.Mutex
	text   strings.Builder
	paths  []string
	seen   map[string]struct{}
	events []ToolCallEvent
}

// SessionUpdate records the update, then defers to the base (which ignores
// it). It never errors, so a recording handler never fails a turn.
func (h *RecordingHandler) SessionUpdate(_ context.Context, n SessionNotification) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		h.text.WriteString(u.AgentMessageChunk.Text)
	case u.ToolCall != nil:
		h.recordToolCall(u.ToolCall)
	case u.ToolCallUpdate != nil:
		h.recordToolCall(u.ToolCallUpdate)
	}
	return nil
}

// recordToolCall accumulates touched paths and any status transition.
// Caller holds h.mu.
func (h *RecordingHandler) recordToolCall(tc *ToolCall) {
	for _, loc := range tc.Locations {
		if loc.Path == "" {
			continue
		}
		if h.seen == nil {
			h.seen = make(map[string]struct{})
		}
		if _, ok := h.seen[loc.Path]; ok {
			continue
		}
		h.seen[loc.Path] = struct{}{}
		h.paths = append(h.paths, loc.Path)
	}
	if tc.Status != "" {
		h.events = append(h.events, ToolCallEvent{ID: tc.ID, Title: tc.Title, Status: tc.Status})
	}
}

// TouchedPaths returns the distinct file paths the agent reported touching,
// in first-seen order.
func (h *RecordingHandler) TouchedPaths() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.paths) == 0 {
		return nil
	}
	out := make([]string, len(h.paths))
	copy(out, h.paths)
	return out
}

// AgentText returns the concatenation of all streamed agent message chunks.
func (h *RecordingHandler) AgentText() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.text.String()
}

// ToolCallEvents returns the recorded tool-call status transitions in order.
func (h *RecordingHandler) ToolCallEvents() []ToolCallEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.events) == 0 {
		return nil
	}
	out := make([]ToolCallEvent, len(h.events))
	copy(out, h.events)
	return out
}
