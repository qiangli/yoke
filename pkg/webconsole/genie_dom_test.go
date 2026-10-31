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

// TestDOMGenieButton drives the common Genie button in a real browser: it
// renders in the launcher's header bar and in a shaped app's corner, the
// panel opens on click and on Alt+Shift+G, a wish reaches the shaping API
// and starts a candidate, pointing at an element records it, and the
// protected Shape page says so. No script may throw on any of these pages.
func TestDOMGenieButton(t *testing.T) {
	source := newNotesSource(t)
	notes := Panel{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: source,
		Auth: AuthSystem, Source: "registered", Available: true}
	base, ctx, errors := domEnv(t, Options{Panels: []Panel{notes},
		Shape: ShapeOptions{Home: t.TempDir(), WorkRoot: t.TempDir(), Delegate: &fakeShaper{}}})

	until := func(expr string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			var ok bool
			if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &ok)); err != nil {
				t.Fatal(err)
			}
			if ok {
				return
			}
			if time.Now().After(deadline) {
				var status string
				_ = chromedp.Run(ctx, chromedp.Evaluate(`(document.querySelector('[data-bashy-genie-panel] .bgn-status') || {}).textContent || ''`, &status))
				t.Fatalf("never true: %s (status %q)", expr, status)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	const panel = `document.querySelector('[data-bashy-genie-panel]')`

	// The launcher: anchored in the header bar, live mode.
	var inBar bool
	var title string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/"),
		chromedp.WaitVisible(`[data-bashy-genie-btn]`),
		chromedp.Evaluate(`!!document.querySelector('#bar [data-bashy-genie-btn="live"]')`, &inBar),
		chromedp.Click(`[data-bashy-genie-btn]`),
		chromedp.WaitVisible(`[data-bashy-genie-panel] textarea`),
		chromedp.Text(`#bgn-title`, &title),
		chromedp.SendKeys(`[data-bashy-genie-panel] textarea`, "a bigger search box"),
		chromedp.Click(`[data-bashy-genie-panel] .bgn-p`),
	); err != nil {
		t.Fatal(err)
	}
	if !inBar || !strings.Contains(title, "console") {
		t.Errorf("launcher button in bar=%v title=%q", inBar, title)
	}
	// The wish reached the shaping API (the loopback owner is a principal)
	// and a candidate was started; the panel says so and links to the
	// Shape page.
	until(panel + `.querySelector('.bgn-status').textContent.includes('Genie is on it')`)
	until(panel + `.querySelector('.bgn-status a').getAttribute('href').endsWith('/shape/console/')`)
	if err := chromedp.Run(ctx, chromedp.Click(`[data-bashy-genie-panel] .bgn-row .bgn-b:nth-last-child(2)`)); err != nil {
		t.Fatal(err)
	}
	until(panel + `.hidden === true`)
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.dispatchEvent(new KeyboardEvent('keydown', {altKey: true, shiftKey: true, code: 'KeyG', bubbles: true})); true`, nil)); err != nil {
		t.Fatal(err)
	}
	until(panel + `.hidden === false`)

	// A shaped app's page: the corner, and pointing at an element.
	var corner bool
	var chip string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/notes/"),
		chromedp.WaitVisible(`[data-bashy-genie-btn]`),
		chromedp.Evaluate(`(() => { const w = document.querySelector('[data-bashy-genie-btn]').parentElement; return w.classList.contains('bgn-corner') && getComputedStyle(w).position === 'fixed'; })()`, &corner),
		chromedp.Click(`[data-bashy-genie-btn]`),
		chromedp.WaitVisible(`[data-bashy-genie-panel] textarea`),
		chromedp.Click(`[data-bashy-genie-panel] .bgn-row .bgn-b:first-child`),
		chromedp.Click(`#add button`),
		chromedp.WaitVisible(`[data-bashy-genie-panel] .bgn-chip`),
		chromedp.Text(`[data-bashy-genie-panel] .bgn-chip`, &chip),
	); err != nil {
		t.Fatal(err)
	}
	if !corner || !strings.Contains(chip, `form#add > button "Add"`) {
		t.Errorf("notes corner=%v chip=%q", corner, chip)
	}

	// The Shape page is protected: the panel says so and files a request.
	var note, send string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/shape/console/"),
		chromedp.WaitVisible(`[data-bashy-genie-btn]`),
		chromedp.Click(`[data-bashy-genie-btn]`),
		chromedp.WaitVisible(`[data-bashy-genie-panel] .bgn-note`),
		chromedp.Text(`[data-bashy-genie-panel] .bgn-note`, &note),
		chromedp.Text(`[data-bashy-genie-panel] .bgn-p`, &send),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "protected") || send != "File request" {
		t.Errorf("protected page note=%q send=%q", note, send)
	}
	assertNoJSErrors(t, "genie button", errors())
}
