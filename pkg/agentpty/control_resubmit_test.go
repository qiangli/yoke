package agentpty

import (
	"testing"
	"time"
)

type writeLog struct{ writes []string }

func (w *writeLog) Write(p []byte) (int, error) {
	w.writes = append(w.writes, string(p))
	return len(p), nil
}

// A steer is typed, submitted with Enter, and submitted AGAIN a moment later.
// Muse Code and agy drop an Enter that arrives while they are busy, leaving the
// steer unsent in their input box: agent-bench l4/t3-stop scored 0/3 (Muse) and
// 0 (agy) that way, and 2 when a second Enter followed 3 s later. A bare Enter
// on an empty input box is a no-op in every fleet TUI, so the second one is safe
// when the first was taken.
func TestWritePTYControlLineSubmitsTwice(t *testing.T) {
	oldEnter, oldResubmit := steerEnterDelay, steerResubmitDelay
	t.Cleanup(func() { steerEnterDelay, steerResubmitDelay = oldEnter, oldResubmit })
	steerEnterDelay, steerResubmitDelay = time.Millisecond, time.Millisecond

	var w writeLog
	writePTYControlLine(&w, "STOP. Another lane owns m3-m6.")
	want := []string{"STOP. Another lane owns m3-m6.", "\r", "\r"}
	if len(w.writes) != len(want) {
		t.Fatalf("writes = %q, want %q", w.writes, want)
	}
	for i := range want {
		if w.writes[i] != want[i] {
			t.Fatalf("writes = %q, want %q", w.writes, want)
		}
	}
}

// A verbatim frame is a keystroke the caller chose exactly (ESC, a bare Enter,
// Tab): it is written as-is and never gets an extra Enter.
func TestWritePTYControlLineVerbatimGetsNoResubmit(t *testing.T) {
	oldEnter, oldResubmit := steerEnterDelay, steerResubmitDelay
	t.Cleanup(func() { steerEnterDelay, steerResubmitDelay = oldEnter, oldResubmit })
	steerEnterDelay, steerResubmitDelay = time.Millisecond, time.Millisecond

	var w writeLog
	frame := VerbatimFrame([]byte{0x1b})
	writePTYControlLine(&w, frame[:len(frame)-1]) // the reader strips the newline
	if len(w.writes) != 1 || w.writes[0] != "\x1b" {
		t.Fatalf("writes = %q, want only ESC", w.writes)
	}
}
