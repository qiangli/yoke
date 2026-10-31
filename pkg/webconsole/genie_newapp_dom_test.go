// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build verifydom

package webconsole

import (
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestDOMGenieNewApp (S526.B1i): a new app is made from the ONE Genie form
// — no target choice, no name box. The person writes the wish on the
// launcher; Genie (the fake intentShaper) answers NEW-APP with no change;
// the server makes the blank app, opens a draft on it and runs the wish
// there as the first step; the panel says "Genie is making Sign-up sheet…",
// and the browser lands in /shape/signup/draft/ with the result in place,
// to Save or Cancel like a document. The launcher lists the new tile.
func TestDOMGenieNewApp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_APPS_DIR", t.TempDir())
	base, ctx, errs := domEnv(t, Options{Shape: ShapeOptions{Home: home, WorkRoot: t.TempDir(),
		Delegate: &intentShaper{}, Verifier: &fakeVerifier{}, Activator: &fakeActivator{home: home}}})

	until := func(step, expr string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			var ok bool
			if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &ok)); err != nil {
				t.Fatalf("%s: %v", step, err)
			}
			if ok {
				return
			}
			if time.Now().After(deadline) {
				var st string
				_ = chromedp.Run(ctx, chromedp.Evaluate(`((document.querySelector('.bgn-status') || {}).textContent || '') + ' @ ' + location.href`, &st))
				t.Fatalf("%s: never true: %s (status %q; js errors %q)", step, expr, st, errs())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	run := func(step string, a ...chromedp.Action) {
		t.Helper()
		if err := chromedp.Run(ctx, a...); err != nil {
			t.Fatalf("%s: %v (js errors %q)", step, err, errs())
		}
	}
	run("open launcher", chromedp.Navigate(base+"/"))
	until("genie button", `!!document.querySelector('[data-bashy-genie-btn="live"]')`)
	run("open panel", chromedp.Click(`[data-bashy-genie-btn="live"]`, chromedp.ByQuery))
	until("one form", `(() => { const p = document.querySelector('[data-bashy-genie-panel]'); return !!p && !p.hidden && !!p.querySelector('textarea'); })()`)
	var toggles int
	run("no target choice", chromedp.Evaluate(`document.querySelectorAll('[data-bgn-target], [data-bgn-newapp-name], .bgn-target').length`, &toggles))
	if toggles != 0 {
		t.Fatalf("the wish panel still offers a target choice or a name box (%d elements)", toggles)
	}
	run("wish", chromedp.SendKeys(`[data-bashy-genie-panel] textarea`, "make a sign-up sheet app for Saturday's class, name and email, waitlist after 20", chromedp.ByQuery, chromedp.NodeReady))
	run("make the wish", chromedp.Click(`[data-bashy-genie-panel] .bgn-b.bgn-p`, chromedp.ByQuery, chromedp.NodeReady))
	until("the panel follows Genie into the new app", `location.pathname.includes('/shape/signup/draft/') || ((document.querySelector('[data-bashy-genie-panel] .bgn-status') || {}).textContent || '').includes('Genie is making Sign-up sheet')`)
	until("lands in the new app's draft", `location.pathname.includes('/shape/signup/draft/') && !!document.querySelector('#signup') && !!document.querySelector('[data-bgn-draft]')`)
	var bar string
	run("read bar", chromedp.Evaluate(`document.querySelector('[data-bgn-draft]').textContent`, &bar))
	if !strings.Contains(bar, "1 wish") {
		t.Fatalf("draft bar = %q, want one wish applied", bar)
	}
	assertNoJSErrors(t, "genie new app", errs())

	// The launcher shows the new tile at once: registered apps are read live.
	run("back to the launcher", chromedp.Navigate(base+"/"))
	until("new tile", `[...document.querySelectorAll('.tile .label')].some((e) => e.textContent.trim() === 'Sign-up sheet')`)
	assertNoJSErrors(t, "launcher after new app", errs())
}
