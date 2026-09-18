package meet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// The transcript is the store of record and appendEvent is its only in-package
// writer (record and recordFull both funnel through it). O_APPEND alone makes
// the offset atomic, not the write: a short write leaves a newline-less partial
// line, the next append concatenates onto it, and readTranscript silently skips
// the merged line — two events vanish with every reader's view consistent.
// Board mode makes concurrent posting the normal case, so this hammers the
// append path and then holds the file to the all-or-nothing bar.
func TestAppendEventConcurrentWritersLoseNothing(t *testing.T) {
	st := newRoom(t)

	// Full mode is the certification load: 50×50 = 2500 events. Each acquisition
	// pays lockfile's holder-record fsync, so the full run takes tens of seconds;
	// -short keeps the same shape at a smoke-test size.
	writers, perWriter := 50, 50
	if testing.Short() {
		writers, perWriter = 10, 10
	}
	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter)
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				ev := Event{
					Round: 1, Speaker: fmt.Sprintf("agent-%02d", w), Role: "participant",
					Kind: "human", Text: fmt.Sprintf("w%02d-m%02d", w, i), TS: fixedNow(),
				}
				if err := appendEvent(st.ID, ev); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("appendEvent under contention: %v", err)
	}

	// Every event parses back: nothing merged, nothing skipped.
	events, err := readTranscript(st.ID)
	if err != nil {
		t.Fatalf("readTranscript: %v", err)
	}
	if len(events) != writers*perWriter {
		t.Fatalf("parsed events = %d, want %d — the difference is messages that VANISHED",
			len(events), writers*perWriter)
	}

	// Every sequence is distinct: no line was written twice, none half-merged
	// into a neighbour that still happened to parse.
	seen := make(map[string]bool, writers*perWriter)
	for _, e := range events {
		if seen[e.Text] {
			t.Fatalf("event %q appears twice", e.Text)
		}
		seen[e.Text] = true
	}
	if len(seen) != writers*perWriter {
		t.Fatalf("distinct events = %d, want %d", len(seen), writers*perWriter)
	}

	// And the RAW file has zero merged lines. readTranscript skips unparseable
	// lines by design, so only the bytes on disk can prove no line was torn.
	dir, err := storeDir(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatal("transcript does not end in a newline — a partial write survived")
	}
	lines := bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n"))
	if len(lines) != writers*perWriter {
		t.Fatalf("raw lines = %d, want %d", len(lines), writers*perWriter)
	}
	for i, line := range lines {
		if !json.Valid(line) {
			t.Fatalf("line %d is not valid JSON (a merged line): %q", i+1, line)
		}
	}
}
