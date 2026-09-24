// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package meet

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/room"
)

func consoleServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/dms/{agent}/console", handleConsoleDM)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func dialConsole(t *testing.T, srv *httptest.Server, agent string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/dms/" + agent + "/console"
	return websocket.DefaultDialer.Dial(u, nil)
}

// The console replays the seat's raw capture from its start at the card's
// geometry, then follows it — the bytes arrive exactly as the terminal got them.
func TestConsoleReplaysThenFollowsTheRawCapture(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	logPath := filepath.Join(t.TempDir(), "seat.log")
	first := "\x1b[?2026h\x1b[1;1H\x1b[2mbanner\x1b[0m\r\n"
	if err := os.WriteFile(logPath, []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := "mirror-agent"
	if err := room.Join(room.Card{ID: room.AgentClaimID(agent), Tool: "claude", Binding: "claude:x",
		Mode: "interactive", PID: os.Getpid(), LogPath: logPath, Cols: 120, Rows: 40}); err != nil {
		t.Fatal(err)
	}
	conn, _, err := dialConsole(t, consoleServer(t), agent)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	kind, data, err := conn.ReadMessage()
	if err != nil || kind != websocket.TextMessage {
		t.Fatalf("first frame: kind=%d err=%v", kind, err)
	}
	var geo consoleFrame
	if json.Unmarshal(data, &geo) != nil || geo != (consoleFrame{Type: "geometry", Cols: 120, Rows: 40}) {
		t.Fatalf("geometry frame = %s", data)
	}
	var got strings.Builder
	read := func(want string) {
		for !strings.Contains(got.String(), want) {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("waiting for %q, have %q: %v", want, got.String(), err)
			}
			if kind == websocket.BinaryMessage {
				got.Write(data)
			}
		}
	}
	read("banner")
	if !strings.HasPrefix(got.String(), first) {
		t.Fatalf("replay is not byte-exact from the start: %q", got.String())
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("⏺ PINEAPPLE\r\n")
	_ = f.Close()
	read("⏺ PINEAPPLE")
	if got.String() != first+"⏺ PINEAPPLE\r\n" {
		t.Fatalf("stream = %q", got.String())
	}
}

// A seat bashy does not own has no capture: the console refuses before
// upgrading, and says how to get one.
func TestConsoleRefusesASeatWithoutACapture(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	srv := consoleServer(t)
	_, resp, err := dialConsole(t, srv, "nobody-live")
	if err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no seat: err=%v resp=%v", err, resp)
	}
	if err := room.Join(room.Card{ID: room.AgentClaimID("shell-only"), Tool: "codex", Binding: "codex:x",
		Mode: "shell", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	_, resp, err = dialConsole(t, srv, "shell-only")
	if err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("shell seat: err=%v resp=%v", err, resp)
	}
}

// A DM to a live seat is pushed into it even when the agent's name is not
// already path-safe: the seat's id is the sanitized name, and the push must use
// it (the composer is the Term view's only input).
func TestDeliverToLiveSeatSteersADottedAgentName(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	agent := "codex-gpt5.6-sol"
	card := room.Card{ID: room.AgentClaimID(agent), Tool: "codex", Binding: "codex:gpt5.6-sol",
		Mode: "interactive", PID: os.Getpid(), CtlSock: filepath.Join(t.TempDir(), "ctl.sock")}
	if err := room.Join(card); err != nil {
		t.Fatal(err)
	}
	var gotSock, gotText string
	prev := bus.SteerFrame
	bus.SteerFrame = func(sock, text string) error { gotSock, gotText = sock, text; return nil }
	t.Cleanup(func() { bus.SteerFrame = prev })

	seat, ok := liveSeat(agent)
	if !ok {
		t.Fatal("live seat not found")
	}
	d := deliverToLiveSeat(seat, agent, "hello")
	if !d.Steered || gotSock != card.CtlSock || gotText != "hello" {
		t.Fatalf("delivery = %+v (sock %q, text %q), want a steer into %s", d, gotSock, gotText, card.CtlSock)
	}
}
