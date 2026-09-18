package webconsole

import (
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/qiangli/yoke/pkg/webterm"
)

// The terminal must survive the whole console stack, not just its own handler:
// coopauth.Mount clones the request, the gate wraps it, and otelhttp wraps the
// ResponseWriter — and a wrapper that does not forward Hijack turns the upgrade
// into a dead socket.
func TestTerminalWorksThroughTheConsoleStack(t *testing.T) {
	shell := []string{"/bin/sh"}
	marker, nl, sizeCmd := "THROUGH-THE-STACK", "\n", "stty size"
	if runtime.GOOS == "windows" {
		shell, nl, sizeCmd = []string{"cmd.exe"}, "\r\n", "echo 30 100"
	}
	h := newTestHandler(t, Options{Terminal: webterm.Options{Shell: shell}})
	srv := httptest.NewServer(h)
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/term/ws?cols=100&rows=30"
	c, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("dial through the console: %v (status %d)", err, code)
	}
	defer c.Close()

	if err := c.WriteMessage(websocket.BinaryMessage, []byte("echo "+marker+nl+sizeCmd+nl)); err != nil {
		t.Fatalf("write: %v", err)
	}

	var sb strings.Builder
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v (saw %d bytes)", err, sb.Len())
		}
		sb.Write(data)
		if strings.Contains(sb.String(), marker) && strings.Contains(sb.String(), "30 100") {
			return
		}
	}
}
