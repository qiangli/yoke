package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// S536.4 page-core tests: honest click outcomes, CSP-safe text,
// ancestor-aware scroll and the stitch geometry run through the SAME
// isolated-world harness as the other page.js tests (newPageHarness in
// page_test.go), against fixture pages. The text fixture is served WITH
// a strict Content-Security-Policy header (no 'unsafe-eval'), the
// condition under which `eval` is refused and `text` must still answer.
//
// Red/green: run with BASHY_PAGE_JS pointing at the pre-S536.4 core
// (git show HEAD:page.js) and every test below fails — old click
// reports bare "clicked" with no effect, and text/scroll-target/shot
// functions do not exist.

// effectFixture exercises each click outcome: an inert button (nothing
// listens), a DOM changer, a dialog opener, and a same-page navigation.
const effectFixture = `<!doctype html><html><body>
<button id="inert">Do nothing</button>
<button id="changer" onclick="document.getElementById('out').textContent='changed'">Change</button>
<button id="dialoger" onclick="document.getElementById('dlg').showModal()">Open dialog</button>
<a id="hashlink" href="#there">Go there</a>
<p id="out">before</p>
<dialog id="dlg"><p>Hi</p></dialog>
<div id="there" style="margin-top:2000px">there</div>
<script>window.hit='';</script>
</body></html>`

func TestPageClickReportsObservedEffect(t *testing.T) {
	h := newPageHarness(t, map[string]string{"/e": effectFixture})

	cases := []struct {
		selector string
		effect   string
		check    string
		want     string
	}{
		{"#inert", "no-effect", "document.getElementById('out').textContent", "before"},
		{"#changer", "dom-changed", "document.getElementById('out').textContent", "changed"},
		{"#dialoger", "dialog", "document.querySelector('dialog[open]') ? 'open' : 'shut'", "open"},
		{"#hashlink", "navigated", "location.hash", "#there"},
	}
	for _, c := range cases {
		h.open("/e")
		res := h.call("click", map[string]any{"selector": c.selector})
		if res["error"] != nil {
			t.Fatalf("click %s: %v", c.selector, res["error"])
		}
		if res["content"] != "clicked: "+c.effect {
			t.Fatalf("click %s content = %v, want clicked: %s", c.selector, res["content"], c.effect)
		}
		var data struct {
			Matched string `json:"matched"`
			Effect  string `json:"effect"`
		}
		if err := json.Unmarshal([]byte(res["data"].(string)), &data); err != nil || data.Effect != c.effect {
			t.Fatalf("click %s data = %v (%v), want effect %s", c.selector, res["data"], err, c.effect)
		}
		var got string
		h.mainEval(c.check, &got)
		if got != c.want {
			t.Fatalf("click %s left page state %q, want %q", c.selector, got, c.want)
		}
	}

	h.open("/e")
	if res := h.call("click", map[string]any{"selector": "#missing"}); res["error"] == nil {
		t.Fatalf("a click that resolves nothing must be an error, got %v", res)
	} else if !strings.Contains(fmt.Sprint(res["error"]), "no element matches") {
		t.Fatalf("unresolved click names no reason: %v", res)
	}
}

// headerFixture serves each path with per-path response headers (the
// shared harness sends Content-Type only).
type headerFixture struct {
	body    string
	headers map[string]string
}

type headerHarness struct {
	t      *testing.T
	ctx    context.Context
	srv    *httptest.Server
	script string
}

func newHeaderHarness(t *testing.T, fixtures map[string]headerFixture) *headerHarness {
	t.Helper()
	if os.Getenv("BASHY_BROWSER_LIVE") == "" {
		t.Skip("live Chrome test disabled; set BASHY_BROWSER_LIVE=1 to run (launches a headless Chrome)")
	}
	script := extensionFile(t, "page.js")
	if alt := os.Getenv("BASHY_PAGE_JS"); alt != "" {
		b, err := os.ReadFile(alt)
		if err != nil {
			t.Fatal(err)
		}
		script = string(b)
	}
	script = extensionFile(t, "guard.js") + "\n" + script
	mux := http.NewServeMux()
	for path, f := range fixtures {
		f := f
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			for k, v := range f.headers {
				w.Header().Set(k, v)
			}
			fmt.Fprint(w, f.body)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.Headless, chromedp.WindowSize(1200, 900),
		chromedp.Flag("disable-features", "IsolateOrigins,site-per-process"),
		chromedp.Flag("disable-site-isolation-trials", true))
	alloc, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancelCtx := chromedp.NewContext(alloc)
	ctx, cancelTimeout := context.WithTimeout(ctx, 60*time.Second)
	t.Cleanup(func() { cancelTimeout(); cancelCtx(); cancelAlloc() })
	return &headerHarness{t: t, ctx: ctx, srv: srv, script: script}
}

func (h *headerHarness) open(path string) {
	h.t.Helper()
	if err := chromedp.Run(h.ctx, chromedp.Navigate(h.srv.URL+path)); err != nil {
		h.t.Fatal(err)
	}
}

func (h *headerHarness) call(fn string, params map[string]any) map[string]any {
	h.t.Helper()
	inner := &pageHarness{t: h.t, ctx: h.ctx, srv: h.srv, script: h.script}
	return inner.call(fn, params)
}

func (h *headerHarness) mainEval(expr string, v any) {
	h.t.Helper()
	if err := chromedp.Run(h.ctx, chromedp.Evaluate(expr, v)); err != nil {
		h.t.Fatal(err)
	}
}

var strictCSP = map[string]string{
	"Content-Security-Policy": "default-src 'self'; script-src 'self'; object-src 'none'; base-uri 'none'",
}

// cspTextFixture is static on purpose: under the strict CSP above the
// page cannot run its own scripts at all, which is exactly when text
// must still answer and eval must not.
const cspTextFixture = `<!doctype html><html><body>
<h1>Visible headline</h1>
<p id="bio">Ada wrote the first program.</p>
<label>Secret <input type="password" id="pw" value="hunter2-secret" aria-label="Secret code"></label>
<label>Nickname <input id="nick" value="ada"></label>
</body></html>`

func TestPageTextReadsUnderStrictCSP(t *testing.T) {
	h := newHeaderHarness(t, map[string]headerFixture{
		"/csp": {body: cspTextFixture, headers: strictCSP},
	})
	h.open("/csp")

	whole := h.call("text", nil)
	if whole["error"] != nil {
		t.Fatalf("whole-page text: %v", whole["error"])
	}
	content := whole["content"].(string)
	for _, want := range []string{"Visible headline", "Ada wrote the first program."} {
		if !strings.Contains(content, want) {
			t.Fatalf("whole-page text misses %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "hunter2-secret") {
		t.Fatalf("whole-page text leaks the password value:\n%s", content)
	}

	bio := h.call("text", map[string]any{"selector": "#bio"})
	if bio["error"] != nil || bio["content"] != "Ada wrote the first program." {
		t.Fatalf("addressed text = %v", bio)
	}

	// A password input reads as its name only — never its value.
	pw := h.call("text", map[string]any{"selector": "#pw"})
	if pw["error"] != nil {
		t.Fatalf("password text: %v", pw["error"])
	}
	if strings.Contains(fmt.Sprint(pw["content"]), "hunter2-secret") {
		t.Fatalf("password value returned as text: %v", pw)
	}
	if pw["content"] != "Secret code" {
		t.Fatalf("password reads as %q, want its accessible name", pw["content"])
	}
	var suppressed struct {
		Suppressed string `json:"suppressed"`
	}
	if err := json.Unmarshal([]byte(pw["data"].(string)), &suppressed); err != nil || suppressed.Suppressed != "password" {
		t.Fatalf("password suppression not declared: %v (%v)", pw["data"], err)
	}

	// Rendered text, not field values: an input's value is not text.
	if nick := h.call("text", map[string]any{"selector": "#nick"}); nick["error"] != nil || nick["content"] != "" {
		t.Fatalf("input value reads as text: %v", nick)
	}

	if miss := h.call("text", map[string]any{"selector": "#missing"}); miss["error"] == nil {
		t.Fatalf("text on a missing target must be an error: %v", miss)
	}
}

// scrollFixture is a tall page with one inner scroll panel holding
// twenty rows; the fixed banner must survive a stitch layout/restore.
const scrollFixture = `<!doctype html><html><body style="margin:0">
<div id="banner" style="position:fixed;top:0;left:0;right:0;height:30px;background:#c00;color:#fff">banner</div>
<h1 style="margin-top:40px">Top of a tall page</h1>
<div id="panel" style="height:200px;overflow-y:auto;border:1px solid #000;margin-top:50px">
<div style="height:40px">Row 1</div><div style="height:40px">Row 2</div><div style="height:40px">Row 3</div><div style="height:40px">Row 4</div><div style="height:40px">Row 5</div><div style="height:40px">Row 6</div><div style="height:40px">Row 7</div><div style="height:40px">Row 8</div><div style="height:40px">Row 9</div><div id="deep" style="height:40px">Row 10 deep target</div><div style="height:40px">Row 11</div><div style="height:40px">Row 12</div><div style="height:40px">Row 13</div><div style="height:40px">Row 14</div><div style="height:40px">Row 15</div><div style="height:40px">Row 16</div><div style="height:40px">Row 17</div><div style="height:40px">Row 18</div><div style="height:40px">Row 19</div><div style="height:40px">Row 20 bottom</div>
</div>
<div style="height:2200px">below the fold</div>
</body></html>`

func TestPageScrollMovesNearestPanel(t *testing.T) {
	h := newPageHarness(t, map[string]string{"/s": scrollFixture})
	h.open("/s")

	// Addressed: the panel moves, the window stays.
	res := h.call("scroll", map[string]any{"selector": "#deep", "amount": 100})
	if res["error"] != nil {
		t.Fatalf("addressed scroll: %v", res["error"])
	}
	if !strings.HasPrefix(res["content"].(string), "scrolled <div>") {
		t.Fatalf("addressed scroll names no panel: %v", res)
	}
	var panelTop, winY float64
	h.mainEval("document.getElementById('panel').scrollTop", &panelTop)
	h.mainEval("window.scrollY", &winY)
	if panelTop != 100 || winY != 0 {
		t.Fatalf("addressed scroll moved panel=%v window=%v, want 100 and 0", panelTop, winY)
	}

	// Unaddressed: the window moves, as before.
	res = h.call("scroll", map[string]any{"direction": "down", "amount": 200})
	if res["error"] != nil {
		t.Fatalf("window scroll: %v", res["error"])
	}
	h.mainEval("window.scrollY", &winY)
	if winY != 200 {
		t.Fatalf("window scrollY = %v, want 200", winY)
	}
}

func TestPageShotLayoutSetRestore(t *testing.T) {
	h := newPageHarness(t, map[string]string{"/s": scrollFixture})
	h.open("/s")

	lay := h.call("shotLayout", nil)
	if lay == nil {
		t.Fatal("shotLayout answered nothing")
	}
	panels, _ := lay["panels"].([]any)
	if len(panels) != 1 {
		t.Fatalf("panels = %v, want exactly the inner panel", lay["panels"])
	}
	p0 := panels[0].(map[string]any)
	if p0["sh"].(float64) <= p0["h"].(float64) {
		t.Fatalf("panel has no hidden extent: %v", p0)
	}
	savedY := lay["y"].(float64)

	// The layout hides fixed elements for the stitch...
	var bannerDisplay string
	h.mainEval("getComputedStyle(document.getElementById('banner')).display", &bannerDisplay)
	if bannerDisplay == "" {
		t.Fatal("banner unreadable")
	}
	if bannerDisplay != "none" {
		t.Fatalf("layout left the fixed banner shown (%q): tiles would repeat it", bannerDisplay)
	}

	set := h.call("shotSet", map[string]any{"x": 0, "y": 300, "p": 0, "pt": 120})
	if set["y"] != 300.0 || set["pst"] != 120.0 {
		t.Fatalf("shotSet did not land exactly: %v", set)
	}

	// ...and the restore puts everything back.
	if res := h.call("shotRestore", lay); res["ok"] != true {
		t.Fatalf("shotRestore: %v", res)
	}
	var winY, panelTop float64
	h.mainEval("window.scrollY", &winY)
	h.mainEval("document.getElementById('panel').scrollTop", &panelTop)
	h.mainEval("getComputedStyle(document.getElementById('banner')).display", &bannerDisplay)
	if winY != savedY || panelTop != 0 {
		t.Fatalf("positions not restored: window=%v panel=%v", winY, panelTop)
	}
	if bannerDisplay == "none" {
		t.Fatal("fixed banner stranded hidden by the stitch")
	}
	// A second layout works on the restored page (no stranded cache).
	if lay2 := h.call("shotLayout", nil); len(lay2["panels"].([]any)) != 1 {
		t.Fatalf("re-layout failed: %v", lay2)
	} else if res := h.call("shotRestore", lay2); res["ok"] != true {
		t.Fatalf("re-restore failed: %v", res)
	}
}
