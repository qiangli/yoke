package pagecontrol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type fixture struct {
	h      *Hub
	server *httptest.Server
}

func setup(t *testing.T, options ...Options) *fixture {
	t.Helper()
	h := New(options...)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.Serve(w, r, r.Header.Get("X-Test-Principal")) }))
	t.Cleanup(func() { h.Close(); server.Close() })
	return &fixture{h, server}
}
func allowed() Options {
	return Options{AllowAction: func(s string) bool { return s == "page.search" || s == "page.toggle" }}
}
func (f *fixture) request(t *testing.T, principal, method, path string, in any) (int, []byte) {
	t.Helper()
	var b bytes.Buffer
	if in != nil {
		if err := json.NewEncoder(&b).Encode(in); err != nil {
			t.Fatal(err)
		}
	}
	req, _ := http.NewRequest(method, f.server.URL+path, &b)
	req.Header.Set("X-Test-Principal", principal)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	out.ReadFrom(resp.Body)
	return resp.StatusCode, out.Bytes()
}
func (f *fixture) page(t *testing.T, principal string) (*websocket.Conn, string) {
	t.Helper()
	headers := http.Header{"Origin": []string{f.server.URL}, "X-Test-Principal": []string{principal}}
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(f.server.URL, "http")+"/events", headers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	var hello struct {
		ID string `json:"page_id"`
	}
	if err = ws.ReadJSON(&hello); err != nil {
		t.Fatal(err)
	}
	if err = ws.WriteJSON(map[string]any{"actions": []Action{{"page.search", "Set search text"}, {"page.toggle", "Toggle presentation"}}, "label": "Example"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		code, b := f.request(t, principal, "GET", "/pages", nil)
		var v struct {
			Pages []Page `json:"pages"`
		}
		json.Unmarshal(b, &v)
		return code == 200 && len(v.Pages) > 0 && len(v.Pages[0].Actions) == 2
	})
	return ws, hello.ID
}
func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
func (f *fixture) invoke(t *testing.T, principal, page string) Command {
	t.Helper()
	code, b := f.request(t, principal, "POST", "/invoke", map[string]any{"page_id": page, "action": "page.search", "input": "john"})
	if code != 202 {
		t.Fatalf("invoke %d: %s", code, b)
	}
	var c Command
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}
func (f *fixture) result(t *testing.T, principal, id string) Command {
	t.Helper()
	code, b := f.request(t, principal, "GET", "/result?id="+id, nil)
	if code != 200 {
		t.Fatalf("result %d %s", code, b)
	}
	var c Command
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}
func TestIsolatedActionsAndResults(t *testing.T) {
	f := setup(t, allowed())
	ws, page := f.page(t, "alice")
	_, otherPage := f.page(t, "bob")
	code, b := f.request(t, "bob", "GET", "/pages", nil)
	if code != 200 || bytes.Contains(b, []byte(page)) {
		t.Fatalf("cross-principal page disclosure: %s", b)
	}
	code, _ = f.request(t, "bob", "POST", "/invoke", map[string]any{"page_id": page, "action": "page.search"})
	if code != 404 {
		t.Fatal(code)
	}
	c := f.invoke(t, "alice", page)
	var wire struct {
		Command Command `json:"command"`
	}
	ws.SetReadDeadline(time.Now().Add(time.Second))
	if err := ws.ReadJSON(&wire); err != nil {
		t.Fatal(err)
	}
	if wire.Command.ID != c.ID || wire.Command.Status != "sent" || string(wire.Command.Input) != `"john"` {
		t.Fatal(wire)
	}
	code, _ = f.request(t, "bob", "GET", "/result?id="+c.ID, nil)
	if code != 404 {
		t.Fatal("cross-principal result", code)
	}
	// Wrong-page messages cannot complete another page's command.
	f.h.mu.Lock()
	f.h.commands[c.ID].PageID = otherPage
	f.h.mu.Unlock()
	ws.WriteJSON(map[string]any{"id": c.ID, "result": true})
	// Put a second message behind it; the first must be processed before the
	// advertisement appears in /pages, making this check deterministic.
	ws.WriteJSON(map[string]any{"actions": []Action{{"page.search", "after foreign acknowledgement"}}})
	waitFor(t, func() bool {
		_, b := f.request(t, "alice", "GET", "/pages", nil)
		return bytes.Contains(b, []byte("after foreign"))
	})
	if got := f.result(t, "alice", c.ID); got.Status != "sent" {
		t.Fatal("wrong-page ack accepted", got)
	}
	f.h.mu.Lock()
	f.h.commands[c.ID].PageID = page
	f.h.mu.Unlock()
	ws.WriteJSON(map[string]any{"id": c.ID, "result": map[string]string{"search": "john"}})
	waitFor(t, func() bool { return f.result(t, "alice", c.ID).Status == "completed" })
	// Duplicate completion cannot overwrite the original result.
	ws.WriteJSON(map[string]any{"id": c.ID, "result": false})
	ws.WriteJSON(map[string]any{"actions": []Action{{"page.search", "after duplicate"}}})
	waitFor(t, func() bool {
		_, b := f.request(t, "alice", "GET", "/pages", nil)
		return bytes.Contains(b, []byte("after duplicate"))
	})
	if got := f.result(t, "alice", c.ID); string(got.Result) != `{"search":"john"}` {
		t.Fatal(got)
	}
}
func TestOriginAndDefaultDeny(t *testing.T) {
	f := setup(t)
	code, _ := f.request(t, "", "GET", "/pages", nil)
	if code != 401 {
		t.Fatal(code)
	}
	headers := http.Header{"Origin": []string{"https://evil.example"}, "X-Test-Principal": []string{"alice"}}
	_, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(f.server.URL, "http")+"/events", headers)
	if err == nil || resp.StatusCode != 403 {
		t.Fatal(err, resp)
	}
	headers.Set("Origin", f.server.URL)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(f.server.URL, "http")+"/events", headers)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	var hello any
	ws.ReadJSON(&hello)
	ws.WriteJSON(map[string]any{"actions": []Action{{"page.search", "denied"}}})
	ws.SetReadDeadline(time.Now().Add(time.Second))
	if ws.ReadJSON(&hello) == nil {
		t.Fatal("default allowlist did not close invalid advertisement")
	}
	req, _ := http.NewRequest("POST", f.server.URL+"/invoke", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("X-Test-Principal", "alice")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal(res.StatusCode)
	}
}
func TestUnavailableActionsAndBoundedInputs(t *testing.T) {
	f := setup(t, allowed())
	_, page := f.page(t, "alice")
	for _, tc := range []struct {
		action string
		input  any
		want   int
	}{{"shell.exec", nil, 403}, {"page.search", strings.Repeat("x", maxBytes+1), 422}} {
		code, b := f.request(t, "alice", "POST", "/invoke", map[string]any{"page_id": page, "action": tc.action, "input": tc.input})
		if code != tc.want {
			t.Fatalf("%d %s", code, b)
		}
	}
	f.h.mu.Lock()
	f.h.pages[page].Actions = []Action{}
	f.h.mu.Unlock()
	code, _ := f.request(t, "alice", "POST", "/invoke", map[string]any{"page_id": page, "action": "page.search"})
	if code != 422 {
		t.Fatal(code)
	}
}
func TestDisconnectAndExpiryNeverReplay(t *testing.T) {
	f := setup(t, allowed())
	ws, page := f.page(t, "alice")
	c := f.invoke(t, "alice", page)
	var wire any
	ws.SetReadDeadline(time.Now().Add(time.Second))
	if err := ws.ReadJSON(&wire); err != nil {
		t.Fatal(err)
	}
	ws.Close()
	waitFor(t, func() bool { return f.result(t, "alice", c.ID).Status == "expired" })
	fresh, freshID := f.page(t, "alice")
	if freshID == page {
		t.Fatal("page identity reused")
	}
	fresh.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if fresh.ReadJSON(&wire) == nil {
		t.Fatal("disconnected command replayed")
	}
	// Expiry is checked on reads, independently of websocket activity.
	f.h.mu.Lock()
	f.h.commands[c.ID].Status = "queued"
	f.h.commands[c.ID].expires = time.Now().Add(-time.Second)
	f.h.mu.Unlock()
	if got := f.result(t, "alice", c.ID); got.Status != "expired" {
		t.Fatal(got)
	}
}
func TestConcurrentDispatchAndClose(t *testing.T) {
	f := setup(t, allowed())
	ws, page := f.page(t, "alice")
	var wg sync.WaitGroup
	ids := make(chan string, 12)
	for range 12 {
		wg.Add(1)
		go func() { defer wg.Done(); ids <- f.invoke(t, "alice", page).ID }()
	}
	wg.Wait()
	close(ids)
	expected := map[string]bool{}
	for id := range ids {
		expected[id] = true
	}
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	seen := map[string]bool{}
	for range 12 {
		var wire struct {
			Command Command `json:"command"`
		}
		if err := ws.ReadJSON(&wire); err != nil {
			t.Fatal(err)
		}
		id := wire.Command.ID
		if !expected[id] || seen[id] {
			t.Fatal("duplicate or unexpected delivery", id)
		}
		seen[id] = true
		ws.WriteJSON(map[string]any{"id": id, "result": true})
	}
	waitFor(t, func() bool {
		for id := range expected {
			if f.result(t, "alice", id).Status != "completed" {
				return false
			}
		}
		return true
	})
	f.h.ClosePrincipal("alice")
	var wire any
	ws.SetReadDeadline(time.Now().Add(time.Second))
	if ws.ReadJSON(&wire) == nil {
		t.Fatal("revoked page socket remained open")
	}
	code, b := f.request(t, "alice", "GET", "/pages", nil)
	if code != 200 || string(bytes.TrimSpace(b)) != `{"pages":[]}` {
		t.Fatal(code, string(b))
	}
	f.h.Close()
	code, _ = f.request(t, "alice", "GET", "/pages", nil)
	if code != 503 {
		t.Fatal(code)
	}
}
func TestResultRetentionIsBounded(t *testing.T) {
	f := setup(t, allowed())
	_, page := f.page(t, "alice")
	f.h.mu.Lock()
	for i := range 128 {
		id := fmt.Sprint(i)
		f.h.commands[id] = &pending{principal: "alice", expires: time.Now(), Command: Command{ID: id, Status: "completed"}}
	}
	f.h.mu.Unlock()
	code, _ := f.request(t, "alice", "POST", "/invoke", map[string]any{"page_id": page, "action": "page.search"})
	if code != 429 {
		t.Fatal(code)
	}
	f.h.mu.Lock()
	for _, c := range f.h.commands {
		c.expires = time.Now().Add(-6 * time.Minute)
	}
	f.h.mu.Unlock()
	f.invoke(t, "alice", page)
}

type blockedResponse struct {
	header  http.Header
	entered chan struct{}
	release chan struct{}
}

func (b *blockedResponse) Header() http.Header { return b.header }
func (b *blockedResponse) WriteHeader(int)     {}
func (b *blockedResponse) Write(p []byte) (int, error) {
	close(b.entered)
	<-b.release
	return len(p), nil
}
func TestSlowInvokeResponseDoesNotBlockOtherPrincipals(t *testing.T) {
	f := setup(t, allowed())
	_, page := f.page(t, "alice")
	payload, _ := json.Marshal(map[string]any{"page_id": page, "action": "page.search", "input": "x"})
	req := httptest.NewRequest("POST", "/invoke", bytes.NewReader(payload))
	blocked := &blockedResponse{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); f.h.Serve(blocked, req, "alice") }()
	defer func() { close(blocked.release); <-done }()
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("invoke never reached response")
	}
	otherDone := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		f.h.Serve(w, httptest.NewRequest("GET", "/pages", nil), "bob")
		otherDone <- w.Code
	}()
	select {
	case code := <-otherDone:
		if code != 200 {
			t.Fatal(code)
		}
	case <-time.After(time.Second):
		t.Fatal("slow HTTP response held the hub mutex")
	}
}
