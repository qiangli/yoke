package chat

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A REAL TURN BOUNDARY, instead of a guess.
//
// Everywhere else, bashy decides a turn is over by watching for 25 seconds of
// silence. That is wrong in both directions: an agent that pauses to think looks
// finished, and an agent that renders a spinner never does.
func TestWaitTurnEndReturnsTheReportedAnswer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ev.ndjson")
	e := &eventTail{path: path}

	// The file does not exist yet — the agent has not started writing. WaitTurnEnd
	// must TAIL, not read-once: the turn we are waiting for has not happened.
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = os.WriteFile(path, []byte(
			`{"type":"turn.start","data":{"prompt":"hi"}}`+"\n"+
				`{"type":"tool.call","data":{"name":"read_file"}}`+"\n"), 0o600)
		time.Sleep(150 * time.Millisecond)
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		_, _ = f.WriteString(`{"type":"turn.end","data":{"status":"ok","text":"4"}}` + "\n")
		_ = f.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	text, ok, err := e.WaitTurnEnd(ctx)
	if err != nil {
		t.Fatalf("WaitTurnEnd: %v", err)
	}
	if !ok {
		t.Fatal("no turn.end seen")
	}
	if text != "4" {
		t.Errorf("reported text = %q, want %q", text, "4")
	}
}

// NO turn.end IS NOT A TURN THAT ENDED.
//
// If the channel goes quiet without reporting, we know NOTHING. Returning ok=true
// there would invent a boundary out of an absence — the exact bug this whole line
// of work exists to stamp out. The caller falls back to the silence heuristic,
// which is a guess, and is at least an honest one.
func TestNoTurnEndIsNotATurnThatEnded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ev.ndjson")
	if err := os.WriteFile(path, []byte(`{"type":"turn.start","data":{}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()

	_, ok, err := (&eventTail{path: path}).WaitTurnEnd(ctx)
	if ok {
		t.Fatal("reported a turn end that the agent never announced")
	}
	if err == nil {
		t.Error("a timeout must surface as an error, not as a quiet false negative")
	}
}

// A torn line — the writer is mid-write — must not be parsed as half an event,
// and must not be lost either. The offset stays behind it and it is read whole.
func TestATornLineIsReadWholeOnTheNextPass(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ev.ndjson")
	e := &eventTail{path: path}

	// Write a complete event and then HALF of the next one.
	if err := os.WriteFile(path, []byte(
		`{"type":"turn.start","data":{}}`+"\n"+`{"type":"turn.`), 0o600); err != nil {
		t.Fatal(err)
	}
	evs, err := e.drain()
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != EventTurnStart {
		t.Fatalf("first drain = %+v, want just turn.start", evs)
	}

	// Now finish the torn line.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`end","data":{"status":"ok","text":"4"}}` + "\n")
	_ = f.Close()

	evs, err = e.drain()
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != EventTurnEnd {
		t.Fatalf("second drain = %+v, want the completed turn.end", evs)
	}
}
