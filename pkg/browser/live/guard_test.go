package live

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/qiangli/yoke/pkg/browser/guard"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

// fakeExt is a scripted extension on the hub's websocket that records every
// frame it is sent, so a test can prove what never reached the page.
type fakeExt struct {
	mu     sync.Mutex
	frames []string
	field  guard.Field
	// page is what the page makes of a guard_fill value (nil: keeps it);
	// noVerdict plays a 0.9.1 extension that answers without one.
	page      func(string) string
	noVerdict bool
}

// fill answers guard_fill as background.js does: the page's verdict and
// the two lengths, nothing of the value.
func (f *fakeExt) fill(params map[string]any) json.RawMessage {
	if f.noVerdict {
		return mustJSON(extResult{Content: "filled"})
	}
	text, _ := params["text"].(string)
	got := text
	if f.page != nil {
		got = f.page(text)
	}
	v := map[string]any{"verified": "mismatch", "field_len": len(got), "value_len": len(text)}
	if got == text {
		v["verified"] = "match"
	}
	data, _ := json.Marshal(v)
	v["content"], v["data"] = "filled", string(data)
	return mustJSON(v)
}

func (f *fakeExt) seen() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.frames, "\n")
}

func guardIsolate(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("BASHY_ASK_DIR", filepath.Join(root, "ask"))
	t.Setenv("BASHY_HOME", filepath.Join(root, "home"))
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(root, "home", "room"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("BASHY_SECRETS_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")
}

func startHubWithExt(t *testing.T, version string, methods []string, field guard.Field) (*Service, *fakeExt, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	svc := New(port)
	if err := svc.EnsureReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	waitHealthy(t, port)
	conn, _, err := websocket.DefaultDialer.Dial("ws://"+svc.hub.addr+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	hello, _ := json.Marshal(wsResponse{Method: "_hello", Result: mustJSON(extHello{Version: version, Methods: methods})})
	if err := conn.WriteMessage(websocket.TextMessage, hello); err != nil {
		t.Fatal(err)
	}
	ext := &fakeExt{field: field}
	if ver, ok := extSetup[t.Name()]; ok {
		ext.page, ext.noVerdict = ver.page, ver.noVerdict
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
			ext.mu.Lock()
			ext.frames = append(ext.frames, string(raw))
			ext.mu.Unlock()
			var result json.RawMessage
			switch req.Method {
			case "guard_field":
				result = mustJSON(ext.field)
			case "guard_fill":
				result = ext.fill(req.Params)
			default:
				result = mustJSON(extResult{Content: req.Method + "-done"})
			}
			resp, _ := json.Marshal(wsResponse{ID: req.ID, Result: result})
			_ = conn.WriteMessage(websocket.TextMessage, resp)
		}
	}()
	for deadline := time.Now().Add(2 * time.Second); svc.hub.ExtVersion() == "" && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	return svc, ext, port
}

var guardedMethods = []string{"navigate", "type", "select", "keyboard_press", "extract", "guard_field", "guard_focus", "guard_fill"}

// A guarded field never receives the text — in the hub role and through
// another process's hub (client role, /dispatch) alike. The extension sees
// only the guard_field preflight and the human-path focus.
func TestLiveGuardedTypeNeverSendsTheText(t *testing.T) {
	guardIsolate(t)
	t.Setenv(guard.FakeAskEnv, "fake:self")
	field := guard.Field{Guarded: true, Reason: "field:label-ssn", Question: "Social Security Number", Site: "acme.typeform.com"}
	hubSvc, ext, port := startHubWithExt(t, "0.9.1", guardedMethods, field)

	client := New(port)
	if err := client.EnsureReady(context.Background()); err != nil || client.role != roleClient {
		t.Fatalf("second service did not take the client role: %v", err)
	}
	for name, s := range map[string]*Service{"hub": hubSvc, "client": client} {
		res, err := s.Execute(context.Background(), wire.Action{Type: wire.ActionType, Selector: "#answer", Text: "Ada Lovelace"})
		if err != nil {
			t.Fatal(err)
		}
		if res.OutcomeClass != guard.ClassHumanTyped || res.Content != guard.ContentHumanTyped {
			t.Fatalf("%s: %+v", name, res)
		}
	}
	seen := ext.seen()
	if strings.Contains(seen, "Ada Lovelace") || strings.Contains(seen, `"method":"type"`) {
		t.Fatalf("the extension was sent the guarded type:\n%s", seen)
	}
	if strings.Count(seen, `"method":"guard_field"`) != 2 || strings.Count(seen, `"method":"guard_focus"`) != 2 {
		t.Fatalf("expected a preflight and a focus per call:\n%s", seen)
	}
}

// provide: the operator's answer goes to the extension as guard_fill — in
// the hub role and through another process's hub — and the agent's text
// never does.
func TestLiveProvideFillsByReference(t *testing.T) {
	guardIsolate(t)
	t.Setenv(guard.FakeAskEnv, "fake:provide")
	t.Setenv(guard.FakeValueEnv, "078-05-1120")
	field := guard.Field{Guarded: true, Reason: "field:label-ssn", Question: "Social Security Number", Site: "acme.typeform.com"}
	hubSvc, ext, port := startHubWithExt(t, "0.9.1", guardedMethods, field)
	client := New(port)
	if err := client.EnsureReady(context.Background()); err != nil || client.role != roleClient {
		t.Fatalf("second service did not take the client role: %v", err)
	}
	for name, s := range map[string]*Service{"hub": hubSvc, "client": client} {
		res, err := s.Execute(context.Background(), wire.Action{Type: wire.ActionType, Selector: "#answer", Text: "Ada Lovelace"})
		if err != nil {
			t.Fatal(err)
		}
		if res.OutcomeClass != guard.ClassFilledByHuman || res.Content != guard.ContentFilledByHuman ||
			!strings.Contains(res.Data, `"verified":"match"`) || !strings.Contains(res.Data, `"value_len":11`) {
			t.Fatalf("%s: %+v", name, res)
		}
		if b, _ := json.Marshal(res); strings.Contains(string(b), "078-05-1120") {
			t.Fatalf("%s: the operator's value is in the result: %s", name, b)
		}
	}
	seen := ext.seen()
	if strings.Contains(seen, "Ada Lovelace") || strings.Contains(seen, `"method":"type"`) {
		t.Fatalf("the agent's text reached the extension:\n%s", seen)
	}
	if strings.Count(seen, `"method":"guard_fill"`) != 2 || strings.Count(seen, "078-05-1120") != 2 {
		t.Fatalf("expected one guard_fill per call carrying the operator's value:\n%s", seen)
	}
}

// A raw /dispatch guard_fill (a client that skips the gate) is refused at
// the hub without an admission the guard claimed for that field.
func TestLiveRawGuardFillNeedsAnAdmission(t *testing.T) {
	guardIsolate(t)
	svc, ext, _ := startHubWithExt(t, "0.9.1", guardedMethods, guard.Field{Site: "x"})
	msg := svc.hub.guardDispatch(context.Background(), "guard_fill", map[string]any{
		"selector": "input[type=password]", "text": "hunter2", "admit": "0123456789abcdef0123456789abcdef",
	})
	if !strings.Contains(msg, "refusing guard_fill") {
		t.Fatalf("raw guard_fill accepted: %q", msg)
	}
	if strings.Contains(ext.seen(), "hunter2") {
		t.Fatal("the raw guard_fill reached the extension")
	}
}

// An unguarded field gets the type as before.
func TestLiveUnguardedTypePassesThrough(t *testing.T) {
	guardIsolate(t)
	svc, ext, _ := startHubWithExt(t, "0.9.1", guardedMethods, guard.Field{Site: "shop.example"})
	res, err := svc.Execute(context.Background(), wire.Action{Type: wire.ActionType, Selector: "#order", Text: "ORD-2026-001234"})
	if err != nil || !res.Success {
		t.Fatalf("%v %+v", err, res)
	}
	if !strings.Contains(ext.seen(), "ORD-2026-001234") {
		t.Fatal("the ordinary value never reached the extension")
	}
}

// An extension without the field check (pre-0.9.0) is refused LOUDLY for
// writes — never a silent pass-through — while reads keep working.
func TestLiveOldExtensionRefusesWritesLoudly(t *testing.T) {
	guardIsolate(t)
	old := []string{"navigate", "type", "select", "keyboard_press", "extract"}
	svc, ext, port := startHubWithExt(t, "0.8.1", old, guard.Field{})
	client := New(port)
	_ = client.EnsureReady(context.Background())
	for _, s := range []*Service{svc, client} {
		for _, a := range []wire.Action{
			{Type: wire.ActionType, Selector: "#q", Text: "hello"},
			{Type: wire.ActionSelect, Selector: "#q", Text: "Yes"},
			{Type: wire.ActionKeyboardPress, Key: "a"},
		} {
			res, _ := s.Execute(context.Background(), a)
			if res.Success || !(strings.Contains(res.Error, "guard_field") || strings.Contains(res.Error, "field check")) {
				t.Errorf("%s on an old extension was not refused loudly: %+v", a.Type, res)
			}
		}
	}
	if seen := ext.seen(); strings.Contains(seen, `"method":"type"`) || strings.Contains(seen, `"method":"select"`) {
		t.Fatalf("a write reached an extension without the field check:\n%s", seen)
	}
	if res, _ := svc.Execute(context.Background(), wire.Action{Type: wire.ActionNavigate, URL: "https://x.test"}); !res.Success {
		t.Fatalf("navigate broke on an old extension: %+v", res)
	}
}

// extSetup scripts the fake extension's page per test (by test name).
var extSetup = map[string]struct {
	page      func(string) string
	noVerdict bool
}{
	"TestLiveProvideFillMismatch":  {page: func(s string) string { return strings.ReplaceAll(s, "-", "")[:5] }},
	"TestLiveProvideNeedsAVerdict": {noVerdict: true},
}

// A page that truncates or rewrites the operator's answer makes the fill a
// FAILURE — guard:fill-mismatch, exit 6 — reported as a verdict and two
// lengths, in both roles; neither the value nor a hash of it comes back.
func TestLiveProvideFillMismatch(t *testing.T) {
	guardIsolate(t)
	t.Setenv(guard.FakeAskEnv, "fake:provide")
	t.Setenv(guard.FakeValueEnv, "078-05-1120")
	field := guard.Field{Guarded: true, Reason: "field:label-ssn", Question: "Social Security Number", Site: "acme.typeform.com"}
	hubSvc, _, port := startHubWithExt(t, "0.9.2", guardedMethods, field)
	client := New(port)
	if err := client.EnsureReady(context.Background()); err != nil || client.role != roleClient {
		t.Fatalf("second service did not take the client role: %v", err)
	}
	for name, s := range map[string]*Service{"hub": hubSvc, "client": client} {
		res, err := s.Execute(context.Background(), wire.Action{Type: wire.ActionType, Selector: "#answer", Text: "Ada Lovelace"})
		if err != nil {
			t.Fatal(err)
		}
		if code, _ := guard.ExitCode(res); res.OutcomeClass != guard.ClassFillMismatch || res.Content != guard.ContentFillMismatch ||
			res.Success || code != guard.ExitFillMismatch {
			t.Fatalf("%s: %+v", name, res)
		}
		if !strings.Contains(res.Data, `"verified":"mismatch"`) || !strings.Contains(res.Data, `"field_len":5`) || !strings.Contains(res.Data, `"value_len":11`) {
			t.Fatalf("%s: verification = %s", name, res.Data)
		}
		b, _ := json.Marshal(res)
		for _, v := range []string{"078-05-1120", "07805"} {
			if strings.Contains(string(b), v) {
				t.Fatalf("%s: a value is in the result: %s", name, b)
			}
		}
	}
}

// An extension that fills without a verdict cannot prove the fill: it stops
// (fill-failed) rather than report guard:filled-by-human.
func TestLiveProvideNeedsAVerdict(t *testing.T) {
	guardIsolate(t)
	t.Setenv(guard.FakeAskEnv, "fake:provide")
	t.Setenv(guard.FakeValueEnv, "078-05-1120")
	field := guard.Field{Guarded: true, Reason: "field:label-ssn", Question: "Social Security Number", Site: "acme.typeform.com"}
	svc, _, _ := startHubWithExt(t, "0.9.1", guardedMethods, field)
	res, err := svc.Execute(context.Background(), wire.Action{Type: wire.ActionType, Selector: "#answer", Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.OutcomeClass != guard.ClassStopped || !strings.Contains(res.Data, "fill-failed") {
		t.Fatalf("an unverified fill was reported: %+v", res)
	}
}
