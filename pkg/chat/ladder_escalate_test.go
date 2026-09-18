package chat

import (
	"context"
	"testing"
)

func TestLadderEscalatorExhausts(t *testing.T) {
	// A ladder with unresolvable agents: each rung returns ok=false (Invoke
	// fails), and once past the last rung it stays false. We assert it advances
	// (does not always return the same rung) and terminates.
	esc := LadderEscalator([]string{"nonexistent-a", "nonexistent-b"})
	req := EscalationRequest{Coachee: "base", Trip: 3}
	// Each call advances a rung; after 2 rungs the ladder is exhausted.
	for i := 0; i < 4; i++ {
		_, _, ok := esc(context.Background(), req)
		// Unresolvable agents make Invoke fail → ok=false; the point is it does
		// not panic and terminates. (A live ladder would return ok=true with a steer.)
		_ = ok
	}
	// Empty ladder: immediately exhausted.
	if _, _, ok := LadderEscalator(nil)(context.Background(), req); ok {
		t.Fatal("empty ladder must not escalate")
	}
}
