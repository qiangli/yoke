package sched

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Phase E — client-facing queue-state surface.
//
// Headers stamped on every dispatched response:
//   X-LLM-Job-ID         — echoed so clients can resume the job
//   X-Queue-Position     — caller's own in-flight count at admission
//   X-Queue-Depth        — total in-flight across all principals
//   X-Estimated-Wait-Ms  — wall-clock wait between admission and
//                          dispatch (0 in v1 because admission is
//                          synchronous; Phase F's real queue will
//                          start populating it)
//
// SSE event (text/event-stream callers only): a single
// "queue_status" event is emitted before the first model token
// when the dispatch wait exceeded the QueueStatusSSEThreshold.
// Clients that don't understand the event ignore it per SSE spec.

const (
	QueuePositionHeader     = "X-Queue-Position"
	QueueDepthHeader        = "X-Queue-Depth"
	QueueEstimatedWaitMsHdr = "X-Estimated-Wait-Ms"
)

// QueueStatusSSEThreshold is the wait floor for emitting the SSE
// event. Tuned so a fast dispatch (typical) stays silent.
const QueueStatusSSEThreshold = 250 * time.Millisecond

// QueueSnapshot is the in-flight view stamped on the response.
// Position is the caller's own concurrent count (after their
// current request was admitted, so always >= 1); Depth is the
// global total across all principals.
type QueueSnapshot struct {
	Position int
	Depth    int
}

// SnapshotForPrincipal returns a (position, depth) snapshot suitable
// for stamping on a response. position counts the caller's live
// requests including the current one; depth is the cluster-wide
// in-flight sum.
func (a *Admitter) SnapshotForPrincipal(principalID string) QueueSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	depth := 0
	for _, n := range a.inFlight {
		depth += n
	}
	return QueueSnapshot{
		Position: a.inFlight[principalID],
		Depth:    depth,
	}
}

// StampQueueHeaders writes the queue-state headers on the response.
// Safe to call repeatedly; the second write overrides the first
// (net/http Header.Set semantics). waitMs is the observed wait
// in milliseconds — pass 0 when admission was synchronous.
func StampQueueHeaders(h http.Header, snap QueueSnapshot, waitMs int) {
	h.Set(QueuePositionHeader, strconv.Itoa(snap.Position))
	h.Set(QueueDepthHeader, strconv.Itoa(snap.Depth))
	h.Set(QueueEstimatedWaitMsHdr, strconv.Itoa(waitMs))
}

// WantsSSE returns true when the request's Accept header indicates
// the caller is streaming.
func WantsSSE(r *http.Request) bool {
	a := strings.ToLower(r.Header.Get("Accept"))
	return strings.Contains(a, "text/event-stream")
}

// QueueStatusSSEBytes formats one "queue_status" event per the
// EventSource wire format. Returns the bytes to write directly to
// the response body. Returns empty (no bytes) when the wait was
// below threshold — caller writes nothing in that case.
func QueueStatusSSEBytes(snap QueueSnapshot, wait time.Duration, jobID string) []byte {
	if wait < QueueStatusSSEThreshold {
		return nil
	}
	payload := fmt.Sprintf(
		`{"position":%d,"depth":%d,"estimated_wait_ms":%d,"job_id":%q,"served_at":%q}`,
		snap.Position, snap.Depth, int(wait/time.Millisecond),
		jobID, time.Now().UTC().Format(time.RFC3339Nano),
	)
	return []byte("event: queue_status\ndata: " + payload + "\n\n")
}

// WriteQueueStatusSSE writes and flushes one queue-status frame when
// wait meets QueueStatusSSEThreshold. It returns whether a frame was
// written. If flusher is nil, the writer is checked for http.Flusher.
func WriteQueueStatusSSE(w http.ResponseWriter, flusher http.Flusher, snap QueueSnapshot, wait time.Duration, jobID string) (bool, error) {
	frame := QueueStatusSSEBytes(snap, wait, jobID)
	if len(frame) == 0 {
		return false, nil
	}
	if _, err := w.Write(frame); err != nil {
		return false, err
	}
	if flusher == nil {
		flusher, _ = w.(http.Flusher)
	}
	if flusher != nil {
		flusher.Flush()
	}
	return true, nil
}
