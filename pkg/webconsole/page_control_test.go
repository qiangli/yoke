package webconsole

import (
	"github.com/gorilla/websocket"
	"github.com/qiangli/yoke/pkg/websession"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPageControlOperatorGate(t *testing.T) {
	h := newTestHandler(t, Options{RequireLogin: true, Sessions: websession.NewStore(time.Hour, nil), Operators: []string{"owner@example.test"}, Shape: ShapeOptions{Home: t.TempDir(), WorkRoot: t.TempDir()}})
	for _, tc := range []struct {
		who  string
		code int
	}{{"owner@example.test", 200}, {"sharee@example.test", 403}} {
		hdr := http.Header{"X-Forwarded-Prefix": {"/cloud/apps"}, "Remote-User": {tc.who}, "Remote-Email": {tc.who}, "Remote-Groups": {"admin"}}
		w := send(h, "GET", "/api/page/console/pages", "127.0.0.1:5555", hdr, "")
		if w.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.who, w.Code, w.Body.String())
		}
		hdr.Set("Origin", "https://attacker.invalid")
		if got := send(h, "POST", "/api/page/console/invoke", "127.0.0.1:5555", hdr, `{}`).Code; got != 403 {
			t.Fatalf("cross origin: %d", got)
		}
	}
}

// Tunnel rewriting changes Host; the vetted cloud origin still upgrades.
func TestPageControlProxiedOrigin(t *testing.T) {
	h := newTestHandler(t, Options{RequireLogin: true, Operators: []string{"owner@example.test"}, Shape: ShapeOptions{Home: t.TempDir(), WorkRoot: t.TempDir()}})
	server := httptest.NewServer(h)
	defer server.Close()
	hdr := http.Header{"Origin": {"https://cloud.example.test"}, "X-Forwarded-Host": {"cloud.example.test"}, "X-Forwarded-Proto": {"https"}, "X-Forwarded-Prefix": {"/cloud/apps"}, "Remote-User": {"owner@example.test"}, "Remote-Email": {"owner@example.test"}, "Remote-Groups": {"admin"}}
	ws, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/page/console/events", hdr)
	if err != nil {
		t.Fatalf("upgrade: %v (%v)", err, resp)
	}
	defer ws.Close()
	var hello map[string]any
	if err := ws.ReadJSON(&hello); err != nil {
		t.Fatal(err)
	}
	if hello["page_id"] == nil {
		t.Fatalf("hello %v", hello)
	}
	hdr.Set("Origin", "https://evil.example.test")
	if bad, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/page/console/events", hdr); err == nil {
		bad.Close()
		t.Fatal("foreign origin connected")
	}
}

func TestPageControlRevokedSessionClosesSocket(t *testing.T) {
	sessions := websession.NewStore(time.Hour, nil)
	cookie, err := sessions.Mint(currentOSUser())
	if err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t, Options{RequireLogin: true, Sessions: sessions, Shape: ShapeOptions{Home: t.TempDir(), WorkRoot: t.TempDir()}})
	server := httptest.NewServer(h)
	defer server.Close()
	hdr := http.Header{"Origin": {server.URL}, "Cookie": {"bashy_console=" + cookie}}
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/page/console/events", hdr)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	var hello map[string]any
	if err := ws.ReadJSON(&hello); err != nil {
		t.Fatal(err)
	}
	sessions.Revoke(cookie)
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := ws.ReadJSON(&hello); err == nil {
		t.Fatal("revoked session socket remained open")
	} else if n, ok := err.(interface{ Timeout() bool }); ok && n.Timeout() {
		t.Fatal("socket did not close before deadline")
	}
}
