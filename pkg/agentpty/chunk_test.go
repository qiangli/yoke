//go:build !windows

package agentpty

import (
	"os"
	"strings"
	"testing"
	"time"
)

// A pty's canonical input queue is ~4096 bytes (MAX_CANON). Write more than that
// in one go and the tail is DROPPED — no error, no short write, nothing.
//
// Measured live: a 4 KB conductor brief vanished entirely into an opencode
// session, while a 40-byte probe on the SAME socket went straight through. The
// session sat at an empty input box, which reads exactly like a model that had
// nothing to say — and very nearly got a model demoted for it.
func TestWritePTYChunkedDeliversEveryByteOfALongPrompt(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Longer than MAX_CANON, which is the whole point.
	prompt := strings.Repeat("conductor brief. ", 500) // ~8.5 KB
	if len(prompt) < 4096 {
		t.Fatalf("test prompt must exceed MAX_CANON, got %d bytes", len(prompt))
	}

	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			_ = r.SetReadDeadline(time.Now().Add(3 * time.Second))
			n, err := r.Read(buf)
			sb.Write(buf[:n])
			if err != nil || sb.Len() >= len(prompt) {
				break
			}
		}
		done <- sb.String()
	}()

	writePTYChunked(w, prompt)
	w.Close()

	got := <-done
	if got != prompt {
		t.Errorf("chunked write lost data: sent %d bytes, terminal received %d", len(prompt), len(got))
	}
}

// A chunk boundary must never split a multi-byte rune: a terminal handed half a
// UTF-8 sequence renders garbage and can drop the rest of the line.
func TestWritePTYChunkedNeverSplitsARune(t *testing.T) {
	r, w, _ := os.Pipe()
	defer r.Close()

	// Multi-byte runes packed so that a naive 512-byte cut lands mid-rune.
	prompt := strings.Repeat("é→漢", 400)

	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			_ = r.SetReadDeadline(time.Now().Add(3 * time.Second))
			n, err := r.Read(buf)
			sb.Write(buf[:n])
			if err != nil || sb.Len() >= len(prompt) {
				break
			}
		}
		done <- sb.String()
	}()

	writePTYChunked(w, prompt)
	w.Close()

	if got := <-done; got != prompt {
		t.Errorf("chunked write corrupted multi-byte text (%d bytes in, %d out)", len(prompt), len(got))
	}
}
