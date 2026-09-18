package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/qiangli/yoke/pkg/browser/wire"
)

func TestActionToParams(t *testing.T) {
	cases := []struct {
		action wire.Action
		method string
		check  func(map[string]any) bool
	}{
		{wire.Action{Type: wire.ActionNavigate, URL: "https://x"}, "navigate",
			func(p map[string]any) bool { return p["url"] == "https://x" }},
		{wire.Action{Type: wire.ActionEvaluate, Script: "document.title"}, "evaluate",
			func(p map[string]any) bool { return p["script"] == "document.title" }},
		{wire.Action{Type: wire.ActionClick, Selector: "#go"}, "click",
			func(p map[string]any) bool { return p["selector"] == "#go" }},
		{wire.Action{Type: wire.ActionCookiesGet, Name: "sid", Domain: "x"}, "cookies_get",
			func(p map[string]any) bool { return p["name"] == "sid" && p["domain"] == "x" }},
	}
	for _, c := range cases {
		method, params, err := actionToParams(c.action)
		if err != nil {
			t.Fatalf("%s: %v", c.action.Type, err)
		}
		if method != c.method {
			t.Errorf("%s → method %q, want %q", c.action.Type, method, c.method)
		}
		if !c.check(params) {
			t.Errorf("%s → params %v failed check", c.action.Type, params)
		}
	}
	if _, _, err := actionToParams(wire.Action{Type: "nonesuch"}); err == nil {
		t.Error("unknown action must error, not silently map")
	}
}

func TestVersionLess(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.4.0", "0.5.0", true},
		{"0.5.0", "0.5.0", false},
		{"0.6.6", "0.5.0", false},
		{"0.10.0", "0.9.0", false}, // numeric, not lexical
	} {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("versionLess(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestUnmarshalExt(t *testing.T) {
	res, _ := unmarshalExt(json.RawMessage(`{"title":"T","url":"https://x","content":"C"}`))
	if !res.Success || res.Title != "T" || res.URL != "https://x" || res.Content != "C" {
		t.Fatalf("unmarshalExt lost fields: %+v", res)
	}
}

// TestHubRoundTrip stands up a real hub, dials it as a fake extension,
// completes the _hello handshake, and drives one action through the
// /dispatch bridge — proving the whole server path without Chrome.
func TestHubRoundTrip(t *testing.T) {
	// A high test port so we never collide with a real hub on DefaultPort.
	svc := New(58973)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.EnsureReady(ctx); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	defer svc.Close()
	waitHealthy(t, 58973)

	// Fake extension: dial the hub, send _hello, then echo every request
	// as a result carrying the requested url (so the round-trip is
	// observable).
	conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:58973/ws", nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	defer conn.Close()

	hello, _ := json.Marshal(wsResponse{
		Method: "_hello",
		Result: mustJSON(extHello{Version: "0.6.6", Methods: []string{"navigate"}}),
	})
	if err := conn.WriteMessage(websocket.TextMessage, hello); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req wsRequest
			if json.Unmarshal(raw, &req) != nil || req.ID == 0 {
				continue
			}
			url, _ := req.Params["url"].(string)
			resp, _ := json.Marshal(wsResponse{ID: req.ID, Result: mustJSON(extResult{URL: url, Title: "ok"})})
			_ = conn.WriteMessage(websocket.TextMessage, resp)
		}
	}()

	// Give the hub a moment to parse _hello.
	deadline := time.Now().Add(2 * time.Second)
	for !svc.Connected() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !svc.Connected() {
		t.Fatal("hub never registered the extension connection")
	}

	// Drive via the /dispatch HTTP bridge (the cross-process path a
	// `bashy browser --mode live` client uses).
	body, _ := json.Marshal(map[string]any{"method": "navigate", "params": map[string]any{"url": "https://ai.example"}})
	httpResp, err := http.Post("http://127.0.0.1:58973/dispatch", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /dispatch: %v", err)
	}
	defer httpResp.Body.Close()
	raw, _ := io.ReadAll(httpResp.Body)
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode dispatch resp: %v (%s)", err, raw)
	}
	if out.Error != "" {
		t.Fatalf("dispatch error: %s", out.Error)
	}
	got, _ := unmarshalExt(out.Result)
	if got.URL != "https://ai.example" || got.Title != "ok" {
		t.Fatalf("round-trip result = %+v", got)
	}
}

// TestNotConnected: with no extension attached, an action returns the
// actionable not-connected error rather than hanging.
func TestNotConnected(t *testing.T) {
	svc := New(58974)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.EnsureReady(ctx); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	defer svc.Close()
	res, err := svc.Execute(ctx, wire.Action{Type: wire.ActionNavigate, URL: "https://x"})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if res.Error == "" {
		t.Fatal("expected a not-connected error")
	}
}

func waitHealthy(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if probeHealth(port) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("hub on %d never became healthy", port)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("mustJSON: %v", err))
	}
	return b
}
