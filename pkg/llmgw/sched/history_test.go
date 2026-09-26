package sched

import (
	"testing"
	"time"
)

func TestHistoryBuffer_RecordAndLookup(t *testing.T) {
	history := NewHistoryBuffer()
	history.Record("k1", "model-a:7b", 2, []string{"coding"}, true)

	rec, ok := history.Lookup("k1")
	if !ok {
		t.Fatal("Record + Lookup should hit")
	}
	if rec.Resolved != "model-a:7b" {
		t.Errorf("Resolved=%q, want model-a:7b", rec.Resolved)
	}
	if rec.Tier != 2 {
		t.Errorf("Tier=%d", rec.Tier)
	}
}

func TestHistoryBuffer_Eviction(t *testing.T) {
	history := NewHistoryBuffer()
	// Overflow the buffer; the oldest entry should fall out.
	// Use a small test by filling to capacity + 5.
	for i := 0; i < HistoryBufferCap+5; i++ {
		history.Record(itoa(i), "m", 2, nil, true)
	}
	if got := history.Len(); got > HistoryBufferCap {
		t.Errorf("size=%d, want <= %d", got, HistoryBufferCap)
	}
	// First few keys should be evicted.
	if _, ok := history.Lookup("0"); ok {
		t.Errorf("oldest entry not evicted")
	}
}

func TestHistoryBuffer_StaleEntryDropped(t *testing.T) {
	history := NewHistoryBuffer()
	history.Record("k-old", "m", 2, nil, true)

	// Force the entry to look ancient.
	history.mu.Lock()
	rec := history.entries["k-old"]
	rec.At = time.Now().Add(-HistoryStaleTTL - time.Hour)
	history.entries["k-old"] = rec
	history.mu.Unlock()

	if _, ok := history.Lookup("k-old"); ok {
		t.Errorf("stale entry should be dropped on Lookup")
	}
}

func TestHistoryBuffer_EmptyKeyOrResolvedSkipped(t *testing.T) {
	history := NewHistoryBuffer()
	history.Record("", "m", 2, nil, true)
	history.Record("k", "", 2, nil, true)
	if history.Len() != 0 {
		t.Errorf("empty key/resolved should be skipped")
	}
}
