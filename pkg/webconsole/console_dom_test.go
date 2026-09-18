// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build verifydom

// End-to-end browser checks for the console's UI.
//
// WHY THESE EXIST. Every other test in this package reads the SERVED BYTES:
// it can assert that a string is present, and nothing more. It cannot see the
// cascade, it cannot see the DOM, and it cannot see a script that throws. Four
// shipped defects in one sitting all lived in exactly that blind spot:
//
//   - both password-eye icons rendered, because the UA's [hidden] rule does not
//     apply to an inline <svg> — a cascade fact;
//   - the eye never swapped, because `hidden` is an HTMLElement property that
//     SVGElement does not have — a DOM fact;
//   - the Files Apps control vanished, because an <svg> gets no box from
//     `.action i` — a layout fact;
//   - Settings stopped opening, because buildPairing threw NotFoundError before
//     the dialog was shown — a runtime fact, and the one that proves the point:
//     a fix in the pairing section broke a control three sections away, and
//     every byte-level test still passed.
//
// So the rule these encode: a page that throws is a broken page, whatever its
// bytes say. assertNoJSErrors is the check that would have caught all four.
//
// Run:  go test ./pkg/webconsole -tags verifydom
// They need a Chrome on the host, which is why they are behind a tag.

package webconsole

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/qiangli/yoke/pkg/board"
	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/resources"
	"github.com/qiangli/yoke/pkg/room"
	"github.com/qiangli/yoke/pkg/websession"
)

// domEnv boots a console, a browser, and an exception recorder. The recorder is
// the point: chromedp does not fail a run because the page threw, so an
// uncaught error is invisible unless something is listening for it.
func domEnv(t *testing.T, opts Options) (string, context.Context, func() []string) {
	t.Helper()
	h := newTestHandler(t, opts)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	alloc, acancel := chromedp.NewExecAllocator(context.Background(), browserOpts()...)
	t.Cleanup(acancel)
	ctx, cancel := chromedp.NewContext(alloc)
	t.Cleanup(cancel)
	ctx, tcancel := context.WithTimeout(ctx, 90*time.Second)
	t.Cleanup(tcancel)

	var errs []string
	chromedp.ListenTarget(ctx, func(ev any) {
		switch e := ev.(type) {
		case *runtime.EventExceptionThrown:
			errs = append(errs, "uncaught exception: "+e.ExceptionDetails.Error())
		case *runtime.EventConsoleAPICalled:
			if e.Type == "error" {
				for _, a := range e.Args {
					errs = append(errs, "console.error: "+string(a.Value))
				}
			}
		}
	})
	return srv.URL, ctx, func() []string { return errs }
}

// browserOpts builds the browser launch flags.
//
// Chrome's own sandbox is left ON everywhere it works. On Linux it does not:
// Ubuntu 23.10+ (which is what the CI runner is) disables unprivileged user
// namespaces through AppArmor, so the zygote finds no usable sandbox and
// Chrome aborts before the first navigation — every test in this file failed
// that way on the Linux leg while passing on macOS and Windows.
//
// --no-sandbox is the documented workaround and is safe HERE for a reason that
// does not generalise: this browser is disposable, it is launched by the test,
// and the only thing it ever loads is the loopback console under test. The
// alternative is not a safer Linux run, it is no Linux run at all.
func browserOpts() []chromedp.ExecAllocatorOption {
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	// chromedp's 20-second default is shorter than Chrome can occasionally
	// take to publish its DevTools endpoint on a busy shared CI runner. Keep
	// the launch bounded by domEnv's 90-second context, but do not mistake a
	// slow browser start for a broken page.
	opts = append(opts, chromedp.WSURLReadTimeout(60*time.Second))
	if goruntime.GOOS == "linux" {
		opts = append(opts, chromedp.NoSandbox)
	}
	return opts
}

func assertNoJSErrors(t *testing.T, where string, errs []string) {
	t.Helper()
	for _, e := range errs {
		t.Errorf("%s: %s", where, e)
	}
}

// The launcher must render its tiles and throw nothing.
func TestDOMLauncherRendersCleanly(t *testing.T) {
	base, ctx, errs := domEnv(t, Options{})

	var body, tiles string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/"),
		chromedp.Sleep(1500*time.Millisecond),
		chromedp.Evaluate(`document.body.innerText.slice(0,200)`, &body),
		chromedp.Evaluate(`String(document.querySelectorAll("a,button").length)`, &tiles),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "launcher", errs())
	if !strings.Contains(body, "APPS") {
		t.Errorf("launcher did not render its app list: %q", body)
	}
	if tiles == "0" {
		t.Error("launcher rendered no interactive elements")
	}
}

// Settings must actually OPEN. It regressed to a dialog that stayed shut
// because a section built before it threw, and the byte-level tests could not
// tell: every string they look for was still in the page.
func TestDOMSettingsDialogOpens(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"pairing unarmed", Options{}},
		{"pairing armed", Options{
			Pairing:       true,
			PairStorePath: t.TempDir() + "/pair.json",
			Sessions:      websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt")),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, ctx, errs := domEnv(t, tc.opts)

			var open, text string
			if err := chromedp.Run(ctx,
				chromedp.Navigate(base+"/"),
				chromedp.Sleep(1500*time.Millisecond),
				chromedp.Click(`#settings-btn`, chromedp.ByQuery),
				chromedp.Sleep(1200*time.Millisecond),
				chromedp.Evaluate(`String(document.getElementById("settings")?.open)`, &open),
				chromedp.Evaluate(`document.getElementById("settings")?.innerText||""`, &text),
			); err != nil {
				t.Fatalf("chromedp: %v", err)
			}
			assertNoJSErrors(t, "settings", errs())
			if open != "true" {
				t.Fatalf("Settings did not open (dialog.open=%s)", open)
			}
			// Every section the dialog promises must be built, not just the
			// ones before whichever one threw.
			// Compare case-insensitively: the sheet uppercases headings in CSS
			// and innerText returns RENDERED text, so "Appearance" arrives as
			// "APPEARANCE".
			lower := strings.ToLower(text)
			for _, want := range []string{"appearance", "open apps", "background", "sections", "phone pairing", "favorites"} {
				if !strings.Contains(lower, want) {
					t.Errorf("Settings is missing the %q section", want)
				}
			}
		})
	}
}

// The login form's show/hide control must show exactly ONE icon and swap it.
func TestDOMPasswordToggleSwaps(t *testing.T) {
	base, ctx, errs := domEnv(t, Options{})

	var eye0, off0, type0, eye1, off1, type1 string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/login"),
		chromedp.WaitVisible(`#password-toggle`, chromedp.ByQuery),
		chromedp.Evaluate(`getComputedStyle(document.querySelector(".eye")).display`, &eye0),
		chromedp.Evaluate(`getComputedStyle(document.querySelector(".eye-off")).display`, &off0),
		chromedp.Evaluate(`document.getElementById("login-password").type`, &type0),
		chromedp.Click(`#password-toggle`, chromedp.ByQuery),
		chromedp.Evaluate(`getComputedStyle(document.querySelector(".eye")).display`, &eye1),
		chromedp.Evaluate(`getComputedStyle(document.querySelector(".eye-off")).display`, &off1),
		chromedp.Evaluate(`document.getElementById("login-password").type`, &type1),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "login", errs())
	if eye0 == "none" || off0 != "none" {
		t.Errorf("initial state must show ONLY the eye: eye=%s eye-off=%s", eye0, off0)
	}
	if eye1 != "none" || off1 == "none" {
		t.Errorf("after click must show ONLY the crossed-out eye: eye=%s eye-off=%s", eye1, off1)
	}
	if type0 != "password" || type1 != "text" {
		t.Errorf("input type must go password -> text, got %s -> %s", type0, type1)
	}
}

// A send receipt is per recipient. The result must keep the recipient, the
// canonical state, and the transport's reason together rather than reducing a
// multi-recipient reply to a misleading generic "sent" confirmation.
func TestDOMMessageSendRendersEveryDelivery(t *testing.T) {
	base, ctx, errs := domEnv(t, Options{})

	var receipt string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/mb/"),
		chromedp.WaitVisible(`#mb-composer`, chromedp.ByQuery),
		chromedp.Evaluate(`
(() => {
  const realFetch = window.fetch;
  window.fetch = async (input, init = {}) => {
    if (init.method === "POST") {
      return new Response(JSON.stringify({result: {
        seq: 17, label: "release crew", deliveries: [
          {to: "ada", state: "accepted", reason: "role seat"},
          {to: "bea", state: "queued", reason: "reader is behind"},
          {to: "cam", state: "delivered", reason: "live session"},
          {to: "dee", state: "read", reason: "cursor caught up"},
          {to: "eli", state: "failed", reason: "not running"},
          {to: "fay", state: "unverified", reason: "no cursor"}
        ]
      }}), {status: 200, headers: {"Content-Type": "application/json"}});
    }
    return realFetch(input, init);
  };
})();`, nil),
		chromedp.SetValue(`#c-body`, "receipt test", chromedp.ByQuery),
		chromedp.Evaluate(`document.getElementById("mb-composer").requestSubmit()`, nil),
		chromedp.WaitReady(`#c-result.ok`, chromedp.ByQuery),
		chromedp.Evaluate(`document.getElementById("c-result").innerText`, &receipt),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "message send", errs())
	for _, want := range []string{
		"ada · accepted · role seat",
		"bea · queued · reader is behind",
		"cam · delivered · live session",
		"dee · read · cursor caught up",
		"eli · failed · not running",
		"fay · unverified · no cursor",
	} {
		if !strings.Contains(receipt, want) {
			t.Errorf("send receipt = %q, missing %q", receipt, want)
		}
	}
}

// Pairing: no on/off switch, a QR when asked, and Refresh only once there is a
// code to replace.
func TestDOMPairingSection(t *testing.T) {
	base, ctx, errs := domEnv(t, Options{
		Pairing:       true,
		PairStorePath: t.TempDir() + "/pair.json",
		Sessions:      websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt")),
	})

	var toggles, refreshBefore, panel, refreshAfter string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/"),
		chromedp.Sleep(1500*time.Millisecond),
		chromedp.Click(`#settings-btn`, chromedp.ByQuery),
		chromedp.Sleep(1200*time.Millisecond),
		chromedp.Evaluate(`String(document.querySelectorAll("#pair-toggle").length)`, &toggles),
		chromedp.Evaluate(`String(document.getElementById("pair-refresh")?.hidden)`, &refreshBefore),
		chromedp.Click(`#pair-btn`, chromedp.ByQuery),
		chromedp.Sleep(1800*time.Millisecond),
		chromedp.Evaluate(`(document.getElementById("pair-panel")?.innerHTML||"").slice(0,300)`, &panel),
		chromedp.Evaluate(`String(document.getElementById("pair-refresh")?.hidden)`, &refreshAfter),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "pairing", errs())
	if toggles != "0" {
		t.Errorf("phone pairing still has an on/off switch (%s found)", toggles)
	}
	if !strings.Contains(panel, "data:image/png;base64,") {
		t.Errorf("no QR rendered after asking for a code: %q", panel)
	}
	if refreshBefore != "true" || refreshAfter != "false" {
		t.Errorf("refresh visibility wrong: before=%s after=%s (want true -> false)",
			refreshBefore, refreshAfter)
	}
}

// Every panel must load and throw nothing. This is the sweep that catches a fix
// in one app breaking another.
func TestDOMEveryPanelLoadsCleanly(t *testing.T) {
	for _, panel := range []string{"/", "/sprint/", "/mb/", "/inbox/", "/files/", "/meet/", "/term/"} {
		t.Run(panel, func(t *testing.T) {
			base, ctx, errs := domEnv(t, Options{})
			var title string
			if err := chromedp.Run(ctx,
				chromedp.Navigate(base+panel),
				chromedp.Sleep(2500*time.Millisecond),
				chromedp.Evaluate(`document.title`, &title),
			); err != nil {
				t.Fatalf("chromedp %s: %v", panel, err)
			}
			assertNoJSErrors(t, panel, errs())
			if title == "" {
				t.Errorf("%s rendered no title", panel)
			}
		})
	}
}

// The Files app carries the console's own return control: four rounded squares,
// laid out with a real box, pointing at the LAUNCHER rather than back at
// itself. It is mounted rather than chrome-injected, so it never receives
// #all-apps-btn and has to draw the same mark itself.
func TestDOMFilesAppsControl(t *testing.T) {
	base, ctx, errs := domEnv(t, Options{})

	var present, rects, href, box string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/files/"),
		chromedp.Sleep(3500*time.Millisecond),
		chromedp.Evaluate(`String(!!document.querySelector(".apps-button"))`, &present),
		chromedp.Evaluate(`String(document.querySelectorAll(".apps-button svg rect").length)`, &rects),
		chromedp.Evaluate(`document.querySelector(".apps-button")?.getAttribute("href")||"NONE"`, &href),
		chromedp.Evaluate(`(()=>{const e=document.querySelector(".apps-button svg");
			if(!e) return "NO SVG";
			const r=e.getBoundingClientRect();
			return JSON.stringify({w:Math.round(r.width),h:Math.round(r.height)});})()`, &box),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "files", errs())
	if present != "true" {
		t.Fatal("the Files header has no Apps control")
	}
	if rects != "4" {
		t.Errorf("Apps mark has %s rects, want the console's 4-square mark", rects)
	}
	if href != "/" {
		t.Errorf("Apps control points at %q, want %q (the launcher, not the files app)", href, "/")
	}
	// A control with no box is a control nobody can click — the exact way this
	// one disappeared when it stopped being an icon-font glyph.
	if strings.Contains(box, `"w":0`) || strings.Contains(box, `"h":0`) || box == "NO SVG" {
		t.Errorf("Apps mark has no layout box: %s", box)
	}
}

// Background is a setting that must actually TAKE. Choosing a swatch has to
// change what the launcher renders, not merely mark a button pressed — a
// pressed button with no visual change is the shape of a setting that silently
// does nothing.
func TestDOMBackgroundSwatchApplies(t *testing.T) {
	base, ctx, errs := domEnv(t, Options{})

	var swatches, before, pressed, after string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/"),
		chromedp.Sleep(1500*time.Millisecond),
		chromedp.Click(`#settings-btn`, chromedp.ByQuery),
		chromedp.Sleep(1200*time.Millisecond),
		chromedp.Evaluate(`String(document.querySelectorAll("#bg-swatches .swatch-btn").length)`, &swatches),
		chromedp.Evaluate(`document.querySelector("[data-bashy-bg]")?.getAttribute("data-bashy-bg")||"MISSING"`, &before),
		// The last swatch is a real background; the first is "None".
		chromedp.Evaluate(`(()=>{const b=[...document.querySelectorAll("#bg-swatches .swatch-btn")];
			if(!b.length) return "NONE"; b[b.length-1].click(); return "clicked";})()`, &pressed),
		chromedp.Sleep(1200*time.Millisecond),
		chromedp.Evaluate(`document.querySelector("[data-bashy-bg]")?.getAttribute("data-bashy-bg")||"MISSING"`, &after),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "background", errs())
	if swatches == "0" {
		t.Fatal("the Background section rendered no swatches")
	}
	if pressed != "clicked" {
		t.Fatalf("could not click a background swatch: %s", pressed)
	}
	if before == "MISSING" || after == "MISSING" {
		t.Fatalf("no element carries data-bashy-bg (before=%q after=%q)", before, after)
	}
	if after == before {
		t.Errorf("choosing a background changed nothing (data-bashy-bg stayed %q)", before)
	}
}

// Long press on a tile must NOT open the browser's context menu.
//
// Favouriting on a phone already works: the long press puts the tile in its
// hover state, the star appears, and it can be tapped — verified on a real
// Samsung, which also corrected an earlier assumption here that a favorite
// could never be added on touch at all. What ruins the gesture is that the same
// press opens the browser's own long list of menu items over the star. So the
// only thing to fix, and the only thing pinned here, is that menu.
//
// It emulates a real touch device (mobile metrics + touch events) rather than
// setEmulatedMedia, which does not implement the `hover` feature — measured:
// matchMedia("(hover: none)") still reported false under it.
func TestDOMLongPressMenuSuppressedOnTiles(t *testing.T) {
	base, ctx, errs := domEnv(t, Options{})

	var hoverNone, onTileTouch, onTileMouse, offTile string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/"),
		emulation.SetDeviceMetricsOverride(390, 844, 3, true),
		emulation.SetTouchEmulationEnabled(true).WithMaxTouchPoints(5),
		chromedp.Navigate(base+"/"),
		chromedp.Sleep(1800*time.Millisecond),
		chromedp.Evaluate(`String(matchMedia("(hover: none)").matches)`, &hoverNone),

		// A touch long-press on a tile: the menu must be suppressed.
		chromedp.Evaluate(`(()=>{const w=document.querySelector(".tile-wrap");
			if(!w) return "NO TILE";
			const ev=new PointerEvent("contextmenu",{bubbles:true,cancelable:true,pointerType:"touch"});
			w.dispatchEvent(ev); return String(ev.defaultPrevented);})()`, &onTileTouch),

		// A MOUSE right-click keeps its menu: a desktop user has no long press
		// and must not lose copy-link or open-in-new-tab.
		chromedp.Evaluate(`(()=>{const w=document.querySelector(".tile-wrap");
			const ev=new PointerEvent("contextmenu",{bubbles:true,cancelable:true,pointerType:"mouse"});
			w.dispatchEvent(ev); return String(ev.defaultPrevented);})()`, &onTileMouse),

		// And suppression is SCOPED to tiles: the rest of the page keeps its
		// menu, so copy/share are not taken away console-wide.
		chromedp.Evaluate(`(()=>{const ev=new PointerEvent("contextmenu",{bubbles:true,cancelable:true,pointerType:"touch"});
			document.body.dispatchEvent(ev); return String(ev.defaultPrevented);})()`, &offTile),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "long-press", errs())

	if hoverNone != "true" {
		t.Fatalf("device emulation did not produce a no-hover device (matchMedia=%s)", hoverNone)
	}
	if onTileTouch != "true" {
		t.Errorf("the browser long-press menu still opens on a tile (defaultPrevented=%s); "+
			"it covers the star the press just revealed", onTileTouch)
	}
	if onTileMouse != "false" {
		t.Errorf("a MOUSE right-click on a tile lost its menu (defaultPrevented=%s); "+
			"suppression must apply to the touch gesture only", onTileMouse)
	}
	if offTile != "false" {
		t.Errorf("context menu suppressed off-tile too (defaultPrevented=%s); "+
			"that would cost copy-link and share across the whole console", offTile)
	}
}

// The operator chooses what a paired phone may reach, in Settings.
//
// Before this, a pass was always meet/mb/sprint and `bashy app pair --allow`
// was the only way to widen it — so a phone that hit "terminal is not in the
// scope" got a refusal with no way to act on it. Two properties matter and both
// are pinned: the shell and the filesystem are OFF by default (a grant must be
// a decision, not an unread default), and ticking one is actually honoured in
// the minted ticket.
func TestDOMPairingScopeIsChosenByTheOperator(t *testing.T) {
	base, ctx, errs := domEnv(t, Options{
		Pairing:       true,
		PairStorePath: t.TempDir() + "/pair.json",
		Sessions:      websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt")),
	})

	var boxes, checkedByDefault, scopeAfter string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/"),
		chromedp.Sleep(1500*time.Millisecond),
		chromedp.Click(`#settings-btn`, chromedp.ByQuery),
		chromedp.Sleep(1200*time.Millisecond),
		chromedp.Evaluate(`String(document.querySelectorAll(".pair-scope-box").length)`, &boxes),
		chromedp.Evaluate(`JSON.stringify([...document.querySelectorAll(".pair-scope-box")]
			.filter(b=>b.checked).map(b=>b.value).sort())`, &checkedByDefault),

		// Tick the shell, ask for a code, and read back what the server minted.
		chromedp.Evaluate(`(()=>{const t=[...document.querySelectorAll(".pair-scope-box")]
			.find(b=>b.value==="terminal");
			if(!t) return "NO TERMINAL BOX"; t.checked=true; return "ticked";})()`, new(string)),
		chromedp.Evaluate(`fetch("api/pair",{method:"POST",
			headers:{Accept:"application/json","Content-Type":"application/json"},
			body:JSON.stringify({scope:selectedPairScope()})})
			.then(r=>r.json()).then(d=>JSON.stringify((d.scope||[]).sort()))`, &scopeAfter,
			func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
				return p.WithAwaitPromise(true)
			}),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "pair scope", errs())

	if boxes == "0" {
		t.Fatal("Settings offers no way to choose what a paired phone may reach")
	}
	// Default-deny: a shell or the filesystem must never be granted by a
	// default nobody read.
	if strings.Contains(checkedByDefault, "terminal") || strings.Contains(checkedByDefault, "files") {
		t.Errorf("terminal/files are pre-granted by default: %s", checkedByDefault)
	}
	if !strings.Contains(checkedByDefault, "sprint") {
		t.Errorf("the read-and-communicate default is missing: %s", checkedByDefault)
	}
	// And the choice must actually reach the ticket.
	if !strings.Contains(scopeAfter, "terminal") {
		t.Errorf("ticking terminal did not widen the minted pass: scope=%s", scopeAfter)
	}
}

// relay must wear the same header as the other apps.
//
// board, messages and terminal each state a brand block in markup — the
// panel's mark in a rounded tile, then bashy + the panel name — and receive the
// console's four-square all-apps control from injectChrome. relay is a MOUNTED
// SPA, so it is served outside that path and receives neither: it has to draw
// both itself, the same way the Files app does.
func TestDOMRelayHeaderMatchesTheOtherApps(t *testing.T) {
	base, ctx, errs := domEnv(t, Options{})

	var brand, wordmark, allAppsRects, allAppsHref string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/meet/"),
		chromedp.Sleep(3500*time.Millisecond),
		chromedp.Evaluate(`String(!!document.querySelector(`+"`"+`header a[title="bashy meet"]`+"`"+`))`, &brand),
		chromedp.Evaluate(`(document.querySelector(`+"`"+`header a[title="bashy meet"]`+"`"+`)?.innerText||"").trim()`, &wordmark),
		chromedp.Evaluate(`String(document.querySelectorAll(`+"`"+`header a[title="All apps"] svg rect`+"`"+`).length)`, &allAppsRects),
		chromedp.Evaluate(`document.querySelector(`+"`"+`header a[title="All apps"]`+"`"+`)?.getAttribute("href")||"NONE"`, &allAppsHref),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "relay", errs())

	if brand != "true" {
		t.Error("relay has no brand block; board/messages/terminal all carry one")
	}
	// The wordmark is hidden below sm: the phone header also holds the rooms
	// button and the conversation title, which is what matters there. At this
	// viewport it must be present.
	if !strings.Contains(strings.ToLower(wordmark), "meet") {
		t.Errorf("relay's brand does not name the app: %q", wordmark)
	}
	if allAppsRects != "4" {
		t.Errorf("relay's all-apps control has %s rects, want the console's 4-square mark", allAppsRects)
	}
	if allAppsHref == "NONE" {
		t.Error("relay has no all-apps control")
	}
}

// "Same header as the other apps" has to be MEASURED, or it drifts back into a
// lookalike. This reads the computed box of relay's header and brand and
// compares them with board's — the two are styled by separate stylesheets
// (board links app.css; relay is a mounted SPA that restates the rules), which
// is exactly the kind of duplication that silently diverges.
func TestDOMRelayHeaderMatchesBoardMetrics(t *testing.T) {
	base, ctx, errs := domEnv(t, Options{})

	probe := `(()=>{const h=document.querySelector("header");
		if(!h) return "NO HEADER";
		const cs=getComputedStyle(h);
		const logo=document.querySelector("header .logo, header .console-logo");
		const mark=document.querySelector("header #wordmark, header .console-wordmark");
		const m=mark?getComputedStyle(mark):null;
		return JSON.stringify({
			padding: cs.padding,
			borderBottomWidth: cs.borderBottomWidth,
			logo: logo?Math.round(logo.getBoundingClientRect().width):0,
			wordmarkSize: m?m.fontSize:"",
			wordmarkWeight: m?m.fontWeight:"",
		});})()`

	var boardBox, relayBox string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.Sleep(1800*time.Millisecond),
		chromedp.Evaluate(probe, &boardBox),
		chromedp.Navigate(base+"/meet/"),
		chromedp.Sleep(3500*time.Millisecond),
		chromedp.Evaluate(probe, &relayBox),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "relay metrics", errs())
	t.Logf("board header = %s", boardBox)
	t.Logf("relay header = %s", relayBox)

	if boardBox == "NO HEADER" || relayBox == "NO HEADER" {
		t.Fatalf("missing a header (board=%s relay=%s)", boardBox, relayBox)
	}
	if boardBox != relayBox {
		t.Errorf("relay's header does not match board's.\n board = %s\n relay = %s\n"+
			"They are styled by separate stylesheets — app.css and the meet SPA's "+
			"index.css — so a change to one must be made in both.", boardBox, relayBox)
	}
}

// relay is FIVE parts, in the console's shape: one bar across the top, then
// left / middle / right beneath it, then the footer.
//
// It used to be three columns with the app bar inside the MIDDLE one — the
// header began 280px in (measured in a live browser) and the app carried two
// brands, one in the bar and one in the sidebar. And being a mounted SPA it
// received neither the all-apps control nor the copyright footer that
// injectChrome gives every embedded app.
func TestDOMRelayIsFiveParts(t *testing.T) {
	base, ctx, errs := domEnv(t, Options{})

	var shape, brands, footer string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/meet/"),
		chromedp.Sleep(3500*time.Millisecond),
		chromedp.Evaluate(`(()=>{const h=document.querySelector("header");
			const f=document.querySelector(".console-foot,#app-foot");
			const b=document.body.getBoundingClientRect();
			const hr=h?h.getBoundingClientRect():null;
			return JSON.stringify({
				headerLeft: hr?Math.round(hr.left):-1,
				headerSpansPage: hr?Math.round(hr.width)===Math.round(b.width):false,
				hasFooter: !!f,
			});})()`, &shape),
		// One brand for the app, not one per panel.
		chromedp.Evaluate(`String(document.querySelectorAll(".console-brand").length)`, &brands),
		chromedp.Evaluate(`(document.querySelector(".console-foot")?.innerText||"").slice(0,60)`, &footer),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "relay shape", errs())
	t.Logf("relay shape = %s", shape)

	if !strings.Contains(shape, `"headerLeft":0`) {
		t.Errorf("relay's app bar does not start at the page edge: %s — "+
			"it is inside a column instead of above all of them", shape)
	}
	if !strings.Contains(shape, `"headerSpansPage":true`) {
		t.Errorf("relay's app bar does not span the page: %s", shape)
	}
	if !strings.Contains(shape, `"hasFooter":true`) {
		t.Error("relay has no footer; injectChrome gives every embedded app one")
	}
	if brands != "1" {
		t.Errorf("relay shows %s brands; the app is named once, in the bar", brands)
	}
	if footer != "© 2026 qiangli. All rights reserved." {
		t.Errorf("relay's footer is not the console copyright line: %q", footer)
	}
}

// The three panels' own headers must be the same height, so one unbroken rule
// runs under the app bar instead of a ragged step between columns. They are
// three separate components and drifted to 68/56/68 once already.
func TestDOMRelayPanelHeadersLineUp(t *testing.T) {
	base, ctx, errs := domEnv(t, Options{})

	var heights string
	if err := chromedp.Run(ctx,
		// A desktop viewport, or there is nothing to line up: the sidebar is
		// hidden below lg and the details panel below xl, so a narrow window
		// measures them at height 0 and the check passes for the wrong reason.
		emulation.SetDeviceMetricsOverride(1600, 900, 1, false),
		chromedp.Navigate(base+"/meet/"),
		chromedp.Sleep(3500*time.Millisecond),
		chromedp.Evaluate(`(()=>{const sel="aside > div:first-child, main > header, [class*=border-l] > div:first-child";
			const hs=[...document.querySelectorAll(sel)].map(e=>Math.round(e.getBoundingClientRect().height));
			return JSON.stringify(hs);})()`, &heights),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "relay panels", errs())
	t.Logf("panel header heights = %s", heights)

	var hs []int
	if err := json.Unmarshal([]byte(heights), &hs); err != nil || len(hs) < 3 {
		t.Fatalf("expected three panel headers, got %s", heights)
	}
	for _, h := range hs {
		if h == 0 {
			t.Fatalf("a panel header measured 0 — the panel is hidden, so this "+
				"would pass without comparing anything: %v", hs)
		}
	}
	for _, h := range hs[1:] {
		if h != hs[0] {
			t.Errorf("the panels' headers are different heights %v — the rule under the "+
				"app bar steps between columns", hs)
			break
		}
	}
}

// relay must follow the console's theme.
//
// The console stores the choice under its own localStorage key and puts
// data-theme on <html>; every other app keys off that attribute. relay keyed
// dark off a `.dark` class that nothing defined and nothing set, so it had no
// dark mode at all — measured: with the console dark and the system dark, relay
// still painted near-white (oklch(0.985 …)).
func TestDOMRelayFollowsTheConsoleTheme(t *testing.T) {
	base, ctx, errs := domEnv(t, Options{})

	read := `JSON.stringify({
		theme: document.documentElement.getAttribute("data-theme"),
		bg: getComputedStyle(document.body).backgroundColor,
	})`
	set := func(theme string) chromedp.Action {
		return chromedp.Evaluate(
			`localStorage.setItem("bashy.apps.config", JSON.stringify({theme:`+
				strconv.Quote(theme)+`}))`, nil)
	}

	var dark, light string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/meet/"),
		chromedp.Sleep(1500*time.Millisecond),
		set("dark"),
		chromedp.Navigate(base+"/meet/"),
		chromedp.Sleep(2500*time.Millisecond),
		chromedp.Evaluate(read, &dark),
		set("light"),
		chromedp.Navigate(base+"/meet/"),
		chromedp.Sleep(2500*time.Millisecond),
		chromedp.Evaluate(read, &light),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "relay theme", errs())
	t.Logf("console dark  -> relay %s", dark)
	t.Logf("console light -> relay %s", light)

	if !strings.Contains(dark, `"theme":"dark"`) {
		t.Errorf("relay ignored the console's dark choice: %s", dark)
	}
	// The console's own dark background, so the two apps are the same colour
	// rather than two interpretations of "dark".
	if !strings.Contains(dark, "rgb(9, 9, 11)") {
		t.Errorf("relay's dark background is not the console's #09090b: %s", dark)
	}
	if !strings.Contains(light, `"theme":"light"`) {
		t.Errorf("relay ignored the console's light choice: %s", light)
	}
	if dark == light {
		t.Errorf("relay renders identically in both themes: %s", dark)
	}
}

// Meet and Chat are two views of the sidebar, so they are tabs — both visible,
// the selected one obvious, one tap to switch. They used to be a dropdown
// hanging off the app logo.
func TestDOMRelayMeetChatAreTabs(t *testing.T) {
	base, ctx, errs := domEnv(t, Options{})

	var tabs, labels, selected string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/meet/"),
		chromedp.Sleep(3000*time.Millisecond),
		chromedp.Evaluate(`String(document.querySelectorAll("[role=tab]").length)`, &tabs),
		chromedp.Evaluate(`JSON.stringify([...document.querySelectorAll("[role=tab]")].map(t=>t.innerText.trim()))`, &labels),
		chromedp.Evaluate(`String([...document.querySelectorAll("[role=tab]")].filter(t=>t.getAttribute("aria-selected")==="true").length)`, &selected),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "relay tabs", errs())

	if tabs != "2" {
		t.Errorf("expected two tabs (Meet, Chat), found %s", tabs)
	}
	if !strings.Contains(labels, "Meet") || !strings.Contains(labels, "Chat") {
		t.Errorf("the tabs are not Meet and Chat: %s", labels)
	}
	// A tab strip whose selection is invisible is just two buttons.
	if selected != "1" {
		t.Errorf("%s tabs are marked selected, want exactly 1", selected)
	}
}

// No panel may ignore the theme. relay's sidebar was a dark navy in the LIGHT
// palette — left from this SPA's standalone design, where a permanently dark
// rail was the look. Beside a light console it read as a bug, because nothing
// else in the console has a panel that stays dark when the theme is light.
//
// The mark is checked against BOARD's rather than a literal: the console
// inverts --logo-* between themes, and relay must do whatever the others do.
func TestDOMRelayLightThemeHasNoDarkPanel(t *testing.T) {
	base, ctx, errs := domEnv(t, Options{})

	read := `(()=>{const a=document.querySelector("aside");
		const r=document.querySelector(".console-logo rect, #brand .logo rect");
		return JSON.stringify({
			body: getComputedStyle(document.body).backgroundColor,
			sidebar: a?getComputedStyle(a).backgroundColor:"",
			logo: r?getComputedStyle(r).fill:"",
		});})()`

	var relay, board string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/meet/"),
		chromedp.Sleep(1200*time.Millisecond),
		chromedp.Evaluate(`localStorage.setItem("bashy.apps.config",JSON.stringify({theme:"light"}))`, nil),
		chromedp.Navigate(base+"/meet/"),
		chromedp.Sleep(2500*time.Millisecond),
		chromedp.Evaluate(read, &relay),
		chromedp.Navigate(base+"/sprint/"),
		chromedp.Sleep(1800*time.Millisecond),
		chromedp.Evaluate(read, &board),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "relay light", errs())
	t.Logf("light relay = %s", relay)
	t.Logf("light board = %s", board)

	// The console's light page colour, shared by both.
	if !strings.Contains(relay, "rgb(242, 242, 245)") {
		t.Errorf("relay's light page is not the console's #f2f2f5: %s", relay)
	}
	// A dark rail in a light theme is the defect this pins. White is the
	// console's card surface; anything near-black is the old navy back again.
	if !strings.Contains(relay, `"sidebar":"rgb(255, 255, 255)"`) {
		t.Errorf("relay's sidebar is not a light surface in the light theme: %s", relay)
	}
	// Whatever the console does with the mark, relay does too.
	var rj, bj map[string]string
	if json.Unmarshal([]byte(relay), &rj) == nil && json.Unmarshal([]byte(board), &bj) == nil {
		if rj["logo"] != bj["logo"] {
			t.Errorf("relay's mark is %s but board's is %s — the apps' marks must "+
				"track the same --logo-* tokens", rj["logo"], bj["logo"])
		}
	}
}

// ONE mark: same tile in every app and every theme.
//
// It used to invert — dark tile on light, light tile on dark — so the product's
// mark was two different objects depending on a setting, and the dark tile sat
// as a black block in a light header. The colour is computed rather than picked
// (the contrast table is in app.css beside the tokens): a single tile has to
// read against the light page, against the dark page, AND carry a glyph, and
// #2563eb has the best worst case at 3.85:1 with all three above the 3:1 WCAG
// non-text bar. The intuitive pale blue fails: #7dd3fc scores 1.49 against the
// light page and would all but vanish there.
func TestDOMOneMarkEverywhere(t *testing.T) {
	base, ctx, errs := domEnv(t, Options{})

	tile := `(()=>{const r=document.querySelector(".console-logo rect, #brand .logo rect");
		return r?getComputedStyle(r).fill:"NO MARK";})()`

	seen := map[string]string{}
	for _, theme := range []string{"light", "dark"} {
		for _, page := range []string{"/sprint/", "/mb/", "/inbox/", "/term/", "/meet/"} {
			var got string
			if err := chromedp.Run(ctx,
				chromedp.Navigate(base+page),
				chromedp.Sleep(900*time.Millisecond),
				chromedp.Evaluate(`localStorage.setItem("bashy.apps.config",`+
					`JSON.stringify({theme:`+strconv.Quote(theme)+`}))`, nil),
				chromedp.Navigate(base+page),
				chromedp.Sleep(2200*time.Millisecond),
				chromedp.Evaluate(tile, &got),
			); err != nil {
				t.Fatalf("chromedp %s %s: %v", theme, page, err)
			}
			seen[theme+" "+page] = got
		}
	}
	assertNoJSErrors(t, "one mark", errs())

	var first, firstKey string
	for k, v := range seen {
		t.Logf("%-16s %s", k, v)
		if v == "NO MARK" {
			t.Errorf("%s has no mark", k)
			continue
		}
		if first == "" {
			first, firstKey = v, k
			continue
		}
		if v != first {
			t.Errorf("the mark differs: %s is %s but %s is %s — one mark, every app, "+
				"every theme", k, v, firstKey, first)
		}
	}
	// And it is the computed colour, not whatever happens to be consistent.
	if first != "rgb(37, 99, 235)" {
		t.Errorf("the mark is %s, want the computed #2563eb", first)
	}
}

// The Files panel must follow the console's theme too.
//
// It is a mounted third-party SPA (File Browser) with its OWN theme mechanism:
// a server-injected setting, else the media preference, written to
// documentElement.className. Nothing in that chain can see the console's
// choice, so Files sat dark while every other app was light — reported as
// "the files app always shows in dark mode".
//
// Same fix as relay: the served template resolves the console's stored choice
// before the bundle boots and writes it where this app already looks for it.
func TestDOMFilesFollowsTheConsoleTheme(t *testing.T) {
	base, ctx, errs := domEnv(t, Options{})

	read := `JSON.stringify({
		cls: document.documentElement.className,
		bg: getComputedStyle(document.body).backgroundColor,
	})`
	set := func(theme string) chromedp.Action {
		return chromedp.Evaluate(
			`localStorage.setItem("bashy.apps.config", JSON.stringify({theme:`+
				strconv.Quote(theme)+`}))`, nil)
	}

	var dark, light string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/files/"),
		chromedp.Sleep(1500*time.Millisecond),
		set("dark"),
		chromedp.Navigate(base+"/files/"),
		chromedp.Sleep(2500*time.Millisecond),
		chromedp.Evaluate(read, &dark),
		set("light"),
		chromedp.Navigate(base+"/files/"),
		chromedp.Sleep(2500*time.Millisecond),
		chromedp.Evaluate(read, &light),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "files theme", errs())
	t.Logf("console dark  -> files %s", dark)
	t.Logf("console light -> files %s", light)

	if !strings.Contains(dark, `"cls":"dark"`) {
		t.Errorf("files ignored the console's dark choice: %s", dark)
	}
	if !strings.Contains(light, `"cls":"light"`) {
		t.Errorf("files ignored the console's light choice: %s", light)
	}
	if dark == light {
		t.Errorf("files renders identically in both themes: %s", dark)
	}
}

// stubBoardWith serves ONE caller-supplied board, for the cases whose subject
// is a board shape the default fixture does not have — an unowned sprint, a
// story in a particular state. It is the same seam stubBoard uses; only the
// payload differs.
func stubBoardWith(t *testing.T, make func() *board.Board) {
	t.Helper()
	orig := collectBoardFn
	t.Cleanup(func() { collectBoardFn = orig })
	collectBoardFn = func(context.Context) (*board.Board, error) { return make(), nil }
}

// stubBoard makes the Sprint page deterministic: one live sprint carrying two
// open and two closed stories. Without it these assertions would be about
// whatever the developer's host happens to be working on.
func stubBoard(t *testing.T) {
	t.Helper()
	orig := collectBoardFn
	t.Cleanup(func() { collectBoardFn = orig })
	collectBoardFn = func(context.Context) (*board.Board, error) {
		return &board.Board{
			SchemaVersion: board.SchemaVersion, Role: "steward", Scope: "machine-global",
			Title: "Bashy Steward Board", GeneratedAt: time.Now().UTC(),
			Sprints: []board.Sprint{{ID: 42, Title: "A sprint with stories", Column: "doing",
				Manager: "project-agent", MeetRoomRef: "2026-room-ref-42",
				StoryTotal: 4, StoryOpen: 2, StoryClosed: 2,
				Continuity: "STATE: mid-flight\n\nNEXT ACTION: keep going"}},
			Todos: []board.Todo{
				{ID: "aaaa1111", Number: 1, Title: "still open", Status: "todo", SprintID: 42},
				{ID: "bbbb2222", Number: 2, Title: "in progress", Status: "doing", SprintID: 42},
				{ID: "cccc3333", Number: 3, Title: "finished", Status: "done", SprintID: 42},
				{ID: "dddd4444", Number: 4, Title: "also finished", Status: "closed", SprintID: 42},
			},
			Resources: &resources.System{
				CPU: resources.CPU{UsagePercent: 31}, Memory: resources.Memory{UsedPercent: 42},
				Disks: []resources.Disk{{Mount: "/", UsedPercent: 57}, {Mount: "/data", UsedPercent: 73}},
			},
			Panels: []board.PanelView{{ID: "agents", Title: "Agents"}, {ID: "runs", Title: "Runs"},
				{ID: "workspaces", Title: "Workspaces", Collapsed: "1 workspace(s); 3.0GiB on disk",
					Columns: []string{"RUN", "STATE", "DISK", "REPO", "WORKSPACE"},
					Rows:    [][]string{{"#7", "working", "3.0GiB", "/repo", "/work/issue-7"}}},
				{ID: "utilization", Title: "Utilization health"}},
		}, nil
	}
}

func TestDOMSprintLinksToItsMeetRoomAndManagerChat(t *testing.T) {
	stubBoard(t)
	base, ctx, errs := domEnv(t, Options{})

	var roomHref, roomLabel, dmHref, dmLabel, manager, newSprintHref, newSprintLabel, newSprintDraft string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.Sleep(2*time.Second),
		chromedp.Evaluate(`document.querySelector('.sprint-title .conversation-link')?.href || ''`, &roomHref),
		chromedp.Evaluate(`document.querySelector('.sprint-title .conversation-link')?.getAttribute('aria-label') || ''`, &roomLabel),
		chromedp.Evaluate(`document.querySelector('.manager .conversation-link')?.href || ''`, &dmHref),
		chromedp.Evaluate(`document.querySelector('.manager .conversation-link')?.getAttribute('aria-label') || ''`, &dmLabel),
		chromedp.Evaluate(`document.querySelector('.bd-sprint .manager')?.textContent || ''`, &manager),
		chromedp.Evaluate(`document.querySelector('#bd-sprints .new-sprint-link')?.href || ''`, &newSprintHref),
		chromedp.Evaluate(`document.querySelector('#bd-sprints .new-sprint-link')?.textContent || ''`, &newSprintLabel),
		chromedp.Evaluate(`new URL(document.querySelector('#bd-sprints .new-sprint-link').href).searchParams.get('draft') || ''`, &newSprintDraft),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "sprint conversation links", errs())

	if !strings.Contains(roomHref, "/meet/?room=2026-room-ref-42") {
		t.Errorf("sprint room href = %q", roomHref)
	}
	if roomLabel != "Open sprint 42 Meet room" {
		t.Errorf("sprint room label = %q", roomLabel)
	}
	if !strings.Contains(dmHref, "/meet/?dm=project-agent") {
		t.Errorf("manager chat href = %q", dmHref)
	}
	if dmLabel != "Chat 1:1 with project-agent" {
		t.Errorf("manager chat label = %q", dmLabel)
	}
	if !strings.Contains(manager, "project manager project-agent") {
		t.Errorf("manager line = %q", manager)
	}
	if !strings.Contains(newSprintHref, "/meet/?chat=1&draft=") {
		t.Errorf("new sprint href = %q", newSprintHref)
	}
	if newSprintLabel != "New sprint" {
		t.Errorf("new sprint label = %q", newSprintLabel)
	}
	if !strings.HasPrefix(newSprintDraft, "Create a new sprint.") {
		t.Errorf("new sprint draft = %q", newSprintDraft)
	}
}

// THE GLYPH MUST AGREE WITH THE DESTINATION.
//
// The regression this pins: conversationLink drew the speech bubble only when
// kind was exactly "dm", so the "chat" kind fell through to the hash — the
// same mark the composer uses for "Everyone". The one control that starts a
// conversation with a single agent was drawn as a broadcast marker.
//
// Asserting on the path DATA rather than on a class, because a class can be
// right while the shape is wrong, and the shape is the thing a reader sees.
func TestDOMConversationMarksMatchTheirDestinations(t *testing.T) {
	stubBoard(t)
	base, ctx, errs := domEnv(t, Options{})

	const (
		channel      = "M4 9h16M4 15h16M10 3 8 21M16 3l-2 18"
		conversation = "M21 15a4 4 0 0 1-4 4H8l-5 3V7a4 4 0 0 1 4-4h10a4 4 0 0 1 4 4z"
	)
	var roomMark, dmMark, newSprintMark, newSprintLabel, newSprintHref string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.Sleep(2*time.Second),
		chromedp.Evaluate(`document.querySelector('.sprint-title .conversation-link path')?.getAttribute('d') || ''`, &roomMark),
		chromedp.Evaluate(`document.querySelector('.manager .conversation-link path')?.getAttribute('d') || ''`, &dmMark),
		chromedp.Evaluate(`document.querySelector('#bd-sprints .new-sprint-link path')?.getAttribute('d') || ''`, &newSprintMark),
		chromedp.Evaluate(`document.querySelector('#bd-sprints .new-sprint-link')?.getAttribute('aria-label') || ''`, &newSprintLabel),
		chromedp.Evaluate(`document.querySelector('#bd-sprints .new-sprint-link')?.href || ''`, &newSprintHref),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "conversation marks", errs())

	// A Meet room IS a channel, so it keeps the channel mark.
	if roomMark != channel {
		t.Errorf("sprint room mark = %q, want the channel mark", roomMark)
	}
	// A manager chat is one agent.
	if dmMark != conversation {
		t.Errorf("manager chat mark = %q, want the conversation mark", dmMark)
	}
	// THE DEFECT: this one was the channel mark.
	if newSprintMark != conversation {
		t.Errorf("new sprint mark = %q, want the conversation mark", newSprintMark)
	}
	// And the label says which of the two it is, so the mark is not the only
	// thing carrying it.
	if !strings.Contains(newSprintLabel, "1:1") {
		t.Errorf("new sprint label = %q, want it to name the 1:1", newSprintLabel)
	}
	// The destination is unchanged and still carries the draft: chat=1 opens
	// Chat's agent picker, and the chosen agent becomes the 1:1.
	if !strings.Contains(newSprintHref, "/meet/?chat=1&draft=") {
		t.Errorf("new sprint href = %q", newSprintHref)
	}
}

// AN UNOWNED SPRINT MUST SAY SO, and must offer the way out.
//
// The regression this pins: sprintEl appended the manager row only `if
// (manager)`, so absence rendered as absence. `bashy sprint show` calls the
// same state "UNREACHABLE: no owner — nobody is accountable and no name can be
// addressed"; the card rendered nothing at all, on the one surface that sees
// every sprint at once.
//
// The fixture carries BOTH an owned and an unowned sprint, because the failure
// mode of a fix here is flattening the two into one treatment.
func TestDOMUnownedSprintSaysSoAndOffersToStaffIt(t *testing.T) {
	stubBoardWith(t, func() *board.Board {
		return &board.Board{
			SchemaVersion: board.SchemaVersion, Role: "steward", Scope: "machine-global",
			Title: "Bashy Steward Board", GeneratedAt: time.Now().UTC(),
			Sprints: []board.Sprint{
				{ID: 7, Title: "An owned sprint", Column: "doing", Manager: "project-agent"},
				{ID: 8, Title: "An unowned sprint", Column: "backlog"},
			},
		}
	})
	base, ctx, errs := domEnv(t, Options{})

	var owned, unowned, assignHref, assignLabel, assignDraft, assignMark string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.Sleep(2*time.Second),
		chromedp.Evaluate(`document.querySelectorAll('.bd-sprint')[0]?.querySelector('.manager')?.textContent || ''`, &owned),
		chromedp.Evaluate(`document.querySelectorAll('.bd-sprint')[1]?.querySelector('.manager')?.textContent || ''`, &unowned),
		chromedp.Evaluate(`document.querySelector('.manager.unassigned .assign-manager-link')?.href || ''`, &assignHref),
		chromedp.Evaluate(`document.querySelector('.manager.unassigned .assign-manager-link')?.getAttribute('aria-label') || ''`, &assignLabel),
		chromedp.Evaluate(`document.querySelector('.manager.unassigned .assign-manager-link') ? new URL(document.querySelector('.manager.unassigned .assign-manager-link').href).searchParams.get('draft') || '' : ''`, &assignDraft),
		chromedp.Evaluate(`document.querySelector('.manager.unassigned .assign-manager-link path')?.getAttribute('d') || ''`, &assignMark),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "unowned sprint", errs())

	// The owned card is UNCHANGED. A fix that makes every card say something
	// about its manager has not distinguished the two states.
	if !strings.Contains(owned, "project manager project-agent") {
		t.Errorf("owned manager line = %q", owned)
	}
	if strings.Contains(owned, "unassigned") {
		t.Errorf("owned card claims to be unassigned: %q", owned)
	}
	// THE DEFECT: this row did not exist.
	if !strings.Contains(unowned, "project manager — unassigned") {
		t.Errorf("unowned manager line = %q, want it to state the gap", unowned)
	}
	// And it offers the way out, as a conversation rather than a channel.
	if !strings.Contains(assignHref, "/meet/?chat=1&draft=") {
		t.Errorf("assign href = %q", assignHref)
	}
	if !strings.Contains(assignLabel, "sprint 8") {
		t.Errorf("assign label = %q, want it to name the sprint", assignLabel)
	}
	// The draft carries the sprint the agent is being asked about — it has no
	// other way to know which one — and stays editable.
	if !strings.Contains(assignDraft, "#8") || !strings.Contains(assignDraft, "An unowned sprint") {
		t.Errorf("assign draft = %q, want it to name sprint 8", assignDraft)
	}
	if assignMark != "M21 15a4 4 0 0 1-4 4H8l-5 3V7a4 4 0 0 1 4-4h10a4 4 0 0 1 4 4z" {
		t.Errorf("assign mark = %q, want the conversation mark", assignMark)
	}
}

// A STORY BODY IS ORDINARY MARKDOWN and must survive to the DOM as itself.
//
// The regression this pins: openStory pushed the body through
// continuitySections — the splitter written for a conductor's continuity
// brief, which splits on blank lines and relabels any paragraph opening with
// ALL-CAPS and a colon. Every heading, list, fence and indented transcript was
// flattened into label/value pairs. The story bodies in this repo carry
// command transcripts; those were the worst casualties.
//
// The fixture below is deliberately one of each, INCLUDING a "NOTE:"
// paragraph, because that is the shape the old splitter would silently eat.
func TestDOMStoryBodyKeepsItsMarkdownShape(t *testing.T) {
	stubBoardWith(t, func() *board.Board {
		return &board.Board{
			SchemaVersion: board.SchemaVersion, Role: "steward", Scope: "machine-global",
			Title: "Bashy Steward Board", GeneratedAt: time.Now().UTC(),
			Sprints: []board.Sprint{{ID: 5, Title: "A sprint", Column: "doing", Manager: "pm"}},
			Todos:   []board.Todo{{ID: "story01", Number: 1, Title: "shaped body", Status: "todo", SprintID: 5}},
		}
	})
	origStory := storyDetailFn
	t.Cleanup(func() { storyDetailFn = origStory })
	storyDetailFn = func(board.Todo) (*board.Story, error) {
		return &board.Story{
			ID: "story01", Seq: 1, Title: "shaped body", Status: "todo", Priority: "p1",
			Body: "# The heading\n\n" +
				"An ordinary paragraph that\nwraps across two source lines.\n\n" +
				"- first bullet\n- second bullet\n\n" +
				"```\nfenced code line\n  indented inside the fence\n```\n\n" +
				"    $ bashy sprint status\n    ON THE CLOCK (2)\n\n" +
				"NOTE: a paragraph that opens like a continuity label.\n",
		}, nil
	}
	base, ctx, errs := domEnv(t, Options{})

	var heading, paras, bullets, code, overflow string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.Sleep(2*time.Second),
		chromedp.Click(`.bd-sprint .refs .ref.link`, chromedp.ByQuery),
		chromedp.Sleep(1*time.Second),
		chromedp.Evaluate(`document.querySelector('.story-body .story-h')?.textContent || ''`, &heading),
		chromedp.Evaluate(`Array.from(document.querySelectorAll('.story-body .story-p')).map(n => n.textContent).join('|')`, &paras),
		chromedp.Evaluate(`Array.from(document.querySelectorAll('.story-body .story-list li')).map(n => n.textContent).join('|')`, &bullets),
		chromedp.Evaluate(`Array.from(document.querySelectorAll('.story-body .story-code')).map(n => n.textContent).join('~~')`, &code),
		chromedp.Evaluate(`getComputedStyle(document.querySelector('.story-body')).overflowY`, &overflow),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "story body", errs())

	if heading != "The heading" {
		t.Errorf("heading = %q", heading)
	}
	// A wrapped paragraph is ONE paragraph, joined — the source line break is
	// not a break in the prose.
	if !strings.Contains(paras, "An ordinary paragraph that wraps across two source lines.") {
		t.Errorf("paragraphs = %q", paras)
	}
	// THE OLD SPLITTER'S FAVOURITE VICTIM: this must stay a paragraph, not
	// become a "NOTE" label with a value beside it.
	if !strings.Contains(paras, "NOTE: a paragraph that opens like a continuity label.") {
		t.Errorf("the NOTE paragraph was relabelled or lost: %q", paras)
	}
	if bullets != "first bullet|second bullet" {
		t.Errorf("bullets = %q", bullets)
	}
	// The fence keeps its interior indentation, and the indented transcript is
	// a block of its own rather than two paragraphs.
	if !strings.Contains(code, "fenced code line\n  indented inside the fence") {
		t.Errorf("fenced code = %q", code)
	}
	if !strings.Contains(code, "$ bashy sprint status") || !strings.Contains(code, "ON THE CLOCK (2)") {
		t.Errorf("indented transcript = %q", code)
	}
	// Nothing is cut: a long record scrolls inside its pane.
	if overflow != "auto" && overflow != "scroll" {
		t.Errorf("story body overflow-y = %q, want it to scroll rather than clip", overflow)
	}
}

// A STORY'S STAGE, and the worker behind it.
//
// The regression this pins: storyIsClosed sorted every story into done-or-not,
// so a story with an agent on it right now rendered identically to one nobody
// had touched. The board's whole job is answering "what is moving?".
//
// The second half is the harder one and is asserted just as hard: a weave run
// does NOT record the story it executes, so the correlation is by agent
// identity within the sprint and its LIMITS must be reported rather than
// papered over. Three outcomes, three fixtures: one run (named), two runs
// (ambiguous), no run (said so).
func TestDOMStoryStagesAndWorkerCorrelation(t *testing.T) {
	stubBoardWith(t, func() *board.Board {
		return &board.Board{
			SchemaVersion: board.SchemaVersion, Role: "steward", Scope: "machine-global",
			Title: "Bashy Steward Board", GeneratedAt: time.Now().UTC(),
			Sprints: []board.Sprint{{ID: 9, Title: "Staged", Column: "doing", Manager: "pm"}},
			Todos: []board.Todo{
				{ID: "s1", Number: 1, Title: "nobody has this", Status: "todo", SprintID: 9},
				{ID: "s2", Number: 2, Title: "assigned not started", Status: "todo", SprintID: 9, Assignee: "solo"},
				{ID: "s3", Number: 3, Title: "being worked", Status: "doing", SprintID: 9, Assignee: "solo"},
				{ID: "s4", Number: 4, Title: "waiting on review", Status: "submitted", SprintID: 9, Assignee: "twin"},
				{ID: "s5", Number: 5, Title: "finished", Status: "done", SprintID: 9},
			},
			Runs: []board.Run{
				{ID: 11, Repo: "coreutils", State: "working", Agent: "solo", Tool: "claude", Model: "opus5", Band: 4, SprintID: 9, AgeSeconds: 120},
				{ID: 12, Repo: "coreutils", State: "working", Agent: "twin", SprintID: 9},
				{ID: 13, Repo: "bashy", State: "working", Agent: "twin", SprintID: 9},
			},
		}
	})
	stories := map[string]*board.Story{
		"s2": {ID: "s2", Seq: 2, Title: "assigned not started", Status: "todo", Assignee: "solo", Sprint: 9, Body: "b"},
		"s4": {ID: "s4", Seq: 4, Title: "waiting on review", Status: "submitted", Assignee: "twin", Sprint: 9, Body: "b"},
		"s5": {ID: "s5", Seq: 5, Title: "finished", Status: "done", Sprint: 9, Body: "b"},
	}
	origStory := storyDetailFn
	t.Cleanup(func() { storyDetailFn = origStory })
	storyDetailFn = func(td board.Todo) (*board.Story, error) {
		if st, ok := stories[td.ID]; ok {
			return st, nil
		}
		return nil, fmt.Errorf("no fixture for %s", td.ID)
	}
	base, ctx, errs := domEnv(t, Options{})

	var stages, oneRun, ambiguous, unassigned string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.Sleep(2*time.Second),
		chromedp.Evaluate(`Array.from(document.querySelectorAll('.bd-sprint .refs .ref.link'))
			.map(n => n.textContent + "=" + Array.from(n.classList).filter(c => c.startsWith('stage-')).join('')).join(",")`, &stages),
		// ONE run correlates: it is named, with its tool, model and band.
		chromedp.Evaluate(`(() => { document.querySelectorAll('.bd-sprint .refs .ref.link')[1].click(); return "" })()`, nil),
		chromedp.Sleep(1*time.Second),
		chromedp.Evaluate(`document.querySelector('.story-detail')?.textContent || ''`, &oneRun),
		// TWO runs under one agent: ambiguous, and it says so.
		chromedp.Evaluate(`(() => { document.querySelectorAll('.bd-sprint .refs .ref.link')[3].click(); return "" })()`, nil),
		chromedp.Sleep(1*time.Second),
		chromedp.Evaluate(`document.querySelector('.story-detail')?.textContent || ''`, &ambiguous),
		// No assignee at all.
		chromedp.Evaluate(`(() => { document.querySelectorAll('.bd-sprint .refs .ref.link')[4].click(); return "" })()`, nil),
		chromedp.Sleep(1*time.Second),
		chromedp.Evaluate(`document.querySelector('.story-detail')?.textContent || ''`, &unassigned),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "story stages", errs())

	// FIVE distinct stages, not two.
	for _, want := range []string{
		"#1=stage-unstarted", "#2=stage-assigned", "#3=stage-working",
		"#4=stage-needs", "#5=stage-closed",
	} {
		if !strings.Contains(stages, want) {
			t.Errorf("stage chips = %q, missing %s", stages, want)
		}
	}
	if !strings.Contains(oneRun, "@solo") {
		t.Errorf("worker line = %q", oneRun)
	}
	if !strings.Contains(oneRun, "coreutils#11") || !strings.Contains(oneRun, "claude:opus5") ||
		!strings.Contains(oneRun, "L4") {
		t.Errorf("correlated run = %q, want it to name the run, binding and band", oneRun)
	}
	// THE LIMIT IS REPORTED. Two runs under one agent cannot be told apart, and
	// picking one would be a guess an operator would act on.
	if !strings.Contains(ambiguous, "cannot tell which is this story") {
		t.Errorf("ambiguous correlation = %q", ambiguous)
	}
	if !strings.Contains(unassigned, "unassigned — nobody is working this story") {
		t.Errorf("unassigned story detail = %q", unassigned)
	}
}

// A SPRINT'S PLAN IS ON THE RECORD AND WAS INVISIBLE.
//
// The gap was two layers deep, which is why the fix is not only a render:
// weaveStory.SpecRef exists and `sprint show` prints it as "spec:", but
// board.Sprint did not carry the field at all, so the browser could not have
// shown it even if it wanted to. This asserts both halves — the field survives
// the projection, and the card renders it.
//
// It is a REFERENCE, not an anchor, and that is asserted too: SpecRef is
// repo-relative and a sprint spans repos, so any href built here would work on
// one host and 404 behind the tunnel on another.
func TestDOMSprintShowsItsPlanReference(t *testing.T) {
	stubBoardWith(t, func() *board.Board {
		return &board.Board{
			SchemaVersion: board.SchemaVersion, Role: "steward", Scope: "machine-global",
			Title: "Bashy Steward Board", GeneratedAt: time.Now().UTC(),
			Sprints: []board.Sprint{
				{ID: 3, Title: "Has a plan", Column: "doing", Manager: "pm", SpecRef: "docs/master-execution-plan.md"},
				{ID: 4, Title: "Has no plan", Column: "doing", Manager: "pm"},
			},
		}
	})
	base, ctx, errs := domEnv(t, Options{})

	var planText, planRef, planRows, anchors string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.Sleep(2*time.Second),
		chromedp.Evaluate(`document.querySelector('.bd-sprint .plan')?.textContent || ''`, &planText),
		chromedp.Evaluate(`document.querySelector('.bd-sprint .plan .plan-ref')?.textContent || ''`, &planRef),
		chromedp.Evaluate(`String(document.querySelectorAll('.bd-sprint .plan').length)`, &planRows),
		chromedp.Evaluate(`String(document.querySelectorAll('.bd-sprint .plan a').length)`, &anchors),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "sprint plan", errs())

	if !strings.Contains(planText, "plan") {
		t.Errorf("plan row = %q, want it labelled", planText)
	}
	// THE FIELD SURVIVED THE PROJECTION. Without the model change this is "".
	if planRef != "docs/master-execution-plan.md" {
		t.Errorf("plan ref = %q", planRef)
	}
	// A sprint with no spec gains NO row — an empty label is worse than nothing.
	if planRows != "1" {
		t.Errorf("plan rows = %s, want exactly one across two sprints", planRows)
	}
	// No dead link: a repo-relative path cannot be an href that resolves on
	// both loopback and a proxied host.
	if anchors != "0" {
		t.Errorf("plan row rendered %s anchor(s); a repo-relative path must not become a link", anchors)
	}
}

func TestDOMSprintShowsDiskAndWorkspaceUsage(t *testing.T) {
	stubBoard(t)
	base, ctx, errs := domEnv(t, Options{})

	var summary, panels string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.Sleep(2*time.Second),
		chromedp.Evaluate(`Array.from(document.querySelectorAll("#bd-summary .bd-stat"))
			.map(n => n.querySelector(".k").textContent + "=" + n.querySelector(".v").textContent).join(",")`, &summary),
		chromedp.Evaluate(`Array.from(document.querySelectorAll(".bd-panel-head"))
			.map(n => n.textContent.trim()).join("|")`, &panels),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "sprint disk and workspaces", errs())
	if !strings.Contains(summary, "cpu=31%") || !strings.Contains(summary, "memory=42%") || !strings.Contains(summary, "disk=73%") {
		t.Errorf("resource summary = %q", summary)
	}
	if strings.Index(summary, "memory=42%") > strings.Index(summary, "disk=73%") {
		t.Errorf("disk does not follow memory: %q", summary)
	}
	if !strings.Contains(panels, "Workspaces") || !strings.Contains(panels, "3.0GiB on disk") {
		t.Errorf("workspace panel is absent: %q", panels)
	}
	if strings.Index(panels, "Runs") > strings.Index(panels, "Workspaces") || strings.Index(panels, "Workspaces") > strings.Index(panels, "Utilization health") {
		t.Errorf("workspace panel placement = %q", panels)
	}
}

// A sprint's STORIES must be on the card, its progress must be readable
// without a click, and a closed story must not look like an open one.
//
// The regression this pins: the board's todo source asked its personal-list
// store for `--owner`, todo rejected the flag, and the source returned on that
// FIRST failure — so a host with 183 stories served zero, and every sprint
// card rendered with no story chips and nothing to click. Byte-level tests saw
// nothing: the page's markup was unchanged, only its data was empty.
func TestDOMSprintCardShowsItsStories(t *testing.T) {
	stubBoard(t)
	base, ctx, errs := domEnv(t, Options{})

	var chips, headStat, openColor, closedColor string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.Sleep(2*time.Second),
		chromedp.Evaluate(`Array.from(document.querySelectorAll(".bd-sprint .refs .ref"))
			.map(n => n.textContent).join(",")`, &chips),
		chromedp.Evaluate(`(document.querySelector(".bd-sprint .stories")||{}).textContent || "NO STAT"`, &headStat),
		chromedp.Evaluate(`(() => {
			const open = document.querySelector(".bd-sprint .refs .ref:not(.past)");
			return open ? getComputedStyle(open).color : "NONE";
		})()`, &openColor),
		chromedp.Evaluate(`(() => {
			const past = document.querySelector(".bd-sprint .refs .ref.past");
			return past ? getComputedStyle(past).color + "|" + getComputedStyle(past).textDecorationLine : "NONE";
		})()`, &closedColor),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "sprint", errs())

	for _, want := range []string{"#1", "#2", "#3", "#4"} {
		if !strings.Contains(chips, want) {
			t.Errorf("story %s is not on the card; chips = %q", want, chips)
		}
	}
	if !strings.Contains(headStat, "2 open") || !strings.Contains(headStat, "2 closed") {
		t.Errorf("the card does not state its open/closed split: %q", headStat)
	}
	if openColor == "NONE" || closedColor == "NONE" {
		t.Fatalf("missing an open or a closed chip (open=%s closed=%s)", openColor, closedColor)
	}
	if !strings.Contains(closedColor, "line-through") {
		t.Errorf("a closed story is not struck through: %s", closedColor)
	}
	if strings.HasPrefix(closedColor, openColor+"|") {
		t.Errorf("closed and open stories render in the SAME colour (%s) — the one "+
			"distinction a progress display exists to make", openColor)
	}
}

// A story number is a LINK, and clicking it must show that story.
//
// The detail pane is the whole reason the chips are buttons. It is asserted on
// SPEAK rather than on content: these fixture ids do not exist in the host's
// todo store, so the pane reports why it could not read one — which is the
// contract. What must never happen is the third outcome, a pane that opens
// and says nothing at all.
func TestDOMStoryLinkOpensItsDetail(t *testing.T) {
	stubBoard(t)
	base, ctx, errs := domEnv(t, Options{})

	var before, after string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.Sleep(2*time.Second),
		chromedp.Evaluate(`(() => {
			const d = document.querySelector(".bd-sprint .story-detail");
			return d ? String(!d.hidden) : "NO PANE";
		})()`, &before),
		chromedp.Click(`.bd-sprint .refs .ref.link`, chromedp.ByQuery),
		chromedp.Sleep(1500*time.Millisecond),
		chromedp.Evaluate(`(() => {
			const d = document.querySelector(".bd-sprint .story-detail");
			if (!d) return "NO PANE";
			return (d.hidden ? "HIDDEN:" : "SHOWN:") + d.textContent.trim().slice(0, 120);
		})()`, &after),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "story link", errs())

	if before != "false" {
		t.Errorf("the detail pane starts open, before anything was clicked: %s", before)
	}
	if !strings.HasPrefix(after, "SHOWN:") {
		t.Fatalf("clicking a story number opened nothing: %s", after)
	}
	if strings.TrimPrefix(after, "SHOWN:") == "" {
		t.Error("the detail pane opened EMPTY — a pane that shows nothing is " +
			"indistinguishable from a link that does not work")
	}
}

// An expanded disclosure must survive the 15-second poll.
//
// The page re-renders with replaceChildren, so anything a reader opened is
// rebuilt from scratch. The opened story BODY was remembered; the story list
// and the continuity brief were not, so a reader mid-scan had the list shut
// under them on the next tick with no action of their own.
func TestDOMSprintDisclosuresSurviveARefresh(t *testing.T) {
	stubBoard(t)
	base, ctx, errs := domEnv(t, Options{})

	// The toggles are the card's `button.more`, in render order: stories, then
	// continuity. Open both, force a re-render, and read them back.
	const openBoth = `(() => {
		const btns = document.querySelectorAll(".bd-sprint button.more");
		btns.forEach(b => b.click());
		return String(btns.length);
	})()`
	const readState = `(() => {
		const btns = Array.from(document.querySelectorAll(".bd-sprint button.more"));
		return btns.map(b => b.textContent.split(" ")[0] + "=" +
			(b.nextElementSibling && !b.nextElementSibling.hidden)).join(",");
	})()`

	var count, before, after, detail string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.Sleep(2*time.Second),
		chromedp.Evaluate(openBoth, &count),
		chromedp.Evaluate(readState, &before),
		// The same re-render the poll performs, without waiting 15s for it.
		chromedp.Evaluate(`load()`, nil, awaitPromise),
		chromedp.Sleep(500*time.Millisecond),
		chromedp.Evaluate(readState, &after),
		chromedp.Evaluate(`(() => {
			const d = document.querySelector(".bd-sprint .story-detail");
			return d ? String(!d.hidden) : "NONE";
		})()`, &detail),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "sprint refresh", errs())

	if count != "2" {
		t.Fatalf("expected the stories and continuity toggles, got %s", count)
	}
	// The two toggles are separated by their own hidden bodies, so an
	// adjacent-sibling rule never matched them and they rendered as one
	// run-on word ("…of 3continuity — 4 sections").
	var gap string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`(() => {
		const b = document.querySelectorAll(".bd-sprint button.more");
		return getComputedStyle(b[0]).marginRight;
	})()`, &gap)); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	if gap == "0px" {
		t.Error("the stories and continuity toggles have no space between them")
	}
	if !strings.Contains(before, "stories=true") || !strings.Contains(before, "continuity=true") {
		t.Fatalf("the toggles did not open at all: %s", before)
	}
	if before != after {
		t.Errorf("a refresh collapsed what the reader had open.\n before = %s\n after  = %s",
			before, after)
	}
	_ = detail
}

// awaitPromise lets chromedp.Evaluate resolve load()'s promise instead of
// racing the fetch it starts.
func awaitPromise(p *runtime.EvaluateParams) *runtime.EvaluateParams {
	return p.WithAwaitPromise(true)
}

// The Inbox tile's unread badge, in a real browser.
//
// It counts the VIEWER's own unread and nothing else. The panel is a peek at
// every other name's mail — looking there advances no cursor, so an agent's
// backlog falls only when that agent reads — which means a fleet total on this
// tile would be a number the person looking at it can never clear. The fleet
// figure belongs in the tooltip, stated rather than alarmed.
//
// Byte-level tests cannot see this: the badge is created by applyInboxCount at
// render time from a second fetch, so a tile with no badge and a tile whose
// badge never rendered are the same bytes.
func TestDOMInboxTileBadgeCountsTheViewerNotTheFleet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_ROOM_DIR", dir)
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	t.Setenv("USER", "operator")
	for _, e := range []room.Event{
		{Principal: "cairn", To: "operator", Topic: "done", Body: "merged"},
		{Principal: "cairn", To: "operator", Topic: "gate", Body: "86/86"},
		// Four more waiting on OTHER names. None of these may reach the badge.
		{Principal: "operator", To: "cairn", Topic: "sprint", Body: "pick up #12"},
		{Principal: "operator", To: "lintel", Topic: "gate", Body: "rerun"},
		{Principal: "operator", To: "lintel", Topic: "gate", Body: "still red"},
		{Principal: "operator", To: "keystone", Topic: "merge", Body: "ready"},
	} {
		e.Type = room.EventNotify
		if err := room.Notify(e); err != nil {
			t.Fatal(err)
		}
	}

	base, ctx, errs := domEnv(t, Options{})
	var badge, tip string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/"),
		chromedp.Sleep(2200*time.Millisecond),
		chromedp.Evaluate(`(()=>{const t=[...document.querySelectorAll(".tile")]
			.find(x=>(x.querySelector(".label")||{}).textContent==="Inbox");
			return t?((t.querySelector(".count")||{}).textContent||"NO BADGE"):"NO TILE";})()`, &badge),
		chromedp.Evaluate(`(()=>{const t=[...document.querySelectorAll(".tile")]
			.find(x=>(x.querySelector(".label")||{}).textContent==="Inbox");
			return t?(t.getAttribute("title")||""):"NO TILE";})()`, &tip),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "inbox badge", errs())

	if badge != "2" {
		t.Errorf("Inbox badge = %q, want \"2\" — the viewer's own unread, not the fleet's 6", badge)
	}
	if !strings.Contains(tip, "2 waiting for you") {
		t.Errorf("tooltip does not state the viewer's count: %q", tip)
	}
	if !strings.Contains(tip, "6 unread across 4 inboxes") {
		t.Errorf("tooltip does not state the fleet backlog: %q", tip)
	}
	if !strings.Contains(tip, "peek") {
		t.Errorf("tooltip does not say reading here consumes nothing: %q", tip)
	}
}

// With nothing waiting for the viewer there is NO badge, even while the fleet
// has a backlog. A badge the reader cannot clear is one they learn to ignore,
// and every other name's mail is theirs to read, not the viewer's.
func TestDOMInboxTileHasNoBadgeWhenNothingIsWaitingForYou(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_ROOM_DIR", dir)
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	t.Setenv("USER", "operator")
	for _, e := range []room.Event{
		{Principal: "operator", To: "cairn", Topic: "sprint", Body: "pick up #12"},
		{Principal: "operator", To: "lintel", Topic: "gate", Body: "rerun"},
	} {
		e.Type = room.EventNotify
		if err := room.Notify(e); err != nil {
			t.Fatal(err)
		}
	}

	base, ctx, errs := domEnv(t, Options{})
	var badge, tip string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/"),
		chromedp.Sleep(2200*time.Millisecond),
		chromedp.Evaluate(`(()=>{const t=[...document.querySelectorAll(".tile")]
			.find(x=>(x.querySelector(".label")||{}).textContent==="Inbox");
			return t?(t.querySelector(".count")?"BADGE":"none"):"NO TILE";})()`, &badge),
		chromedp.Evaluate(`(()=>{const t=[...document.querySelectorAll(".tile")]
			.find(x=>(x.querySelector(".label")||{}).textContent==="Inbox");
			return t?(t.getAttribute("title")||""):"NO TILE";})()`, &tip),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "inbox badge empty", errs())

	if badge != "none" {
		t.Errorf("Inbox badge = %q with nothing waiting for the viewer, want none", badge)
	}
	// The fleet backlog is still reported — just not as an alarm.
	if !strings.Contains(tip, "2 unread across 2 inboxes") {
		t.Errorf("tooltip does not state the fleet backlog: %q", tip)
	}
}

// seedInbox points the bus stores at a scratch dir and seeds addressed mail.
func seedInbox(t *testing.T, events ...room.Event) {
	t.Helper()
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	t.Setenv("USER", "operator")
	for _, e := range events {
		e.Type = room.EventNotify
		if err := room.Notify(e); err != nil {
			t.Fatal(err)
		}
	}
}

// inboxText reads a selector's text off the open page.
func inboxText(sel string) string {
	return `(document.querySelector(` + "`" + sel + "`" + `)?.innerText||"NONE").trim()`
}

// Mark-read is offered on YOUR inbox and nowhere else.
//
// The server refuses regardless — the route takes no name — but a control that
// is offered and then refused teaches the reader that the page lies. This is
// also the assertion no byte-level test can make: the buttons are created at
// render time from a fetch, so "absent" and "never rendered" are the same bytes.
func TestDOMInboxMarkReadIsOfferedOnlyOnYourOwnInbox(t *testing.T) {
	seedInbox(t,
		room.Event{Principal: "cairn", To: "operator", Topic: "done", Body: "merged"},
		room.Event{Principal: "cairn", To: "operator", Topic: "gate", Body: "86/86"},
		room.Event{Principal: "operator", To: "cairn", Topic: "sprint", Body: "pick up #12"},
	)
	base, ctx, errs := domEnv(t, Options{})

	var mineAll, mineOne, mineFoot, mineMode string
	var peekAll, peekOne, peekFoot, peekMode string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/inbox/#name=operator"),
		chromedp.Sleep(2000*time.Millisecond),
		chromedp.Evaluate(`(()=>{const b=document.getElementById("ib-markall");
			return b&&!b.hidden?b.innerText.trim():"HIDDEN";})()`, &mineAll),
		chromedp.Evaluate(`String(document.querySelectorAll("#ib-list .mark").length)`, &mineOne),
		chromedp.Evaluate(inboxText("#ib-foot"), &mineFoot),
		chromedp.Evaluate(`(()=>{const m=document.getElementById("ib-mode");
			return m&&!m.hidden?m.innerText.trim():"HIDDEN";})()`, &mineMode),

		chromedp.Navigate(base+"/inbox/#name=cairn"),
		chromedp.Sleep(2000*time.Millisecond),
		chromedp.Evaluate(`(()=>{const b=document.getElementById("ib-markall");
			return b&&!b.hidden?b.innerText.trim():"HIDDEN";})()`, &peekAll),
		chromedp.Evaluate(`String(document.querySelectorAll("#ib-list .mark").length)`, &peekOne),
		chromedp.Evaluate(inboxText("#ib-foot"), &peekFoot),
		chromedp.Evaluate(`(()=>{const m=document.getElementById("ib-mode");
			return m&&!m.hidden?m.innerText.trim():"HIDDEN";})()`, &peekMode),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "inbox mark-read offer", errs())

	if mineAll != "Mark all read (2)" {
		t.Errorf("own inbox mark-all = %q, want the count", mineAll)
	}
	if mineOne != "2" {
		t.Errorf("own inbox per-message controls = %s, want 2 (one per unread)", mineOne)
	}
	if !strings.Contains(mineFoot, "Your inbox") {
		t.Errorf("own inbox footer = %q", mineFoot)
	}

	if peekAll != "HIDDEN" {
		t.Errorf("cairn's inbox offers mark-all (%q) — every other name is a peek", peekAll)
	}
	if peekOne != "0" {
		t.Errorf("cairn's inbox offers %s per-message controls, want 0", peekOne)
	}
	if !strings.Contains(peekFoot, "peek") || !strings.Contains(peekFoot, "only cairn can consume it") {
		t.Errorf("peek footer does not say whose mail it is: %q", peekFoot)
	}

	// The header pill must agree with the buttons underneath it. A standing
	// "read-only" banner was true of the whole page until your own inbox gained
	// controls; a banner contradicting the controls below it is worse than none.
	if mineMode != "HIDDEN" {
		t.Errorf("own inbox shows the %q pill while offering mark-read controls", mineMode)
	}
	// innerText is the RENDERED text, so it carries the pill's uppercase
	// transform. Compare on the word, not the casing the stylesheet chose.
	if !strings.EqualFold(peekMode, "peek") {
		t.Errorf("cairn's inbox pill = %q, want peek", peekMode)
	}
}

// Marking works end to end, and the badge on the launcher follows.
//
// "Perform as expected" is the whole requirement here: a person reads their
// mail in a browser and it becomes read, without going to find a terminal.
func TestDOMInboxMarkAllReadClearsYourCountAndTheLauncherBadge(t *testing.T) {
	seedInbox(t,
		room.Event{Principal: "cairn", To: "operator", Topic: "done", Body: "merged"},
		room.Event{Principal: "cairn", To: "operator", Topic: "gate", Body: "86/86"},
		room.Event{Principal: "operator", To: "cairn", Topic: "sprint", Body: "pick up #12"},
	)
	base, ctx, errs := domEnv(t, Options{})

	var before, after, fleetAfter, badgeAfter string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/inbox/#name=operator"),
		chromedp.Sleep(2000*time.Millisecond),
		chromedp.Evaluate(inboxText("#ib-stats"), &before),
		chromedp.Click("#ib-markall", chromedp.ByID),
		chromedp.Sleep(1800*time.Millisecond),
		chromedp.Evaluate(inboxText("#ib-stats"), &after),
		// cairn still has its message: marking mine read consumed nothing of theirs.
		chromedp.Navigate(base+"/inbox/#name=cairn"),
		chromedp.Sleep(1800*time.Millisecond),
		chromedp.Evaluate(inboxText("#ib-stats"), &fleetAfter),
		// And the launcher badge, which reads the viewer's count, is gone.
		chromedp.Navigate(base+"/"),
		chromedp.Sleep(2000*time.Millisecond),
		chromedp.Evaluate(`(()=>{const t=[...document.querySelectorAll(".tile")]
			.find(x=>(x.querySelector(".label")||{}).textContent==="Inbox");
			return t?(t.querySelector(".count")?"BADGE":"none"):"NO TILE";})()`, &badgeAfter),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "inbox mark all", errs())

	if !strings.Contains(before, "2 unread") {
		t.Fatalf("stats before = %q, want 2 unread", before)
	}
	if strings.Contains(after, "unread") {
		t.Errorf("stats after marking all read = %q, want no unread", after)
	}
	if !strings.Contains(fleetAfter, "1 unread") {
		t.Errorf("cairn = %q after the OPERATOR marked their own mail read; "+
			"cairn's message must be untouched", fleetAfter)
	}
	if badgeAfter != "none" {
		t.Errorf("launcher badge = %q after clearing your inbox, want none", badgeAfter)
	}
}

// One message, not the ones below it.
//
// The server uses CommitItem rather than a cursor write; this proves the page
// gets that behaviour rather than a bulk consume dressed as a per-item control.
func TestDOMInboxMarkOneReadLeavesTheOthersWaiting(t *testing.T) {
	seedInbox(t,
		room.Event{Principal: "cairn", To: "operator", Topic: "one", Body: "first"},
		room.Event{Principal: "cairn", To: "operator", Topic: "two", Body: "second"},
		room.Event{Principal: "cairn", To: "operator", Topic: "three", Body: "third"},
	)
	base, ctx, errs := domEnv(t, Options{})

	var stats, remaining string
	if err := chromedp.Run(ctx,
		// Newest first, so the FIRST control on the page is the newest message —
		// the one whose acknowledgement a cursor write would use to swallow the
		// two below it.
		chromedp.Navigate(base+"/inbox/#name=operator&order=new"),
		chromedp.Sleep(2000*time.Millisecond),
		chromedp.Click("#ib-list .mark", chromedp.ByQuery),
		chromedp.Sleep(1800*time.Millisecond),
		chromedp.Evaluate(inboxText("#ib-stats"), &stats),
		chromedp.Evaluate(`String(document.querySelectorAll("#ib-list .mark").length)`, &remaining),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "inbox mark one", errs())

	if !strings.Contains(stats, "2 unread") {
		t.Errorf("stats = %q after marking the NEWEST of three read, want 2 unread — "+
			"a cursor write would have consumed the two below it", stats)
	}
	if remaining != "2" {
		t.Errorf("%s per-message controls remain, want 2", remaining)
	}
}

// EVERY TILE ON THE LAUNCHER SHOWS A DRAWN MARK, NOT A LETTER.
//
// appIcon falls back to the app's initial when it has no mark for the name.
// For a third-party app that is honest; for one of ours it is a bug that
// renders — the grid still looks fine, so nothing fails and nobody is told.
//
// It shipped that way: `relay` and `board` were renamed to `meet` and
// `sprint` without rekeying the SVG table, which is keyed by PANEL NAME, so
// Meet and Sprint dropped to "M" and "S" while every byte-level test stayed
// green. The chrome_test companion pins the table; this pins what the browser
// actually paints, which is the only place the fallback is visible.
func TestDOMEveryTileShowsAMarkNotALetter(t *testing.T) {
	base, ctx, errs := domEnv(t, Options{})

	// label -> "svg" | the text it fell back to. Read off the rendered tiles,
	// so a tile that never rendered is a missing key, not a silent pass.
	probe := `(()=>{const o={};
		for (const b of document.querySelectorAll(".tile")) {
			const l = b.querySelector(".label"), i = b.querySelector(".icon");
			if (!l || !i) continue;
			o[l.textContent] = i.querySelector("svg") ? "svg" : i.textContent.trim();
		}
		return JSON.stringify(o);})()`

	var raw string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/"),
		chromedp.Sleep(1500*time.Millisecond),
		chromedp.Evaluate(probe, &raw),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "launcher marks", errs())

	got := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode tile marks: %v (%s)", err, raw)
	}
	for _, p := range Discover() {
		mark, ok := got[p.Label]
		if !ok {
			t.Errorf("tile %q never rendered; the launcher shows %v", p.Label, got)
			continue
		}
		if mark != "svg" {
			t.Errorf("tile %q renders the placeholder %q instead of its mark", p.Label, mark)
		}
	}
}

// The Runbooks section, in a real browser.
//
// A runbook is a kb page of type runbook: the reusable PROCEDURE a story cites
// while the story carries this iteration's values. The Sprint page has ONE
// section for them and ONE pane a runbook's body renders into — both the
// section's own chips and a story's "runbooks" chip land there.
// longSlug is wider than the chip's fixed slug width (app.css: 20ch), so the
// section test can see the ellipsis engage.
const longSlug = "rotate-the-signing-key-and-republish-every-platform-artifact"

func stubRunbooks(t *testing.T) {
	t.Helper()
	repo := kb.Open(filepath.Join(t.TempDir(), kb.RepoSub))
	write := func(slug, typ, title, body string) {
		t.Helper()
		if err := repo.Write(&kb.Page{
			Slug: slug, Form: kb.FormPage, Type: typ, Title: title,
			Description: "WHEN the DOM test needs a runbook", Status: kb.StatusCandidate, Body: body,
		}, "add"); err != nil {
			t.Fatal(err)
		}
	}
	write("cut-release", kb.TypeRunbook, "cut a release", "## Steps\n\n1. tag it\n2. promote it\n")
	write("some-lesson", kb.TypeLesson, "some lesson", "not a runbook")
	write(longSlug, kb.TypeRunbook, "a long one", "## Long\n\nthe slug is a sentence\n")
	orig := runbookRingsFn
	t.Cleanup(func() { runbookRingsFn = orig })
	runbookRingsFn = func([]string) []runbookRing { return []runbookRing{{Name: "repo x", Store: repo}} }
}

func TestDOMRunbooksSectionOpensAPageBody(t *testing.T) {
	stubBoard(t)
	stubRunbooks(t)
	base, ctx, errs := domEnv(t, Options{})

	var chips, tips, clipped, pane, hash string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.Sleep(2*time.Second),
		chromedp.Evaluate(`Array.from(document.querySelectorAll("#bd-runbooks .ref.link")).map(b => b.textContent).join(",")`, &chips),
		chromedp.Evaluate(`Array.from(document.querySelectorAll("#bd-runbooks .ref.link")).map(b => b.title).join(",")`, &tips),
		chromedp.Evaluate(`(() => {
			const s = Array.from(document.querySelectorAll("#bd-runbooks .ref.link .slug"));
			return s.map(x => (x.scrollWidth > x.clientWidth ? "clipped" : "fits") + ":" + getComputedStyle(x).textOverflow).join(",");
		})()`, &clipped),
		chromedp.Click(`#bd-runbooks .ref.link`, chromedp.ByQuery),
		chromedp.Sleep(1500*time.Millisecond),
		chromedp.Evaluate(`(() => {
			const d = document.querySelector("#bd-runbooks .story-detail");
			if (!d) return "NO PANE";
			const h = d.querySelector(".story-body .story-h");
			return (d.hidden ? "HIDDEN:" : "SHOWN:") + (h ? h.textContent.trim() : "NO HEADING");
		})()`, &pane),
		chromedp.Evaluate(`location.hash`, &hash),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "runbooks section", errs())

	// Two chips: the seeded runbook and the one with a slug longer than the
	// chip's fixed width (the lesson is excluded). Each names its page by seq
	// AND slug — the seq is the handle a human types (kb:1), the slug what a
	// story body cites — and the tooltip carries the complete kb:<slug>, so a
	// clipped slug can be read by hovering.
	if chips != "#1 cut-release,#3 "+longSlug {
		t.Fatalf("the section lists runbooks only, as `#seq slug`: %q", chips)
	}
	if !strings.HasPrefix(tips, "kb:cut-release (kb:1) — cut a release") || !strings.Contains(tips, "kb:"+longSlug+" (kb:3)") {
		t.Errorf("each chip's title must carry the complete kb:<slug> and the seq handle: %q", tips)
	}
	if clipped != "fits:ellipsis,clipped:ellipsis" {
		t.Errorf("a short slug fits and a long one is clipped with an ellipsis (both at the fixed width): %q", clipped)
	}
	if pane != "SHOWN:Steps" {
		t.Errorf("clicking a runbook chip should show its body through the markdown renderer: %s", pane)
	}
	if !strings.Contains(hash, "runbook=cut-release") {
		t.Errorf("the open runbook is not deep-linked in the hash: %s", hash)
	}
}

func TestDOMStoryRunbookChipOpensTheSharedPane(t *testing.T) {
	stubBoard(t)
	stubRunbooks(t)
	origStory := storyDetailFn
	t.Cleanup(func() { storyDetailFn = origStory })
	storyDetailFn = func(board.Todo) (*board.Story, error) {
		return &board.Story{
			ID: "aaaa1111", Seq: 1, Title: "still open", Status: "todo",
			Body: "Runbook: [[kb:cut-release]] — also see [[kb:some-lesson]].",
			Outbound: []board.LinkRef{
				{Ref: "kb:cut-release", Seq: 1, Title: "cut a release", Type: "runbook", Status: "resolved"},
				{Ref: "kb:some-lesson", Title: "some lesson", Type: "lesson", Status: "resolved"},
				{Ref: "kb:typo-slug", Status: "dangling"},
			},
		}, nil
	}
	base, ctx, errs := domEnv(t, Options{})

	var chips, needs, pane string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.Sleep(2*time.Second),
		chromedp.Click(`.bd-sprint .refs .ref.link`, chromedp.ByQuery), // the first story chip
		chromedp.Sleep(1500*time.Millisecond),
		chromedp.Evaluate(`Array.from(document.querySelectorAll(".bd-sprint .story-detail .refs .ref.link")).map(b => b.textContent).join(",")`, &chips),
		chromedp.Evaluate(`Array.from(document.querySelectorAll(".bd-sprint .story-detail .refs .ref.needs")).map(b => b.textContent).join(",")`, &needs),
		chromedp.Click(`.bd-sprint .story-detail .refs .ref.link`, chromedp.ByQuery),
		chromedp.Sleep(1500*time.Millisecond),
		chromedp.Evaluate(`(() => {
			const d = document.querySelector("#bd-runbooks .story-detail");
			if (!d) return "NO PANE";
			return (d.hidden ? "HIDDEN:" : "SHOWN:") + d.textContent.trim().slice(0, 40);
		})()`, &pane),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "story runbook chip", errs())

	if chips != "#1 cut-release" {
		t.Errorf("a story's runbook row shows runbook-typed kb refs only, not the lesson, as `#seq slug`: %q", chips)
	}
	if needs != "typo-slug" {
		t.Errorf("a dangling kb ref must stay visible as a warning chip: %q", needs)
	}
	if !strings.HasPrefix(pane, "SHOWN:kb:cut-release") {
		t.Errorf("the story's chip must open the SAME pane the section uses: %s", pane)
	}
}

func TestDOMRunbookPaneSurvivesARefresh(t *testing.T) {
	stubBoard(t)
	stubRunbooks(t)
	base, ctx, errs := domEnv(t, Options{})

	const readPane = `(() => {
		const d = document.querySelector("#bd-runbooks .story-detail");
		return d ? String(!d.hidden) + ":" + d.textContent.trim().slice(0, 15) : "NONE";
	})()`
	var before, after string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.Sleep(2*time.Second),
		chromedp.Click(`#bd-runbooks .ref.link`, chromedp.ByQuery),
		chromedp.Sleep(1500*time.Millisecond),
		chromedp.Evaluate(readPane, &before),
		chromedp.Evaluate(`load()`, nil, awaitPromise),
		chromedp.Sleep(1500*time.Millisecond),
		chromedp.Evaluate(readPane, &after),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "runbook refresh", errs())

	if !strings.HasPrefix(before, "true:kb:cut-release") {
		t.Fatalf("the runbook pane did not open: %s", before)
	}
	if before != after {
		t.Errorf("a refresh closed the runbook the reader had open.\n before = %s\n after  = %s", before, after)
	}
}
