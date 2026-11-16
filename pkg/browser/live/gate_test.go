package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return p
}

func isolateGate(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	saved := gateDir
	gateDir = func() string { return dir }
	t.Cleanup(func() { gateDir = saved })
}

// The gate pauses the hub without stopping it: the extension, agent calls
// and copy sheets are refused; resume restores them; the state persists.
func TestHubGatePausesAndResumes(t *testing.T) {
	isolateGate(t)
	port := freePort(t)
	svc := New(port)
	if err := svc.EnsureReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	waitHealthy(t, port)
	ctx := context.Background()

	if g, err := GateStatus(ctx, port); err != nil || g.Paused {
		t.Fatalf("default gate = %+v, %v; want open", g, err)
	}
	// A change without the control token is refused.
	body, _ := json.Marshal(map[string]any{"paused": true})
	resp, err := http.Post("http://127.0.0.1:"+itoa(port)+"/gate", "application/json", bytes.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tokenless pause: %v %v", err, resp)
	}

	ext, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:"+itoa(port)+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ext.Close()
	waitConn := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for svc.hub.connected() != want && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitConn(true)

	if g, err := SetGate(ctx, port, true, "test"); err != nil || !g.Paused || g.By != "test" {
		t.Fatalf("pause = %+v, %v", g, err)
	}
	if code := closeCodeOf(ext); code != CloseHubPaused {
		t.Fatalf("connected extension got close %d, want %d", code, CloseHubPaused)
	}
	again, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:"+itoa(port)+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	if code := closeCodeOf(again); code != CloseHubPaused {
		t.Fatalf("a dial while paused got close %d", code)
	}
	disp, _ := json.Marshal(map[string]any{"method": "extract"})
	r, err := http.Post("http://127.0.0.1:"+itoa(port)+"/dispatch", "application/json", bytes.NewReader(disp))
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Error string }
	_ = json.NewDecoder(r.Body).Decode(&out)
	r.Body.Close()
	if !strings.Contains(out.Error, "paused") {
		t.Fatalf("dispatch while paused: %q", out.Error)
	}
	if _, err := svc.hub.call(ctx, "extract", nil); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("in-process call while paused: %v", err)
	}
	reg, _ := json.Marshal(map[string]string{"path": "/x.html"})
	r, _ = http.Post("http://127.0.0.1:"+itoa(port)+"/sheet", "application/json", bytes.NewReader(reg))
	if r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("sheet register while paused: %d", r.StatusCode)
	}
	if st := LiveStatus(ctx, port); !st.Paused || st.Reachable || !strings.Contains(st.Message, "resume") {
		t.Fatalf("status while paused = %+v", st)
	}
	if ReadGate(port).Paused != true {
		t.Fatal("pause was not persisted")
	}

	if g, err := SetGate(ctx, port, false, "test"); err != nil || g.Paused {
		t.Fatalf("resume = %+v, %v", g, err)
	}
	ok, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:"+itoa(port)+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ok.Close()
	waitConn(true)
	if !svc.hub.connected() {
		t.Fatal("extension not accepted after resume")
	}
}

// A pause recorded while no hub ran is honoured by the next hub.
func TestHubStartsWithTheRecordedGate(t *testing.T) {
	isolateGate(t)
	port := freePort(t)
	if _, err := WriteGate(port, true, "test"); err != nil {
		t.Fatal(err)
	}
	svc := New(port)
	if err := svc.EnsureReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	waitHealthy(t, port)
	if g, err := GateStatus(context.Background(), port); err != nil || !g.Paused {
		t.Fatalf("started gate = %+v, %v; want paused", g, err)
	}
}

func closeCodeOf(c *websocket.Conn) int {
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				return ce.Code
			}
			return -1
		}
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
