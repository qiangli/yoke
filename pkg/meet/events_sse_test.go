package meet

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/room"
)

type sseFrame struct {
	id   string
	data map[string]any
}

func readFrames(t *testing.T, srv string, path string, hdr map[string]string, n int) []sseFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	var frames []sseFrame
	var cur sseFrame
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.data); err != nil {
				t.Fatal(err)
			}
		case line == "" && cur.data != nil:
			frames = append(frames, cur)
			cur = sseFrame{}
			if len(frames) == n {
				return frames
			}
		}
	}
	t.Fatalf("stream ended with %d/%d frames: %v", len(frames), n, sc.Err())
	return nil
}

func TestEventsSSEReconnectHasNoGap(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	for i := 0; i < 3; i++ {
		room.Record(room.EventSprint, "mgr", "sprint:#1", "note", "n")
	}
	srv := serveTest(t)

	first := readFrames(t, srv.URL, "/api/events?from=0", nil, 3)
	for i, f := range first {
		if f.id != []string{"1", "2", "3"}[i] || f.data["seq"] != float64(i+1) {
			t.Fatalf("frame %d = %+v", i, f)
		}
	}
	// Events land while disconnected; reconnect from the last id seen.
	room.Record(room.EventTodo, "a", "todo:x", "done", "d")
	room.Record(room.EventHandoff, "a", "handoff:h", "created", "b")
	again := readFrames(t, srv.URL, "/api/events?from=3", nil, 2)
	if again[0].data["seq"] != float64(4) || again[0].data["type"] != "todo" || again[1].data["seq"] != float64(5) {
		t.Fatalf("reconnect frames = %+v", again)
	}
	viaHeader := readFrames(t, srv.URL, "/api/events", map[string]string{"Last-Event-ID": "4"}, 1)
	if viaHeader[0].data["seq"] != float64(5) {
		t.Fatalf("Last-Event-ID resume = %+v", viaHeader)
	}
	// Live delivery on an open stream.
	go func() { time.Sleep(200 * time.Millisecond); room.Record(room.EventNote, "a", "", "", "live") }()
	live := readFrames(t, srv.URL, "/api/events?from=5&kind=note", nil, 1)
	if live[0].data["seq"] != float64(6) {
		t.Fatalf("live = %+v", live)
	}
}

func TestEventsSSEResyncPastHead(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	room.Record(room.EventNote, "a", "", "", "x")
	srv := serveTest(t)
	got := readFrames(t, srv.URL, "/api/events?from=40", nil, 1)
	if got[0].data["type"] != "resync" || got[0].id != "1" {
		t.Fatalf("got %+v", got)
	}
}

func TestEventsSSERejectsBadFrom(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	srv := serveTest(t)
	resp, err := http.Get(srv.URL + "/api/events?from=abc")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
