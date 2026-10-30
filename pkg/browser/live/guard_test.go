package live

import (
	"context"
	"encoding/json"
	"errors"
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
	// fillErr makes guard_fill answer this extension error instead.
	fillErr string
}

// fakeTab is the tab id the fake extension stamps on its guard_field answer.
const fakeTab = 41

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
		ext.page, ext.noVerdict, ext.fillErr = ver.page, ver.noVerdict, ver.fillErr
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
			var extErr string
			switch req.Method {
			case "guard_field":
				var f map[string]any
				_ = json.Unmarshal(mustJSON(ext.field), &f)
				f["tab_id"] = fakeTab
				result = mustJSON(f)
			case "guard_fill":
				if ext.fillErr != "" {
					extErr = ext.fillErr
					break
				}
				result = ext.fill(req.Params)
			default:
				result = mustJSON(extResult{Content: req.Method + "-done"})
			}
			resp, _ := json.Marshal(wsResponse{ID: req.ID, Result: result, Error: extErr})
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
	if strings.Count(seen, `"method":"guard_field"`) != 2 || strings.Count(seen, `"method":"guard_focus"`) != 4 || strings.Count(seen, `"release":true`) != 2 {
		t.Fatalf("expected a preflight, a focus and a release per call:\n%s", seen)
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
	fillErr   string
}{
	"TestLiveProvideFillMismatch":  {page: func(s string) string { return strings.ReplaceAll(s, "-", "")[:5] }},
	"TestLiveProvideNeedsAVerdict": {noVerdict: true},
	// An extension error that quotes the value: only the code may pass.
	"TestLiveFillFailureLogsAValueFreeCause": {fillErr: `guard_fill: wanted "078-05-1120" (cause: not-found)`},
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
	if res.OutcomeClass != guard.ClassStopped || !strings.Contains(res.Data, "fill-failed") || !strings.Contains(res.Data, `"cause":"no-verdict"`) {
		t.Fatalf("an unverified fill was reported: %+v", res)
	}
}

// The guard's follow-ups go to the tab the field check ran in (tab_id from
// guard_field's answer), in both roles: a worker that restarted while the
// operator answered no longer knows its driven tab, and the active tab may
// be another page.
func TestLiveGuardFollowUpsArePinnedToTheCheckedTab(t *testing.T) {
	guardIsolate(t)
	field := guard.Field{Guarded: true, Reason: "field:label-ssn", Question: "Social Security Number", Site: "acme.typeform.com"}
	for _, ans := range []string{"fake:provide", "fake:self"} {
		t.Setenv(guard.FakeAskEnv, ans)
		hubSvc, ext, port := startHubWithExt(t, "0.9.3", guardedMethods, field)
		client := New(port)
		if err := client.EnsureReady(context.Background()); err != nil || client.role != roleClient {
			t.Fatalf("second service did not take the client role: %v", err)
		}
		for _, s := range []*Service{hubSvc, client} {
			if _, err := s.Execute(context.Background(), wire.Action{Type: wire.ActionType, Selector: "#answer", Text: "x"}); err != nil {
				t.Fatal(err)
			}
		}
		method := map[string]string{"fake:provide": fillMethod, "fake:self": focusMethod}[ans]
		pinned := 0
		for _, f := range strings.Split(ext.seen(), "\n") {
			var req wsRequest
			if json.Unmarshal([]byte(f), &req) != nil || req.Method != method {
				continue
			}
			if req.Params["tab_id"] != float64(fakeTab) {
				t.Fatalf("%s: %s not pinned to the checked tab: %s", ans, method, f)
			}
			if req.Params["release"] == true {
				continue
			}
			if req.Params["tab_id"] != float64(fakeTab) {
				t.Fatalf("%s: %s not pinned to the checked tab: %s", ans, method, f)
			}
			pinned++
		}
		if pinned != 2 {
			t.Fatalf("%s: expected one pinned %s per role, saw %d:\n%s", ans, method, pinned, ext.seen())
		}
	}
}

// A failed fill reports a VALUE-FREE cause: data.cause on the outcome, and
// the hub's log line (in the hub process for a /dispatch client). An
// extension message that quotes the value contributes only its code.
func TestLiveFillFailureLogsAValueFreeCause(t *testing.T) {
	guardIsolate(t)
	t.Setenv(guard.FakeAskEnv, "fake:provide")
	t.Setenv(guard.FakeValueEnv, "078-05-1120")
	var log strings.Builder
	var logMu sync.Mutex
	SetLogger(NewWriterLogger(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return log.Write(p)
	})))
	t.Cleanup(func() { SetLogger(nil) })
	field := guard.Field{Guarded: true, Reason: "field:label-ssn", Question: "Social Security Number", Site: "acme.typeform.com"}
	hubSvc, _, port := startHubWithExt(t, "0.9.3", guardedMethods, field)
	client := New(port)
	if err := client.EnsureReady(context.Background()); err != nil || client.role != roleClient {
		t.Fatalf("second service did not take the client role: %v", err)
	}
	for name, s := range map[string]*Service{"hub": hubSvc, "client": client} {
		res, err := s.Execute(context.Background(), wire.Action{Type: wire.ActionType, Selector: "#answer", Text: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if res.OutcomeClass != guard.ClassStopped || !strings.Contains(res.Data, `"via":"fill-failed"`) || !strings.Contains(res.Data, `"cause":"not-found"`) {
			t.Fatalf("%s: %+v", name, res)
		}
		if b, _ := json.Marshal(res); strings.Contains(string(b), "078-05-1120") {
			t.Fatalf("%s: the value is in the result: %s", name, b)
		}
	}
	logMu.Lock()
	out := log.String()
	logMu.Unlock()
	if strings.Count(out, "guard_fill failed") != 3 || !strings.Contains(out, "cause=not-found") || !strings.Contains(out, "via=dispatch") {
		t.Fatalf("expected the hub role, the client and the hub's /dispatch to log the cause:\n%s", out)
	}
	if strings.Contains(out, "078-05-1120") || strings.Contains(out, "wanted") {
		t.Fatalf("the log carries the extension's message:\n%s", out)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// fillCause: only fixed codes, whatever the error text says.
func TestFillCauseCodes(t *testing.T) {
	verdict := mustJSON(map[string]any{"verified": "match", "field_len": 3, "value_len": 3})
	_, noVerdict := fillVerdict(mustJSON(extResult{Content: "filled"}))
	_, unreadable := fillVerdict(json.RawMessage(`"filled"`))
	cases := []struct {
		raw       json.RawMessage
		err, verr error
		want      string
	}{
		{verdict, nil, nil, ""},
		{nil, errors.New("live: refusing guard_fill: guard: guard_fill not admitted for this field"), nil, "refused"},
		{nil, errors.New(notConnectedError("")), nil, "not-connected"},
		{nil, errors.New("extension disconnected"), nil, "not-connected"},
		{nil, context.DeadlineExceeded, nil, "timeout"},
		{nil, errors.New("guard: the tab the field was checked in is gone (cause: tab-gone)"), nil, "tab-gone"},
		{nil, errors.New("guard_fill: the operator's answer could not be filled (cause: inject-failed)"), nil, "inject-failed"},
		{nil, errors.New("guard_fill: the operator's answer could not be filled"), nil, "ext-threw"},
		{nil, errors.New(`wanted "078-05-1120" (cause: 078-05-1120)`), nil, "ext-threw"},
		{nil, errors.New(`(cause: Ada Lovelace)`), nil, "ext-threw"},
		{nil, nil, noVerdict, "no-verdict"},
		{nil, nil, unreadable, "parse"},
	}
	for _, c := range cases {
		if got := fillCause(c.raw, c.err, c.verr); got != c.want {
			t.Errorf("fillCause(%v, %v) = %q, want %q", c.err, c.verr, got, c.want)
		}
	}
}

// A guard_fill sent while the extension is reconnecting (its worker was
// stopped or reloaded during the operator's dialog) waits for it rather
// than failing the operator's answer.
func TestHubGuardFillWaitsForAReconnect(t *testing.T) {
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
	go func() {
		time.Sleep(300 * time.Millisecond)
		conn, _, err := websocket.DefaultDialer.Dial("ws://"+svc.hub.addr+"/ws", nil)
		if err != nil {
			return
		}
		hello, _ := json.Marshal(wsResponse{Method: "_hello", Result: mustJSON(extHello{Version: "0.9.3", Methods: guardedMethods})})
		_ = conn.WriteMessage(websocket.TextMessage, hello)
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req wsRequest
			if json.Unmarshal(raw, &req) != nil || req.ID == 0 {
				continue
			}
			resp, _ := json.Marshal(wsResponse{ID: req.ID, Result: mustJSON(map[string]any{"verified": "match", "field_len": 1, "value_len": 1})})
			_ = conn.WriteMessage(websocket.TextMessage, resp)
		}
	}()
	resp, err := svc.hub.call(context.Background(), fillMethod, map[string]any{})
	if err != nil || resp.Error != "" {
		t.Fatalf("guard_fill did not wait for the extension: %v %q", err, resp.Error)
	}
	// Any other method still fails fast when nothing is connected.
	h := newHub(0)
	start := time.Now()
	if _, err := h.call(context.Background(), "extract", nil); err == nil || time.Since(start) > time.Second {
		t.Fatalf("extract without an extension: %v after %v", err, time.Since(start))
	}
}

// The hub pings the extension while connected, so a worker waiting on the
// operator is not stopped as idle.
func TestHubKeepAlivePings(t *testing.T) {
	old := HubPingInterval
	HubPingInterval = 30 * time.Millisecond
	t.Cleanup(func() { HubPingInterval = old })
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
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	pings := 0
	for pings < 2 {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("after %d pings: %v", pings, err)
		}
		var req wsRequest
		if json.Unmarshal(raw, &req) == nil && req.ID == 0 && req.Method == "_ping" {
			pings++
		}
	}
}
