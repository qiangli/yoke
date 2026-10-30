package solo

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/qiangli/yoke/pkg/browser/wire"
)

// S536.4 end to end through solo (real headless Chrome, shared CDP
// backend with probe): honest click outcomes, CSP-safe text, panel
// scroll, and a stitched full-page capture whose extent, content and
// restored positions are all verified. Opt-in like the other
// live-Chrome tests: BASHY_BROWSER_LIVE=1.
//
// The panel's bottom row is solid magenta and the page bottom solid
// cyan: neither is visible without scrolling, so a legacy (unstitched)
// capture demonstrably lacks them while the stitched one must contain
// them — the red control is built into the fixture.

// effectsFixture is one page for every S536.4 behavior: click targets
// for each observed outcome, readable text plus a password input, a
// tall document, and an inner scroll panel with twenty colored rows.
const effectsFixture = `<!doctype html><html><body style="margin:0">
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

// effectsCSPFixture is the static text subset, served with a strict
// Content-Security-Policy: no page script runs here at all.
const effectsCSPFixture = `<!doctype html><html><body>
<h1 id="headline">CSP headline</h1>
<p id="bio">Ada wrote the first program.</p>
<label>Secret <input type="password" id="pw" value="hunter2-secret" aria-label="Secret code"></label>
</body></html>`

func TestSoloEffectsClickTextScrollCapture(t *testing.T) {
	if os.Getenv("BASHY_BROWSER_LIVE") == "" {
		t.Skip("live Chrome test disabled; set BASHY_BROWSER_LIVE=1 to run (launches a real Chrome)")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("BASHY_SECRETS_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")

	mux := http.NewServeMux()
	mux.HandleFunc("/fx", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, effectsFixture)
	})
	mux.HandleFunc("/csp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; object-src 'none'; base-uri 'none'")
		fmt.Fprint(w, effectsCSPFixture)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	c := New(Config{})
	if !c.Available(ctx) {
		t.Skip("no Chrome or Chromium binary found")
	}
	defer c.Close()

	exec := func(a wire.Action) *wire.Result {
		t.Helper()
		res, err := c.Execute(ctx, a)
		if err != nil {
			t.Fatalf("%s: %v", a.Type, err)
		}
		if res == nil {
			t.Fatalf("%s: no result", a.Type)
		}
		return res
	}
	eval := func(expr string, v any) {
		t.Helper()
		if err := chromedp.Run(c.current(), chromedp.Evaluate(expr, v)); err != nil {
			t.Fatal(err)
		}
	}
	open := func(path string) {
		t.Helper()
		if res := exec(wire.Action{Type: wire.ActionNavigate, URL: srv.URL + path}); !res.Success {
			t.Fatalf("navigate %s: %+v", path, res)
		}
	}

	// 1. Honest click: every outcome is observed, not dispatched.
	open("/fx")
	for _, tc := range []struct{ selector, effect, check, want string }{
		{"#inert", "no-effect", "document.getElementById('out').textContent", "before"},
		{"#changer", "dom-changed", "document.getElementById('out').textContent", "changed"},
		{"#hashlink", "navigated", "location.hash", "#there"},
		{"#dialoger", "dialog", "document.querySelector('dialog[open]') ? 'open' : 'shut'", "open"},
	} {
		res := exec(wire.Action{Type: wire.ActionClick, Selector: tc.selector})
		if !res.Success {
			t.Fatalf("click %s: %+v", tc.selector, res)
		}
		if res.Content != "clicked: "+tc.effect {
			t.Fatalf("click %s content = %q, want clicked: %s", tc.selector, res.Content, tc.effect)
		}
		var data struct {
			Effect string `json:"effect"`
		}
		if err := json.Unmarshal([]byte(res.Data), &data); err != nil || data.Effect != tc.effect {
			t.Fatalf("click %s data = %q (%v)", tc.selector, res.Data, err)
		}
		var got string
		eval(tc.check, &got)
		if got != tc.want {
			t.Fatalf("click %s left %q, want %q", tc.selector, got, tc.want)
		}
	}

	// 2. CSP-safe text: the static page, the addressed element, and the
	// password suppression — then the same under a strict CSP.
	open("/fx")
	if res := exec(wire.Action{Type: wire.ActionText}); !res.Success {
		t.Fatalf("text: %+v", res)
	} else {
		if !strings.Contains(res.Content, "Effects headline") || !strings.Contains(res.Content, "Ada wrote") {
			t.Fatalf("whole-page text misses copy:\n%s", res.Content)
		}
		if strings.Contains(res.Content, "hunter2-secret") {
			t.Fatalf("whole-page text leaks the password:\n%s", res.Content)
		}
	}
	if res := exec(wire.Action{Type: wire.ActionText, Selector: "#bio"}); !res.Success || res.Content != "Ada wrote the first program." {
		t.Fatalf("addressed text: %+v", res)
	}
	if res := exec(wire.Action{Type: wire.ActionText, Selector: "#pw"}); !res.Success {
		t.Fatalf("password text: %+v", res)
	} else if strings.Contains(res.Content, "hunter2-secret") || res.Content != "Secret code" {
		t.Fatalf("password reads as %q", res.Content)
	}
	open("/csp")
	if res := exec(wire.Action{Type: wire.ActionText}); !res.Success || !strings.Contains(res.Content, "CSP headline") {
		t.Fatalf("text under strict CSP: %+v", res)
	} else if strings.Contains(res.Content, "hunter2-secret") {
		t.Fatalf("text under strict CSP leaks the password:\n%s", res.Content)
	}

	// 3. Addressed scroll moves the panel, never the window.
	open("/fx")
	if res := exec(wire.Action{Type: wire.ActionScroll, Selector: "#deep", Amount: 100}); !res.Success {
		t.Fatalf("addressed scroll: %+v", res)
	} else if !strings.HasPrefix(res.Content, "scrolled <div>") {
		t.Fatalf("addressed scroll names no panel: %+v", res)
	}
	var pst, winY float64
	eval("document.getElementById('panel').scrollTop", &pst)
	eval("window.scrollY", &winY)
	if pst != 100 || winY != 0 {
		t.Fatalf("addressed scroll: panel=%v window=%v, want 100 and 0", pst, winY)
	}

	// 4. Stitched full-page capture: extent, content, restored position.
	open("/fx")
	shotPath := filepath.Join(t.TempDir(), "stitched.png")
	res := exec(wire.Action{Type: wire.ActionScreenshot, FullPage: true, SavePath: shotPath})
	if !res.Success {
		t.Fatalf("full-page screenshot: %+v", res)
	}
	if res.Path == "" {
		t.Fatalf("full-page screenshot wrote no file: %+v", res)
	}
	var meta struct {
		Capture string `json:"capture"`
	}
	if err := json.Unmarshal([]byte(res.Data), &meta); err != nil || meta.Capture != "stitched_full_page" {
		t.Fatalf("capture path = %q (%v), want stitched_full_page", res.Data, err)
	}
	var geo struct {
		DPR, VW, VH, DocH, PanelY, PanelH, PanelSH float64
	}
	eval(`({
	  DPR: window.devicePixelRatio || 1, VW: window.innerWidth, VH: window.innerHeight,
	  DocH: Math.max(document.documentElement.scrollHeight, document.body.scrollHeight),
	  PanelY: Math.round(document.getElementById('panel').getBoundingClientRect().top + window.scrollY),
	  PanelH: document.getElementById('panel').clientHeight,
	  PanelSH: document.getElementById('panel').scrollHeight
	})`, &geo)
	img := decodeShot(t, res.Path)
	// Extent: the stitched image holds the document plus the panel's
	// hidden extent, at the renderer's own measured scale.
	dpr := float64(img.Bounds().Dx()) / geo.VW
	wantH := (geo.DocH + (geo.PanelSH - geo.PanelH)) * dpr
	if dh := float64(img.Bounds().Dy()) - wantH; dh < -2*dpr || dh > 2*dpr {
		t.Fatalf("stitched height = %d, want ~%.0f (doc %.0f + hidden %.0f at dpr %g)",
			img.Bounds().Dy(), wantH, geo.DocH, geo.PanelSH-geo.PanelH, dpr)
	}
	// Content: the panel's bottom row (magenta) and the page bottom
	// (cyan) are only in the image when stitched.
	magenta := color.RGBA{255, 0, 255, 255}
	cyan := color.RGBA{0, 255, 255, 255}
	panelZoneY := (geo.PanelY + geo.PanelSH - 20) * dpr
	if !hasColorBand(img, magenta, int(panelZoneY-5*dpr), int(50*dpr)) {
		t.Fatal("stitched image lacks the panel's bottom row (magenta)")
	}
	if !hasColorBand(img, cyan, img.Bounds().Dy()-int(100*dpr), int(100*dpr)) {
		t.Fatal("stitched image lacks the page bottom (cyan)")
	}
	// Red control: a plain viewport capture lacks the magenta row, so
	// the fixture distinguishes a stitch from a legacy capture.
	plainPath := filepath.Join(t.TempDir(), "viewport.png")
	pres := exec(wire.Action{Type: wire.ActionScreenshot, SavePath: plainPath})
	if !pres.Success {
		t.Fatalf("viewport screenshot: %+v", pres)
	}
	if hasColor(decodeShot(t, pres.Path), magenta) {
		t.Fatal("viewport capture contains magenta: the fixture does not distinguish stitched from legacy")
	}
	// Restored positions: the stitch leaves the page as it found it.
	eval("window.scrollY", &winY)
	eval("document.getElementById('panel').scrollTop", &pst)
	if winY != 0 || pst != 0 {
		t.Fatalf("positions not restored: window=%v panel=%v", winY, pst)
	}
}

func decodeShot(t *testing.T, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// hasColor reports whether c appears anywhere in img (exact match;
// screenshots of solid CSS blocks are pixel-exact).
func hasColor(img image.Image, c color.RGBA) bool {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if uint8(r>>8) == c.R && uint8(g>>8) == c.G && uint8(bl>>8) == c.B {
				return true
			}
		}
	}
	return false
}

// hasColorBand reports whether c appears in the row band [y0, y0+h).
func hasColorBand(img image.Image, c color.RGBA, y0, h int) bool {
	b := img.Bounds()
	if y0 < b.Min.Y {
		y0 = b.Min.Y
	}
	y1 := y0 + h
	if y1 > b.Max.Y {
		y1 = b.Max.Y
	}
	for y := y0; y < y1; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if uint8(r>>8) == c.R && uint8(g>>8) == c.G && uint8(bl>>8) == c.B {
				return true
			}
		}
	}
	return false
}
