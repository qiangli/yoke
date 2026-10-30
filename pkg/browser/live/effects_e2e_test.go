package live

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/qiangli/yoke/pkg/browser/wire"
)

// S536.4 through the REAL extension (same technique as
// extension_e2e_test.go): honest click outcomes, CSP-safe text where
// `evaluate` is refused, an addressed scroll that moves the panel, and a
// stitched full-page capture with verified extent, content and restored
// positions.
//
// Opt-in like the other Chrome tests:
//
//	BASHY_BROWSER_LIVE=1 go test ./pkg/browser/live -run TestExtensionE2EEffects
//
// The /csp fixture is served with a strict Content-Security-Policy and
// runs no page script at all: the extension's MAIN-world evaluate (new
// Function) is refused there while the isolated-world text answers.

// e2eEffectsFixture mirrors the solo effects page (colored panel bottom
// + page bottom for capture-content checks).
const e2eEffectsFixture = `<!doctype html><html><body style="margin:0">
<button id="inert">Do nothing</button>
<button id="changer" onclick="document.getElementById('out').textContent='changed'">Change</button>
<button id="dialoger" onclick="document.getElementById('dlg').showModal()">Open dialog</button>
<a id="hashlink" href="#there">Go there</a>
<p id="out">before</p>
<dialog id="dlg"><p>Hi</p></dialog>
<h1 id="headline">Effects headline</h1>
<p id="bio">Ada wrote the first program.</p>
<label>Secret <input type="password" id="pw" value="hunter2-secret" aria-label="Secret code"></label>
<div id="panel" style="height:200px;overflow-y:auto;border:1px solid #000">
<div style="height:40px;background:#f0f0f0">Row 1</div><div style="height:40px;background:#e0e0e0">Row 2</div><div style="height:40px;background:#f0f0f0">Row 3</div><div style="height:40px;background:#e0e0e0">Row 4</div><div style="height:40px;background:#f0f0f0">Row 5</div><div style="height:40px;background:#e0e0e0">Row 6</div><div style="height:40px;background:#f0f0f0">Row 7</div><div style="height:40px;background:#e0e0e0">Row 8</div><div style="height:40px;background:#f0f0f0">Row 9</div><div id="deep" style="height:40px;background:#e0e0e0">Row 10 deep target</div><div style="height:40px;background:#f0f0f0">Row 11</div><div style="height:40px;background:#e0e0e0">Row 12</div><div style="height:40px;background:#f0f0f0">Row 13</div><div style="height:40px;background:#e0e0e0">Row 14</div><div style="height:40px;background:#f0f0f0">Row 15</div><div style="height:40px;background:#e0e0e0">Row 16</div><div style="height:40px;background:#f0f0f0">Row 17</div><div style="height:40px;background:#e0e0e0">Row 18</div><div style="height:40px;background:#f0f0f0">Row 19</div><div id="panelbottom" style="height:40px;background:#ff00ff">Row 20 bottom</div>
</div>
<div id="there" style="height:1800px">middle</div>
<div id="pagebottom" style="height:200px;background:#00ffff">page bottom</div>
</body></html>`

const e2eCSPFixture = `<!doctype html><html><body>
<h1 id="headline">CSP headline</h1>
<p id="bio">Ada wrote the first program.</p>
<label>Secret <input type="password" id="pw" value="hunter2-secret" aria-label="Secret code"></label>
</body></html>`

// startEffectsE2E is startExtensionE2E with per-path response headers
// (for the CSP fixture, which a plain body map cannot express). It
// returns the fixture server URL too, so the test can navigate between
// fixture pages with absolute URLs.
func startEffectsE2E(t *testing.T, fixtures map[string]headerFixture, path string) (*Service, context.Context, string) {
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
	svc := New(port)
	if err := svc.EnsureReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	waitHealthy(t, port)

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
	for p, f := range fixtures {
		f := f
		mux.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			for k, v := range f.headers {
				w.Header().Set(k, v)
			}
			fmt.Fprint(w, f.body)
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
	return svc, ctx, srv.URL
}

// bandHas reports whether the exact RGB triple appears in the row band
// [y0, y0+h) of img. Screenshots of solid CSS blocks are pixel-exact.
func bandHas(img image.Image, r, g, b uint8, y0, h int) bool {
	bounds := img.Bounds()
	if y0 < bounds.Min.Y {
		y0 = bounds.Min.Y
	}
	y1 := y0 + h
	if y1 > bounds.Max.Y {
		y1 = bounds.Max.Y
	}
	for y := y0; y < y1; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			pr, pg, pb, _ := img.At(x, y).RGBA()
			if uint8(pr>>8) == r && uint8(pg>>8) == g && uint8(pb>>8) == b {
				return true
			}
		}
	}
	return false
}

func TestExtensionE2EEffects(t *testing.T) {
	guardIsolate(t)
	fixtures := map[string]headerFixture{
		"/fx":  {body: e2eEffectsFixture},
		"/csp": {body: e2eCSPFixture, headers: strictCSP},
	}
	svc, _, site := startEffectsE2E(t, fixtures, "/fx")
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	do := func(a wire.Action) *wire.Result {
		t.Helper()
		res, err := svc.Execute(ctx, a)
		if err != nil {
			t.Fatalf("%s: %v", a.Type, err)
		}
		if res == nil {
			t.Fatalf("%s: no result", a.Type)
		}
		return res
	}
	eval := func(script string) (string, string) {
		t.Helper()
		res := do(wire.Action{Type: wire.ActionEvaluate, Script: script})
		return res.Data, res.Error
	}

	// 1. Honest click outcomes through the real extension.
	for _, tc := range []struct{ selector, effect, check, want string }{
		{"#inert", "no-effect", `document.getElementById('out').textContent`, "before"},
		{"#changer", "dom-changed", `document.getElementById('out').textContent`, "changed"},
		{"#hashlink", "navigated", `location.hash`, "#there"},
		{"#dialoger", "dialog", `document.querySelector('dialog[open]') ? 'open' : 'shut'`, "open"},
	} {
		res := do(wire.Action{Type: wire.ActionClick, Selector: tc.selector})
		if !res.Success {
			t.Fatalf("click %s: %+v", tc.selector, res)
		}
		if res.Content != "clicked: "+tc.effect {
			t.Fatalf("click %s content = %q, want clicked: %s", tc.selector, res.Content, tc.effect)
		}
		if got, evalErr := eval(tc.check); evalErr != "" || got != tc.want {
			t.Fatalf("click %s state = %q (err %q), want %q", tc.selector, got, evalErr, tc.want)
		}
	}

	// 2. Text reads on the plain page, with password suppression.
	if res := do(wire.Action{Type: wire.ActionText}); !res.Success {
		t.Fatalf("text: %+v", res)
	} else if !strings.Contains(res.Content, "Effects headline") || strings.Contains(res.Content, "hunter2-secret") {
		t.Fatalf("whole-page text wrong:\n%s", res.Content)
	}
	if res := do(wire.Action{Type: wire.ActionText, Selector: "#pw"}); !res.Success || res.Content != "Secret code" {
		t.Fatalf("password text: %+v", res)
	}

	// 3. Addressed scroll moves the panel (read back by eval on this
	// policy-free page), and the result names the panel.
	if res := do(wire.Action{Type: wire.ActionScroll, Selector: "#deep", Amount: 100}); !res.Success {
		t.Fatalf("addressed scroll: %+v", res)
	} else if !strings.HasPrefix(res.Content, "scrolled <div>") {
		t.Fatalf("addressed scroll names no panel: %+v", res)
	}
	if got, evalErr := eval(`document.getElementById('panel').scrollTop + '/' + window.scrollY`); evalErr != "" || got != "100/0" {
		t.Fatalf("addressed scroll positions = %q (err %q), want 100/0", got, evalErr)
	}

	// 4. Stitched full-page capture: path, extent, content, restore.
	// Re-navigate first so the pre-shot positions are exactly 0/0.
	if res := do(wire.Action{Type: wire.ActionNavigate, URL: site + "/fx"}); !res.Success {
		t.Fatalf("re-navigate: %+v", res)
	}
	shot := do(wire.Action{Type: wire.ActionScreenshot, FullPage: true})
	if !shot.Success || shot.Image == "" {
		t.Fatalf("full-page screenshot: %+v", shot)
	}
	var meta struct {
		Capture string `json:"capture"`
		Panels  int    `json:"panels"`
		Tiles   int    `json:"tiles"`
	}
	if err := json.Unmarshal([]byte(shot.Data), &meta); err != nil || meta.Capture != "stitched_full_page" {
		t.Fatalf("capture path = %q (%v), want stitched_full_page", shot.Data, err)
	}
	if meta.Panels != 1 || meta.Tiles < 3 {
		t.Fatalf("stitch meta = %+v, want 1 panel and tiled captures", meta)
	}
	geoJSON, evalErr := eval(`JSON.stringify({
	  VW: window.innerWidth, DocH: Math.max(document.documentElement.scrollHeight, document.body.scrollHeight),
	  PanelY: Math.round(document.getElementById('panel').getBoundingClientRect().top + window.scrollY),
	  PanelH: document.getElementById('panel').clientHeight,
	  PanelSH: document.getElementById('panel').scrollHeight })`)
	if evalErr != "" {
		t.Fatalf("geometry eval: %s", evalErr)
	}
	var geo struct {
		VW, DocH, PanelY, PanelH, PanelSH float64
	}
	if err := json.Unmarshal([]byte(geoJSON), &geo); err != nil {
		t.Fatalf("geometry: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(shot.Image)
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	scale := float64(img.Bounds().Dx()) / geo.VW
	wantH := (geo.DocH + (geo.PanelSH - geo.PanelH)) * scale
	if dh := float64(img.Bounds().Dy()) - wantH; dh < -2*scale || dh > 2*scale {
		t.Fatalf("stitched height = %d, want ~%.0f", img.Bounds().Dy(), wantH)
	}
	if !bandHas(img, 255, 0, 255, int((geo.PanelY+geo.PanelSH-20)*scale-5*scale), int(50*scale)) {
		t.Fatal("stitched image lacks the panel's bottom row (magenta)")
	}
	if !bandHas(img, 0, 255, 255, img.Bounds().Dy()-int(100*scale), int(100*scale)) {
		t.Fatal("stitched image lacks the page bottom (cyan)")
	}
	if got, evalErr := eval(`window.scrollY + '/' + document.getElementById('panel').scrollTop`); evalErr != "" || got != "0/0" {
		t.Fatalf("positions after stitch = %q (err %q), want the pre-shot 0/0", got, evalErr)
	}

	// 5. Under a strict CSP, evaluate is refused and text still answers.
	// (Unaddressed navigate opens a fresh tab; the address that follows
	// acts on it.)
	if res := do(wire.Action{Type: wire.ActionNavigate, URL: site + "/csp"}); !res.Success {
		t.Fatalf("navigate to CSP page: %+v", res)
	}
	if _, evalErr := eval(`1+1`); evalErr == "" {
		t.Fatal("evaluate answered under a strict CSP: the fixture proves nothing")
	}
	if res := do(wire.Action{Type: wire.ActionText}); !res.Success || !strings.Contains(res.Content, "CSP headline") {
		t.Fatalf("text under strict CSP: %+v", res)
	} else if strings.Contains(res.Content, "hunter2-secret") {
		t.Fatalf("text under strict CSP leaks the password:\n%s", res.Content)
	}
}

// hugePanelFixture serves one panel whose content (300 rows) needs far
// more tiles than the stitch cap covers, below a tall outer document
// with a fixed banner: the stitch must refuse explicitly (never answer
// the downgraded single capture) and still unhide the banner.
func hugePanelFixture() string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html><body style="margin:0">` +
		`<div id="banner" style="position:fixed;top:0;left:0;right:0;height:30px;background:#c00;color:#fff">banner</div>` +
		`<div style="height:500px">above the panel</div>` +
		`<div id="panel" style="height:200px;overflow-y:auto;border:1px solid #000">`)
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&b, `<div style="height:40px">Row %d</div>`, i)
	}
	b.WriteString(`</div><div style="height:2000px">below the fold</div></body></html>`)
	return b.String()
}

// TestExtensionE2EScreenshotRestoreAndRefusal runs on the manager's
// remote Chrome (BASHY_BROWSER_LIVE=1): a stitched capture from NONZERO
// window/panel positions restores those exact offsets (not 0,0), and a
// panel past the tile cap is an explicit refusal with the page restored.
func TestExtensionE2EScreenshotRestoreAndRefusal(t *testing.T) {
	guardIsolate(t)
	fixtures := map[string]headerFixture{
		"/fx":   {body: e2eEffectsFixture},
		"/huge": {body: hugePanelFixture()},
	}
	svc, _, site := startEffectsE2E(t, fixtures, "/fx")
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	do := func(a wire.Action) *wire.Result {
		t.Helper()
		res, err := svc.Execute(ctx, a)
		if err != nil {
			t.Fatalf("%s: %v", a.Type, err)
		}
		if res == nil {
			t.Fatalf("%s: no result", a.Type)
		}
		return res
	}
	eval := func(script string) (string, string) {
		t.Helper()
		res := do(wire.Action{Type: wire.ActionEvaluate, Script: script})
		return res.Data, res.Error
	}
	must := func(script string) string {
		t.Helper()
		got, evalErr := eval(script)
		if evalErr != "" {
			t.Fatalf("eval %s: %s", script, evalErr)
		}
		return got
	}

	// From nonzero positions the stitch restores THOSE offsets.
	must(`window.scrollTo(0, 250), document.getElementById('panel').scrollTop = 120, "set"`)
	wantWin := must(`String(window.scrollY)`)
	wantPst := must(`String(document.getElementById('panel').scrollTop)`)
	if wantWin == "0" {
		t.Fatalf("fixture window did not scroll (win=%s)", wantWin)
	}
	shot := do(wire.Action{Type: wire.ActionScreenshot, FullPage: true})
	if !shot.Success || shot.Image == "" {
		t.Fatalf("nonzero full-page screenshot: %+v", shot)
	}
	var meta struct {
		Capture string `json:"capture"`
	}
	if err := json.Unmarshal([]byte(shot.Data), &meta); err != nil || meta.Capture != "stitched_full_page" {
		t.Fatalf("capture path = %q (%v), want stitched_full_page", shot.Data, err)
	}
	if got, evalErr := eval(`window.scrollY + '/' + document.getElementById('panel').scrollTop`); evalErr != "" || got != wantWin+"/"+wantPst {
		t.Fatalf("positions after stitch = %q (err %q), want %s/%s", got, evalErr, wantWin, wantPst)
	}

	// Past the tile cap the stitch refuses instead of downgrading, and
	// the failed layout still restores positions and the banner.
	if res := do(wire.Action{Type: wire.ActionNavigate, URL: site + "/huge"}); !res.Success {
		t.Fatalf("navigate to huge page: %+v", res)
	}
	must(`window.scrollTo(0, 250), "set"`)
	wantWin = must(`String(window.scrollY)`)
	shot = do(wire.Action{Type: wire.ActionScreenshot, FullPage: true})
	if shot.Success {
		t.Fatalf("over-cap stitch succeeded (a downgraded full_page must never read as success)")
	}
	for _, want := range []string{"over the", "cap", "refusing the single-capture fallback"} {
		if !strings.Contains(shot.Error, want) {
			t.Fatalf("refusal misses %q: %q", want, shot.Error)
		}
	}
	if got, evalErr := eval(`window.scrollY`); evalErr != "" || got != wantWin {
		t.Fatalf("window after refusal = %q (err %q), want %s", got, evalErr, wantWin)
	}
	if got, evalErr := eval(`getComputedStyle(document.getElementById('banner')).display`); evalErr != "" || got == "none" {
		t.Fatalf("banner after refusal = %q (err %q), want it reshown", got, evalErr)
	}
}
