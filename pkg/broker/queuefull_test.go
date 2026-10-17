package broker

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestQueueFullRefusesWithReason (Sprint 455 P11): with the device busy
// (in-flight) and the wait queue at its limit behind blocked fake backends,
// the next request is refused immediately — not parked, not dropped — with a
// machine-readable reason, and a queued request proceeds once a slot frees.
func TestQueueFullRefusesWithReason(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.QueueLimit = 1 })
	h.eng.hold = make(chan struct{})
	h.eng.started = make(chan string, 8)

	first := postAsync(h, chat("llama3.2:3b"), nil)
	<-h.eng.started // device busy: the in-flight slot is taken
	queued := postAsync(h, chat("llama3.2:3b"), nil)
	waitQueued(t, h.b.device, 1) // the one queue slot is taken

	start := time.Now()
	resp, out := h.do("POST", "/api/chat", chat("llama3.2:3b"), nil)
	elapsed := time.Since(start)
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("queue-full refusal took %v, want it immediately (<100ms)", elapsed)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("queue full status = %d, want 429 (body %v)", resp.StatusCode, out)
	}
	assertQueueFullBody(t, resp, out, 1)

	// The refusal is kept in the audit record, not silently dropped.
	found := false
	for _, r := range h.b.audit.snapshot() {
		if r.Status == http.StatusTooManyRequests && strings.Contains(r.Error, "queue") {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit keeps no queue-full refusal: %+v", h.b.audit.snapshot())
	}

	close(h.eng.hold) // free the slot: the queued request must proceed
	if got := <-first; got != 200 {
		t.Fatalf("in-flight request status = %d, want 200", got)
	}
	if got := <-queued; got != 200 {
		t.Fatalf("queued request status = %d, want 200 after a slot freed", got)
	}
}

// TestQueueFullDrainingIs503: while the door is draining, the same refusal
// answers 503 so callers back off instead of retrying into a closing door.
func TestQueueFullDrainingIs503(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.QueueLimit = 1 })
	h.eng.hold = make(chan struct{})
	h.eng.started = make(chan string, 8)

	first := postAsync(h, chat("llama3.2:3b"), nil)
	<-h.eng.started
	queued := postAsync(h, chat("llama3.2:3b"), nil)
	waitQueued(t, h.b.device, 1)

	h.b.SetDraining(true)
	resp, out := h.do("POST", "/api/chat", chat("llama3.2:3b"), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("draining queue-full status = %d, want 503 (body %v)", resp.StatusCode, out)
	}
	assertQueueFullBody(t, resp, out, 1)

	h.b.SetDraining(false)
	close(h.eng.hold)
	if got := <-first; got != 200 {
		t.Fatalf("in-flight request status = %d, want 200", got)
	}
	if got := <-queued; got != 200 {
		t.Fatalf("queued request status = %d, want 200 after a slot freed", got)
	}
}

// postAsync fires one chat request and reports its status asynchronously.
func postAsync(h *harness, body map[string]any, hdr map[string]string) <-chan int {
	out := make(chan int, 1)
	go func() {
		resp, _ := h.do("POST", "/api/chat", body, hdr)
		out <- resp.StatusCode
	}()
	return out
}

func assertQueueFullBody(t *testing.T, resp *http.Response, out map[string]any, limit int) {
	t.Helper()
	if out["type"] != "queue_full" {
		t.Fatalf("refusal body type = %v, want queue_full (body %v)", out["type"], out)
	}
	if out["queue"] != DeviceQueueName {
		t.Fatalf("refusal body queue = %v, want %q (body %v)", out["queue"], DeviceQueueName, out)
	}
	depth, _ := out["depth"].(float64)
	if depth < 1 {
		t.Fatalf("refusal body depth = %v, want the live queue depth >= 1 (body %v)", out["depth"], out)
	}
	if lim, _ := out["limit"].(float64); int(lim) != limit {
		t.Fatalf("refusal body limit = %v, want %d (body %v)", out["limit"], limit, out)
	}
	retry, _ := out["retry_after_s"].(float64)
	if retry < 1 {
		t.Fatalf("refusal body retry_after_s = %v, want >= 1 (body %v)", out["retry_after_s"], out)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("queue-full refusal carries no Retry-After header")
	}
}
