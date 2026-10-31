// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build verifydom

package webconsole

import (
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

// TestDOMNewAppFromLauncher: the launcher's "New app" card names a blank app
// and opens its empty page, the way File > New opens a new document.
func TestDOMNewAppFromLauncher(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_APPS_DIR", t.TempDir())
	base, ctx, errs := domEnv(t, Options{})

	var path, blank string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/"),
		chromedp.WaitVisible("#new-app-open"),
		chromedp.Click("#new-app-open"),
		chromedp.WaitVisible("#new-app-label"),
		chromedp.SendKeys("#new-app-label", "Sign-up sheet"),
		chromedp.Click("#new-app-create"),
		chromedp.WaitVisible("main[data-bashy-blank]"),
		chromedp.Evaluate(`location.pathname`, &path),
		chromedp.Text("main[data-bashy-blank] h1", &blank),
		chromedp.Sleep(200*time.Millisecond),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "new app", errs())
	if path != "/sign-up-sheet/" || blank != "Sign-up sheet" {
		t.Fatalf("new app opened %q with heading %q", path, blank)
	}
}

// TestDOMNewAppInputReadableInDarkTheme: the New app name box (and the
// cloud Pair box, which shares its style) shows typed text in both themes:
// its text color is the theme foreground, never the page's inherited color
// on a fixed light fill.
func TestDOMNewAppInputReadableInDarkTheme(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_APPS_DIR", t.TempDir())
	base, ctx, errs := domEnv(t, Options{})

	var same bool
	if err := chromedp.Run(ctx,
		emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: "dark"}}),
		chromedp.Navigate(base+"/"),
		chromedp.WaitVisible("#new-app-open"),
		chromedp.Click("#new-app-open"),
		chromedp.WaitVisible("#new-app-label"),
		chromedp.SendKeys("#new-app-label", "Sign-up sheet"),
		chromedp.Evaluate(`(() => {
			const rgb = (c) => (c.match(/[0-9.]+/g) || []).slice(0, 4).map(Number);
			const lum = ([r, g, b]) => { const f = (v) => { v /= 255; return v <= .03928 ? v / 12.92 : ((v + .055) / 1.055) ** 2.4; }; return .2126 * f(r) + .7152 * f(g) + .0722 * f(b); };
			const el = document.getElementById("new-app-label"), s = getComputedStyle(el);
			const fg = rgb(s.color);
			// Composite the element's fill over every translucent ancestor
			// fill down to the first opaque one: what the eye actually sees.
			const layers = [];
			for (let n = el; n; n = n.parentElement) {
				const c = rgb(getComputedStyle(n).backgroundColor);
				const a = c.length === 4 ? c[3] : 1;
				if (a > 0) layers.push([c[0], c[1], c[2], a]);
				if (a >= 1) break;
			}
			let bg = [255, 255, 255];
			for (const [r, g, b, a] of layers.reverse()) bg = [r, g, b].map((v, i) => v * a + bg[i] * (1 - a));
			const [a, b] = [lum(fg), lum(bg)].sort((x, y) => y - x);
			return (a + .05) / (b + .05) < 3;
		})()`, &same),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "new app dark", errs())
	if same {
		t.Fatal("New app input text is invisible in the dark theme (light text on a light fill)")
	}
}
