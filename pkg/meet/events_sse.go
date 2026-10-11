package meet

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/room"
)

// sseKeepAlive keeps an idle stream from being reaped by a proxy; a comment
// line is ignored by EventSource.
const sseKeepAlive = 15 * time.Second

// handleEvents serves the host event log as SSE: GET /api/events?from=SEQ.
//
// It runs room.Stream, the same reader `bashy events` uses, so the two cannot
// disagree about gaps. Every frame carries `id: <seq>`, so a browser's
// EventSource reconnects with Last-Event-ID and resumes with no gap; an
// explicit ?from=SEQ wins over the header. A position the log can no longer
// continue (archived past it, or the log replaced) is answered with a
// {"type":"resync"} frame before delivery goes on. ?kind=a,b narrows by type.
func handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("meet: streaming unsupported"))
		return
	}
	q := r.URL.Query()
	raw, explicit := q.Get("from"), q.Has("from")
	if !explicit {
		raw = strings.TrimSpace(r.Header.Get("Last-Event-ID"))
		explicit = raw != ""
	}
	var from int64
	if explicit {
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("meet: from must be a non-negative sequence number"))
			return
		}
		from = n
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx, cancel := context.WithCancel(r.Context())
	var mu sync.Mutex
	kept := make(chan struct{})
	defer func() { cancel(); <-kept }()
	go func() {
		defer close(kept)
		t := time.NewTicker(sseKeepAlive)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				mu.Lock()
				_, err := fmt.Fprint(w, ": keepalive\n\n")
				flusher.Flush()
				mu.Unlock()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	_ = room.Stream(ctx, room.StreamOptions{
		After: from, Explicit: explicit, Follow: true, Poll: 500 * time.Millisecond,
		Match: bus.KindFilter([]string{q.Get("kind")}),
	}, func(it room.StreamItem) error {
		line, err := bus.EncodeStreamItem(it)
		if err != nil {
			return nil
		}
		seq := it.Event.Seq
		if it.Resync != nil {
			seq = it.Resync.Seq
		}
		mu.Lock()
		defer mu.Unlock()
		if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", seq, line); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	})
}
