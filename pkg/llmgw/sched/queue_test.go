package sched

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAdmitter_SnapshotForPrincipal(t *testing.T) {
	a := resetAdmitter(t)
	// Two principals with different in-flight counts.
	_ = a.TryAdmit("tk-a", 10, "")
	_ = a.TryAdmit("tk-a", 10, "")
	_ = a.TryAdmit("tk-b", 10, "")

	snap := a.SnapshotForPrincipal("tk-a")
	if snap.Position != 2 {
		t.Errorf("Position=%d, want 2", snap.Position)
	}
	if snap.Depth != 3 {
		t.Errorf("Depth=%d, want 3", snap.Depth)
	}
	snap = a.SnapshotForPrincipal("tk-b")
	if snap.Position != 1 {
		t.Errorf("Position=%d, want 1", snap.Position)
	}
}

func TestStampQueueHeaders(t *testing.T) {
	h := http.Header{}
	StampQueueHeaders(h, QueueSnapshot{Position: 3, Depth: 17}, 1800)
	for _, want := range []struct {
		k, v string
	}{
		{QueuePositionHeader, "3"},
		{QueueDepthHeader, "17"},
		{QueueEstimatedWaitMsHdr, "1800"},
	} {
		if got := h.Get(want.k); got != want.v {
			t.Errorf("%s=%q, want %q", want.k, got, want.v)
		}
	}
}

func TestQueueStatusSSEBytes_BelowThresholdEmitsNothing(t *testing.T) {
	got := QueueStatusSSEBytes(QueueSnapshot{Position: 1, Depth: 1}, 100*time.Millisecond, "abc")
	if len(got) != 0 {
		t.Errorf("wait < threshold should emit no bytes; got %d", len(got))
	}
}

func TestQueueStatusSSEBytes_AboveThresholdEmitsEvent(t *testing.T) {
	got := QueueStatusSSEBytes(QueueSnapshot{Position: 3, Depth: 17}, 1800*time.Millisecond, "job-abc")
	s := string(got)
	for _, sub := range []string{
		"event: queue_status",
		`"position":3`,
		`"depth":17`,
		`"estimated_wait_ms":1800`,
		`"job_id":"job-abc"`,
	} {
		if !strings.Contains(s, sub) {
			t.Errorf("missing %q in event:\n%s", sub, s)
		}
	}
	// SSE frame must end with the blank line that terminates an event.
	if !strings.HasSuffix(s, "\n\n") {
		t.Errorf("SSE event must end with blank line; got: %q", s)
	}
}

func TestWantsSSE(t *testing.T) {
	for _, tt := range []struct {
		accept string
		want   bool
	}{
		{"text/event-stream", true},
		{"TEXT/EVENT-STREAM", true},
		{"application/json, text/event-stream", true},
		{"application/json", false},
		{"", false},
	} {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		if tt.accept != "" {
			r.Header.Set("Accept", tt.accept)
		}
		if got := WantsSSE(r); got != tt.want {
			t.Errorf("WantsSSE(%q)=%v, want %v", tt.accept, got, tt.want)
		}
	}
}

func TestQueueStatusSSEBytes_RenderedPositionParseable(t *testing.T) {
	// Sanity: a downstream parser should be able to pull the int
	// fields cleanly. We don't ship a parser here — just verify the
	// rendered numbers are exact (no trailing fluff like "1800ms").
	bytes := QueueStatusSSEBytes(QueueSnapshot{Position: 42, Depth: 99}, 555*time.Millisecond, "j")
	s := string(bytes)
	// Pluck the position number's substring.
	want := strconv.Itoa(42)
	if !strings.Contains(s, `"position":`+want) {
		t.Errorf("position not rendered as bare int; event=\n%s", s)
	}
}

func TestWriteQueueStatusSSE(t *testing.T) {
	rr := httptest.NewRecorder()
	written, err := WriteQueueStatusSSE(rr, rr, QueueSnapshot{Position: 2, Depth: 4}, time.Second, "job")
	if err != nil {
		t.Fatalf("WriteQueueStatusSSE: %v", err)
	}
	if !written || !rr.Flushed {
		t.Fatalf("written=%v flushed=%v, want both true", written, rr.Flushed)
	}
	if !strings.Contains(rr.Body.String(), "event: queue_status") {
		t.Errorf("missing queue_status frame: %q", rr.Body.String())
	}
}
