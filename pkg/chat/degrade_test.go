package chat

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// A CAPABILITY THAT QUIETLY DOES NOT WORK IS THE WHOLE PROBLEM.
//
// The registry declares `events_arg:` for ycode, so bashy asks for a turn
// boundary. But ycode emits events from its ONE-SHOT path and not from its TUI —
// and the TUI is what steer_exec launches. So a steerable ycode session asks for a
// reported boundary and receives nothing.
//
// The fallback to the silence heuristic is the correct behaviour. Doing it
// SILENTLY is not: the operator would believe turns were being reported while
// bashy went on counting seconds of quiet.
func TestNoEventsFallsBackAndSAYSSO(t *testing.T) {
	s := &Session{
		Nick:      "Elif",
		done:      make(chan struct{}),
		lastWrite: time.Now(),
		// A path nothing will ever write to — exactly the live situation.
		events: &eventTail{path: filepath.Join(t.TempDir(), "never.ndjson")},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// It must NOT hang forever waiting for a turn.end that is never coming, and it
	// must NOT invent a boundary. It falls through to silence — which, with a
	// short quiet period, returns.
	if err := s.WaitIdle(ctx, 300*time.Millisecond); err != nil {
		t.Fatalf("WaitIdle should fall back to the silence heuristic, got: %v", err)
	}

	// And the turn must come from the SCRAPE, not from a phantom report.
	if got := s.Turn(); got != "" {
		t.Errorf("Turn() = %q — nothing was written, so nothing is the answer", got)
	}
}
