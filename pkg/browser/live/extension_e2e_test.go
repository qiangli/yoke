package live

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"

	"github.com/qiangli/yoke/pkg/browser/guard"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

// End to end through the REAL extension: a hub Service, the shipped
// extension (background.js + guard.js + page.js, unpacked) loaded into a
// headless Chrome and connected to that hub over its websocket, and a
// Typeform-style fixture in the driven tab. Service.Execute runs the guard,
// the fake asker answers "provide", and guard_fill goes hub → background.js
// guardFill → page.js guardFill/guardVerify → back to FillAdmitted, the one
// path the page tests (page.js alone) and the hub tests (a scripted fake
// extension) do not cover between them.
//
// Opt-in like the other Chrome tests:
//
//	BASHY_BROWSER_LIVE=1 go test ./pkg/browser/live -run TestExtensionE2E
//
// Needs a Chrome that still honours --load-extension: Chrome for Testing /
// Chromium (branded Chrome 137+ ignores it). BASHY_BROWSER_EXT_CHROME names
// one; otherwise a Playwright Chrome for Testing is looked up, then the
// chromedp default.

const e2eValue = "078-05-1120"

func e2eChrome(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("BASHY_BROWSER_EXT_CHROME"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	pats := []string{
		filepath.Join(home, "Library/Caches/ms-playwright/chromium-*/chrome-mac*/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"),
		filepath.Join(home, ".cache/ms-playwright/chromium-*/chrome-linux*/chrome"),
	}
	for _, pat := range pats {
		if m, _ := filepath.Glob(pat); len(m) > 0 {
			return m[len(m)-1]
		}
	}
	return ""
}

// startExtensionE2E starts a hub, loads the real extension against it and
// opens path of fixtures in the tab the extension drives.
func startExtensionE2E(t *testing.T, fixtures map[string]string, path string) (*Service, context.Context) {
	t.Helper()
	if os.Getenv("BASHY_BROWSER_LIVE") == "" {
		t.Skip("live Chrome test disabled; set BASHY_BROWSER_LIVE=1 to run (launches a headless Chrome with the extension)")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	// Every hub log line of the run is kept, and must not carry the value.
	var logMu sync.Mutex
	var logs strings.Builder
	SetLogger(NewWriterLogger(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logs.Write(p)
	})))
	t.Cleanup(func() {
		SetLogger(nil)
		logMu.Lock()
		defer logMu.Unlock()
		noValueOrDigest(t, "the hub log", logs.String())
	})
	svc := New(port)
	if err := svc.EnsureReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	waitHealthy(t, port)

	// The shipped extension, unchanged but for one appended line that
	// connects it to this hub (the popup's "connect" does the same).
	dir := filepath.Join(t.TempDir(), "ext")
	if _, err := ExtractExtension(dir); err != nil {
		t.Fatal(err)
	}
	bg := filepath.Join(dir, "background.js")
	src, err := os.ReadFile(bg)
	if err != nil {
		t.Fatal(err)
	}
	boot := fmt.Sprintf("\n;connectedPort = %d; wantConnected = true; openWS();\n", port)
	if err := os.WriteFile(bg, append(src, boot...), 0o644); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	for p, body := range fixtures {
		body := body
		mux.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, body)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	opts := []chromedp.ExecAllocatorOption{
		chromedp.NoFirstRun, chromedp.NoDefaultBrowserCheck,
		chromedp.Flag("headless", "new"),
		chromedp.WindowSize(1200, 900),
		chromedp.UserDataDir(filepath.Join(t.TempDir(), "profile")),
		chromedp.Flag("disable-extensions-except", dir),
		chromedp.Flag("load-extension", dir),
		chromedp.Flag("disable-features", "DisableLoadExtensionCommandLineSwitch"),
	}
	if exe := e2eChrome(t); exe != "" {
		opts = append(opts, chromedp.ExecPath(exe))
	}
	alloc, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancelCtx := chromedp.NewContext(alloc)
	t.Cleanup(func() { cancelCtx(); cancelAlloc() })
	// The first Run starts the browser and binds its life to ctx: a timeout
	// context here would close Chrome the moment it is cancelled.
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+path)); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); svc.hub.ExtVersion() == ""; {
		if time.Now().After(deadline) {
			t.Fatal("the real extension never connected to the hub (does this Chrome honour --load-extension? set BASHY_BROWSER_EXT_CHROME)")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got, want := svc.hub.ExtVersion(), EmbeddedVersion(); got != want {
		t.Fatalf("connected extension v%s, shipped v%s", got, want)
	}
	return svc, ctx
}

// restartWorkerAndSwitchTab plays what can happen while the operator reads
// the dialog: the extension's worker is stopped (Chrome's idle stop of an
// MV3 worker; a hot reload does the same) and comes back without its driven
// tab, and another tab is now the active one. The worker is woken the way
// a person would wake it: by opening the extension's popup page — in a new
// tab, which becomes the active one. It returns before the worker has
// reconnected.
func restartWorkerAndSwitchTab(t *testing.T, ctx context.Context, svc *Service) {
	t.Helper()
	svc.hub.mu.Lock()
	old := svc.hub.conn
	svc.hub.mu.Unlock()
	worker, base := ourWorker(t, ctx)
	c := chromedp.FromContext(ctx)
	if err := target.CloseTarget(worker).Do(cdp.WithExecutor(ctx, c.Browser)); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		svc.hub.mu.Lock()
		gone := svc.hub.conn != old
		svc.hub.mu.Unlock()
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stopped worker's websocket never closed")
		}
	}
	id, err := target.CreateTarget(base + "popup.html").Do(cdp.WithExecutor(ctx, c.Browser))
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ActivateTarget(id).Do(cdp.WithExecutor(ctx, c.Browser)); err != nil {
		t.Fatal(err)
	}
	// Return at once: the operator's answer arrives while the worker is
	// still coming back.
}

// ourWorker finds the extension's service worker among the browser's
// (Chrome runs component extensions too): the one whose manifest is ours.
func ourWorker(t *testing.T, ctx context.Context) (target.ID, string) {
	t.Helper()
	ts, err := chromedp.Targets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ti := range ts {
		if ti.Type != "service_worker" || !strings.HasPrefix(ti.URL, "chrome-extension://") {
			continue
		}
		wctx, cancel := chromedp.NewContext(ctx, chromedp.WithTargetID(ti.TargetID))
		var name string
		err := chromedp.Run(wctx, chromedp.Evaluate(`chrome.runtime.getManifest().name`, &name))
		cancel()
		if err == nil && name == "ycode-live" {
			return ti.TargetID, ti.URL[:strings.LastIndex(ti.URL, "/")+1]
		}
	}
	t.Fatal("the extension's service worker is not running")
	return "", ""
}

// slowOperator answers provide after during() ran — the window in which a
// real operator reads the dialog and types.
type slowOperator struct {
	during func()
	value  string
	mu     sync.Mutex
	vals   map[string][]byte
}

func (o *slowOperator) Ask(ctx context.Context, q guard.GuardAsk) (guard.GuardAnswer, error) {
	o.during()
	id := fmt.Sprintf("%032x", time.Now().UnixNano())
	o.mu.Lock()
	o.vals[id] = []byte(o.value)
	o.mu.Unlock()
	return guard.GuardAnswer{Choice: guard.ChoiceProvide, AskID: id, Via: "operator"}, nil
}

func (o *slowOperator) Done(context.Context, guard.GuardAsk) (bool, error) { return true, nil }

func (o *slowOperator) take(id string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	v, ok := o.vals[id]
	delete(o.vals, id)
	if !ok {
		return nil, fmt.Errorf("no value")
	}
	return v, nil
}

// noValueOrDigest fails when out carries the value, a separator-free form
// of it, or a hex digest (full or truncated) of it.
func noValueOrDigest(t *testing.T, what, out string) {
	t.Helper()
	s256, s1, m5 := sha256.Sum256([]byte(e2eValue)), sha1.Sum([]byte(e2eValue)), md5.Sum([]byte(e2eValue))
	for _, leak := range []string{e2eValue, strings.ReplaceAll(e2eValue, "-", ""),
		hex.EncodeToString(s256[:]), hex.EncodeToString(s256[:8]), hex.EncodeToString(s1[:]), hex.EncodeToString(m5[:])} {
		if strings.Contains(out, leak) {
			t.Fatalf("%s carries the value or a digest of it: %s", what, out)
		}
	}
}

func e2eProvide(t *testing.T, svc *Service) *wire.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := svc.Execute(ctx, wire.Action{Type: wire.ActionType, Selector: "input[aria-label=ssn]", Text: "Ada Lovelace"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	noValueOrDigest(t, "the result", string(b))
	return res
}

func TestExtensionE2EFrameSelectorAndStaleIndex(t *testing.T) {
	fixtures := map[string]string{
		"/form":  `<!doctype html><label for="answer">Top answer</label><input id="answer" name="top"><iframe src="/child"></iframe>`,
		"/child": `<!doctype html><label for="answer">Child answer</label><input id="answer" name="child">`,
	}
	svc, browserCtx := startExtensionE2E(t, fixtures, "/form")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ex, err := svc.Execute(ctx, wire.Action{Type: wire.ActionExtract})
	if err != nil || ex == nil || !ex.Success {
		t.Fatalf("extract: %v %v", ex, err)
	}
	var data struct {
		Elements []struct {
			Index    int    `json:"index"`
			Name     string `json:"name"`
			StableID string `json:"stable_id"`
			Selector string `json:"selector"`
			Label    string `json:"label"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(ex.Data), &data); err != nil {
		t.Fatal(err)
	}
	var childIndex int
	var childSelector string
	for _, e := range data.Elements {
		if e.Name == "child" {
			childIndex, childSelector = e.Index, e.Selector
			if !strings.HasPrefix(e.StableID, "frame:") || e.Label != "Child answer" {
				t.Fatalf("child metadata: %+v", e)
			}
		}
	}
	if childIndex == 0 || childSelector == "" {
		t.Fatalf("child absent: %s", ex.Data)
	}
	res, err := svc.Execute(ctx, wire.Action{Type: wire.ActionType, Selector: childSelector, Text: "child value"})
	if err != nil || res == nil || !res.Success {
		t.Fatalf("frame-qualified type: %v %v", res, err)
	}
	var got string
	if err := chromedp.Run(browserCtx, chromedp.Evaluate(`document.querySelector('iframe').contentDocument.getElementById('answer').value`, &got)); err != nil || got != "child value" {
		t.Fatalf("child value %q: %v", got, err)
	}
	if err := chromedp.Run(browserCtx, chromedp.Evaluate(`document.querySelector('iframe').outerHTML = '<iframe src="/child"></iframe>'`, nil)); err != nil {
		t.Fatal(err)
	}
	res, err = svc.Execute(ctx, wire.Action{Type: wire.ActionType, ElementID: childIndex, Text: "wrong"})
	if err == nil && res != nil && res.Success {
		t.Fatalf("stale frame index wrote: %+v", res)
	}
	if res != nil && !strings.Contains(res.Error, "guard:fill-mismatch") && err == nil {
		t.Fatalf("missing drift reason: %+v", res)
	}
}

func e2eData(t *testing.T, res *wire.Result) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal([]byte(res.Data), &d); err != nil {
		t.Fatalf("data %q: %v", res.Data, err)
	}
	return d
}

func e2eSSNScreen(input string) map[string]string {
	return map[string]string{"/ssn": typeformScreen("Social Security Number", input)}
}

// provide → guard_fill through the real extension: the field holds the
// answer and the verdict is a match with both lengths.
func TestExtensionE2EGuardFillMatch(t *testing.T) {
	guardIsolate(t)
	t.Setenv(guard.FakeAskEnv, "fake:provide")
	t.Setenv(guard.FakeValueEnv, e2eValue)
	svc, _ := startExtensionE2E(t, e2eSSNScreen(`<input type="text" aria-label="ssn" placeholder="Type your answer here...">`), "/ssn")
	client := New(svc.port)
	if err := client.EnsureReady(context.Background()); err != nil || client.role != roleClient {
		t.Fatalf("client role: %v", err)
	}
	for name, s := range map[string]*Service{"client": client, "hub": svc} {
		res := e2eProvide(t, s)
		if res.OutcomeClass != guard.ClassFilledByHuman || res.Content != guard.ContentFilledByHuman {
			t.Fatalf("%s: outcome = %+v", name, res)
		}
		if code, _ := guard.ExitCode(res); code != guard.ExitFilledByHuman {
			t.Fatalf("%s: exit %d", name, code)
		}
		d := e2eData(t, res)
		if d["verified"] != guard.VerifiedMatch || d["field_len"] != float64(11) || d["value_len"] != float64(11) {
			t.Fatalf("%s: data = %v", name, d)
		}
	}
}

// A page that rewrites the answer (strips the dashes): the fill is a
// mismatch, guard:fill-mismatch exit 6, with the lengths and nothing else.
func TestExtensionE2EGuardFillMismatch(t *testing.T) {
	guardIsolate(t)
	t.Setenv(guard.FakeAskEnv, "fake:provide")
	t.Setenv(guard.FakeValueEnv, e2eValue)
	svc, _ := startExtensionE2E(t, e2eSSNScreen(
		`<input type="text" aria-label="ssn" oninput="this.value=this.value.replace(/-/g,'')">`), "/ssn")
	client := New(svc.port)
	if err := client.EnsureReady(context.Background()); err != nil || client.role != roleClient {
		t.Fatalf("client role: %v", err)
	}
	for name, s := range map[string]*Service{"client": client, "hub": svc} {
		res := e2eProvide(t, s)
		if res.OutcomeClass != guard.ClassFillMismatch || res.Content != guard.ContentFillMismatch || res.Success {
			t.Fatalf("%s: outcome = %+v", name, res)
		}
		if code, _ := guard.ExitCode(res); code != 6 {
			t.Fatalf("%s: exit %d", name, code)
		}
		d := e2eData(t, res)
		if d["verified"] != guard.VerifiedMismatch || d["field_len"] != float64(9) || d["value_len"] != float64(11) {
			t.Fatalf("%s: data = %v", name, d)
		}
	}
}

// The worker restarts and another tab becomes active while the operator
// answers. The fill must still land in the tab whose field was checked,
// with its verdict — 0.9.2 filled whatever tab was active (or nothing) and
// the answer ended guard:stopped (fill-failed), the run 662 live failure.
func TestExtensionE2EGuardFillAfterWorkerRestart(t *testing.T) {
	guardIsolate(t)
	t.Setenv(guard.FakeAskEnv, "")
	var svc *Service
	var ctx context.Context
	op := &slowOperator{value: e2eValue, vals: map[string][]byte{}}
	op.during = func() { restartWorkerAndSwitchTab(t, ctx, svc) }
	guard.SetHumanAsker(op)
	guard.SetValueSource(op.take)
	t.Cleanup(func() { guard.SetHumanAsker(nil); guard.SetValueSource(nil) })
	svc, ctx = startExtensionE2E(t, e2eSSNScreen(`<input type="text" aria-label="ssn">`), "/ssn")
	res := e2eProvide(t, svc)
	if res.OutcomeClass != guard.ClassFilledByHuman {
		t.Fatalf("outcome = %+v", res)
	}
	d := e2eData(t, res)
	if d["verified"] != guard.VerifiedMatch || d["field_len"] != float64(11) || d["value_len"] != float64(11) {
		t.Fatalf("data = %v", d)
	}
}

// Live evaluate never hands back a password input's raw value, directly or
// inside a structured object; ordinary visible text and structure survive.
func TestExtensionE2EEvaluateSuppressesPasswordValues(t *testing.T) {
	guardIsolate(t)
	page := `<!doctype html><html><body><label>Password <input type="password" id="pw" value="hunter2-initial"></label>
<label>Nickname <input id="nick" value="ada"></label><p id="msg">hello visible</p></body></html>`
	svc, _ := startExtensionE2E(t, map[string]string{"/pw": page}, "/pw")
	client := New(svc.port)
	if err := client.EnsureReady(context.Background()); err != nil || client.role != roleClient {
		t.Fatalf("client role: %v", err)
	}
	for name, s := range map[string]*Service{"client": client, "hub": svc} {
		eval := func(script string) string {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			res, err := s.Execute(ctx, wire.Action{Type: wire.ActionEvaluate, Script: script})
			if err != nil || res == nil || res.Error != "" {
				t.Fatalf("%s %q: %v %+v", name, script, err, res)
			}
			return res.Data
		}
		if d := eval(`document.querySelector('#pw').value`); d != guard.PasswordMark {
			t.Fatalf("%s: direct value = %q", name, d)
		}
		d := eval(`({pw: document.querySelector('#pw').value, list: [document.querySelector('#pw').value], nick: document.querySelector('#nick').value, msg: document.querySelector('#msg').textContent})`)
		var obj map[string]any
		if err := json.Unmarshal([]byte(d), &obj); err != nil || obj["nick"] != "ada" || obj["msg"] != "hello visible" || obj["pw"] != guard.PasswordMark {
			t.Fatalf("%s: structured = %s (%v)", name, d, err)
		}
		if strings.Contains(d, "hunter2") {
			t.Fatalf("%s: leaked: %s", name, d)
		}
	}
}

func TestExtensionE2EEvaluateSuppressesIframePasswordValues(t *testing.T) {
	guardIsolate(t)
	outer := `<!doctype html><body><p id="msg">outer visible</p><iframe id="f" src="/inner"></iframe></body>`
	inner := `<!doctype html><body><input type="password" id="pw" value="framepw-9981"><p id="t">inner text</p></body>`
	svc, _ := startExtensionE2E(t, map[string]string{"/outer": outer, "/inner": inner}, "/outer")
	time.Sleep(time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := svc.Execute(ctx, wire.Action{Type: wire.ActionEvaluate, Script: `({pw: document.getElementById('f').contentDocument.getElementById('pw').value, msg: document.getElementById('msg').textContent, inner: document.getElementById('f').contentDocument.getElementById('t').textContent})`})
	if err != nil || res == nil || res.Error != "" {
		t.Fatalf("%v %+v", err, res)
	}
	if strings.Contains(res.Data, "framepw") || !strings.Contains(res.Data, guard.PasswordMark) ||
		!strings.Contains(res.Data, "outer visible") || !strings.Contains(res.Data, "inner text") {
		t.Fatalf("iframe password not suppressed: %s", res.Data)
	}
}

const selectorE2EPage = `<!doctype html><body>
<label for="who">Who</label><input id="who" name="who">
<input id="other" name="other" placeholder="other">
<input class="dup" id="d1"><input class="dup" id="d2">
<input id="hid" style="display:none">
<div id="host"></div><iframe src="/selchild"></iframe>
<script>
window.log = [];
document.addEventListener('keydown', e => window.log.push('key:' + e.key + ':' + (e.target.id || e.target.tagName)));
document.getElementById('host').attachShadow({mode: 'open'}).innerHTML = '<button id="sb">Shadow go</button>';
document.getElementById('host').shadowRoot.getElementById('sb').addEventListener('ping', e => window.log.push('shadow:' + JSON.stringify(e.detail)));
setTimeout(() => { const b = document.createElement('button'); b.id = 'late'; b.textContent = 'Late'; document.body.appendChild(b); }, 400);
</script></body>`

const selectorE2EChild = `<!doctype html><body><label for="cin">Child field</label><input id="cin">
<button id="cb">Child go</button>
<script>
document.addEventListener('keydown', e => { parent.log.push('childkey:' + e.key + ':' + (e.target.id || e.target.tagName)); });
document.getElementById('cb').addEventListener('ping', e => parent.log.push('child:' + (e.detail && e.detail.n)));
</script></body>`

func e2eLog(t *testing.T, ctx context.Context) string {
	t.Helper()
	var out string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`JSON.stringify(window.log)`, &out)); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestExtensionE2ESelectorWaitDispatchKeyboard(t *testing.T) {
	guardIsolate(t)
	svc, browserCtx := startExtensionE2E(t, map[string]string{"/sel": selectorE2EPage, "/selchild": selectorE2EChild}, "/sel")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	run := func(a wire.Action) (*wire.Result, error) { return svc.Execute(ctx, a) }
	ok := func(what string, a wire.Action) *wire.Result {
		t.Helper()
		res, err := run(a)
		if err != nil || res == nil || !res.Success {
			t.Fatalf("%s: %+v %v", what, res, err)
		}
		return res
	}
	fails := func(what, want string, a wire.Action) {
		t.Helper()
		res, err := run(a)
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if res != nil {
			if res.Success {
				t.Fatalf("%s: unexpectedly succeeded: %+v", what, res)
			}
			msg += res.Error
		}
		if !strings.Contains(msg, want) {
			t.Fatalf("%s: error %q lacks %q", what, msg, want)
		}
	}
	if err := chromedp.Run(browserCtx, chromedp.WaitReady("iframe")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	// XPath / label / text / shadow / frame selectors in wait_for_selector.
	ok("xpath wait late", wire.Action{Type: wire.ActionWaitForSelector, Selector: "xpath=//button[@id='late']", State: "visible", TimeoutMs: 5000})
	ok("label wait", wire.Action{Type: wire.ActionWaitForSelector, Selector: "label=Who", State: "visible", TimeoutMs: 3000})
	ok("child label wait", wire.Action{Type: wire.ActionWaitForSelector, Selector: "label=Child field", State: "attached", TimeoutMs: 3000})
	ok("shadow css wait", wire.Action{Type: wire.ActionWaitForSelector, Selector: "#sb", State: "visible", TimeoutMs: 3000})
	ok("text wait", wire.Action{Type: wire.ActionWaitForSelector, Selector: "text=Shadow go", State: "attached", TimeoutMs: 3000})
	ok("detached", wire.Action{Type: wire.ActionWaitForSelector, Selector: "xpath=//nothing", State: "detached", TimeoutMs: 2000})
	fails("missing wait", "timeout", wire.Action{Type: wire.ActionWaitForSelector, Selector: "xpath=//nothing", State: "attached", TimeoutMs: 600})
	fails("hidden wait", "timeout", wire.Action{Type: wire.ActionWaitForSelector, Selector: "xpath=//input[@id='hid']", State: "visible", TimeoutMs: 600})

	// dispatch_event reaches page listeners (with detail) through open
	// shadow roots and same-origin frames.
	ok("dispatch shadow", wire.Action{Type: wire.ActionDispatchEvent, Event: "ping", Detail: `{"a":1}`, Selector: "xpath=//*[@id='sb']"})
	ok("dispatch frame", wire.Action{Type: wire.ActionDispatchEvent, Event: "ping", Detail: `{"n":7}`, Selector: "text=Child go"})
	fails("dispatch missing", "no element matches", wire.Action{Type: wire.ActionDispatchEvent, Event: "ping", Selector: "xpath=//nothing"})
	if got := e2eLog(t, browserCtx); got != `["shadow:{\"a\":1}","child:7"]` {
		t.Fatalf("dispatch log: %s", got)
	}

	// keyboard_press: one visible target gets the key; missing, hidden,
	// ambiguous targets fail before any key event, even with another
	// element focused.
	if err := chromedp.Run(browserCtx, chromedp.Evaluate(`window.log = []; document.getElementById('other').focus()`, nil)); err != nil {
		t.Fatal(err)
	}
	for what, sel := range map[string]string{
		"missing":   "xpath=//nothing",
		"hidden":    "xpath=//input[@id='hid']",
		"ambiguous": "input.dup",
		"ambig-xp":  "xpath=//input[@class='dup']",
	} {
		fails("keyboard "+what, "keyboard_press", wire.Action{Type: wire.ActionKeyboardPress, Key: "x", Selector: sel})
	}
	if got := e2eLog(t, browserCtx); got != `[]` {
		t.Fatalf("a refused keyboard target still produced events: %s", got)
	}
	var active string
	if err := chromedp.Run(browserCtx, chromedp.Evaluate(`document.activeElement.id`, &active)); err != nil || active != "other" {
		t.Fatalf("focus moved on refusal: %q %v", active, err)
	}
	ok("keyboard label", wire.Action{Type: wire.ActionKeyboardPress, Key: "x", Selector: "label=Who"})
	ok("keyboard child label", wire.Action{Type: wire.ActionKeyboardPress, Key: "y", Selector: "label=Child field"})
	if got := e2eLog(t, browserCtx); !strings.Contains(got, "key:x:who") || !strings.Contains(got, "childkey:y:cin") {
		t.Fatalf("keyboard log: %s", got)
	}
}
