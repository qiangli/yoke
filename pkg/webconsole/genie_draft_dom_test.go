// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build verifydom

package webconsole

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestDOMGenieEditSession drives the edit session (S526.B1h) in a real
// browser. From the wish panel on the live notes app, Start editing opens
// the draft: the bar reads "Editing draft: 0 wishes" with undo and redo
// off. A wish made from the bar's sparkle becomes a step and the page
// shows it in place, outlined; undo from the bar steps back (the live
// heading returns, redo comes on), Ctrl-Shift-Z redoes, Ctrl-Z undoes. A
// refused Save names the failing part and keeps the draft; a second Save
// lands on the live app, one version newer, with no bar left. A second
// session is ended with Cancel (armed on the first click) and the live app
// is unchanged. The live page meanwhile says a draft is open and leads back
// in. No script throws anywhere.
func TestDOMGenieEditSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source := newNotesSource(t)
	notes := Panel{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: source,
		Auth: AuthSystem, Source: "registered", Available: true}
	verifier := &switchVerifier{pass: false, detail: "smoke check failed: the heading must keep its text"}
	base, ctx, errs := domEnv(t, Options{Panels: []Panel{notes},
		Shape: ShapeOptions{Home: home, WorkRoot: t.TempDir(), Delegate: &fakeShaper{},
			Verifier: verifier, Activator: &fakeActivator{home: home}}})
	client := &http.Client{Timeout: 10 * time.Second}
	draftState := func() (int, draftView) {
		t.Helper()
		resp, err := client.Get(base + "/api/shape/notes/draft")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var v draftView
		_ = json.NewDecoder(resp.Body).Decode(&v)
		return resp.StatusCode, v
	}

	until := func(expr string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			var ok bool
			if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &ok)); err != nil {
				t.Fatal(err)
			}
			if ok {
				return
			}
			if time.Now().After(deadline) {
				var bar string
				_ = chromedp.Run(ctx, chromedp.Evaluate(`((document.querySelector('.bgn-bar') || {}).textContent || '') + ' @ ' + location.href + ' draft=' + localStorage.getItem('bashy.genie.draft')`, &bar))
				t.Fatalf("never true: %s (bar %q; js errors %q)", expr, bar, errs())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	text := func(expr string) string {
		t.Helper()
		var s string
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &s)); err != nil {
			t.Fatal(err)
		}
		return s
	}
	key := func(k string, shift bool) {
		t.Helper()
		sh := "false"
		if shift {
			sh = "true"
		}
		if err := chromedp.Run(ctx, chromedp.Evaluate(`document.body.dispatchEvent(new KeyboardEvent('keydown', {key: '`+k+`', code: 'KeyZ', ctrlKey: true, shiftKey: `+sh+`, bubbles: true, cancelable: true})); true`, nil)); err != nil {
			t.Fatal(err)
		}
	}
	live := base + "/notes/"
	draft := base + "/shape/notes/draft/"
	const bar = `document.querySelector('[data-bgn-draft]')`

	// Start editing from the wish panel on the live app.
	if err := chromedp.Run(ctx, chromedp.Navigate(live), chromedp.WaitVisible(`[data-bashy-genie-btn="live"]`),
		chromedp.Click(`[data-bashy-genie-btn="live"]`), chromedp.WaitVisible(`[data-bgn-edit] .bgn-b`)); err != nil {
		t.Fatal(err)
	}
	if got := text(`document.querySelector('[data-bgn-edit] .bgn-b').textContent`); got != "Start editing" {
		t.Errorf("edit button = %q", got)
	}
	if err := chromedp.Run(ctx, chromedp.Click(`[data-bgn-edit] .bgn-b`), chromedp.WaitVisible(`[data-bgn-draft]`)); err != nil {
		t.Fatal(err)
	}
	until(`location.href === ` + jsonString(draft))
	until(bar + `.querySelector('.bgn-count').textContent === 'Editing draft: 0 wishes'`)
	var undoOff, redoOff, saveOff, inBar, fixed bool
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(bar+`.querySelector('.bgn-b[aria-label^="Undo"]').disabled`, &undoOff),
		chromedp.Evaluate(bar+`.querySelector('.bgn-b[aria-label^="Redo"]').disabled`, &redoOff),
		chromedp.Evaluate(bar+`.querySelector('.bgn-b[aria-label^="Save"]').disabled`, &saveOff),
		chromedp.Evaluate(`!!document.querySelector('[data-bgn-draft] [data-bashy-genie-btn="draft"]:not([aria-disabled])')`, &inBar),
		chromedp.Evaluate(`getComputedStyle(`+bar+`).position === 'fixed'`, &fixed),
	); err != nil {
		t.Fatal(err)
	}
	if !undoOff || !redoOff || !saveOff || !inBar || !fixed {
		t.Errorf("fresh bar: undo off=%v redo off=%v save off=%v sparkle in bar=%v fixed=%v", undoOff, redoOff, saveOff, inBar, fixed)
	}
	for _, want := range []string{"Cancel", "Save"} {
		if !strings.Contains(text(bar+`.textContent`), want) {
			t.Errorf("bar lacks %q: %q", want, text(bar+`.textContent`))
		}
	}
	if got := text(`document.querySelector('h1').textContent`); got != "Notes" {
		t.Errorf("the fresh draft is not the active version: h1 %q", got)
	}

	// A wish from the bar's sparkle: a step, shown in place and outlined.
	if err := chromedp.Run(ctx, chromedp.Click(`[data-bgn-draft] [data-bashy-genie-btn="draft"]`), chromedp.WaitVisible(`[data-bashy-genie-panel] textarea`),
		chromedp.SendKeys(`[data-bashy-genie-panel] textarea`, "show the newest note first"),
		chromedp.Click(`[data-bashy-genie-panel] .bgn-b.bgn-p`)); err != nil {
		t.Fatal(err)
	}
	until(bar + `.querySelector('.bgn-count').textContent === 'Editing draft: 1 wish' && document.querySelector('h1') && document.querySelector('h1').textContent.includes('newest first')`)
	until(`!!document.querySelector('h1.bgn-changed')`)
	until(bar + `.querySelector('.bgn-sum').textContent.includes('differs from your current version')`)
	if got := text(bar + `.querySelector('.bgn-wish').textContent`); got != "show the newest note first" {
		t.Errorf("bar wish = %q", got)
	}
	if code, v := draftState(); code != http.StatusOK || v.Wishes != 1 {
		t.Fatalf("draft after a wish = %d %+v", code, v)
	}

	// Undo from the bar: the heading is the live one again, redo is on.
	if err := chromedp.Run(ctx, chromedp.Click(`[data-bgn-draft] .bgn-b[aria-label^="Undo"]`)); err != nil {
		t.Fatal(err)
	}
	until(bar + `.querySelector('.bgn-count').textContent === 'Editing draft: 0 wishes' && document.querySelector('h1') && document.querySelector('h1').textContent === 'Notes'`)
	until(`!` + bar + `.querySelector('.bgn-b[aria-label^="Redo"]').disabled`)
	if !strings.Contains(text(bar+`.querySelector('.bgn-sum').textContent`), "Undone: 1 wish") {
		t.Errorf("undone note missing: %q", text(bar+`.querySelector('.bgn-sum').textContent`))
	}
	// Ctrl-Shift-Z redoes, Ctrl-Z undoes.
	key("Z", true)
	until(bar + `.querySelector('.bgn-count').textContent === 'Editing draft: 1 wish' && document.querySelector('h1').textContent.includes('newest first')`)
	key("z", false)
	until(bar + `.querySelector('.bgn-count').textContent === 'Editing draft: 0 wishes'`)
	key("Z", true)
	until(bar + `.querySelector('.bgn-count').textContent === 'Editing draft: 1 wish'`)

	// The live page says a draft is open and leads back in.
	if err := chromedp.Run(ctx, chromedp.Navigate(live), chromedp.WaitVisible(`[data-bgn-draft-note]`)); err != nil {
		t.Fatal(err)
	}
	if got := text(`document.querySelector('[data-bgn-draft-note]').textContent`); !strings.Contains(got, "This is your current version") || !strings.Contains(got, "1 wish") {
		t.Errorf("draft note: %q", got)
	}
	if err := chromedp.Run(ctx, chromedp.Click(`[data-bgn-draft-note] .bgn-p`), chromedp.WaitVisible(`[data-bgn-draft]`)); err != nil {
		t.Fatal(err)
	}
	until(`location.href === ` + jsonString(draft) + ` && ` + bar + `.querySelector('.bgn-count').textContent === 'Editing draft: 1 wish'`)

	// Save: the trusted verifier refuses; the bar says why, the draft stays.
	if err := chromedp.Run(ctx, chromedp.Click(`[data-bgn-draft] .bgn-b[aria-label^="Save"]`)); err != nil {
		t.Fatal(err)
	}
	until(bar + `.querySelector('.bgn-alert').textContent.includes('Not saved: smoke check failed: the heading must keep its text')`)
	until(`location.href === ` + jsonString(draft))
	if code, v := draftState(); code != http.StatusOK || v.Wishes != 1 {
		t.Fatalf("draft after a refused save = %d %+v", code, v)
	}
	// Save again: verified, activated, the live app shows the change, no
	// bar is left, the draft is gone.
	verifier.mu.Lock()
	verifier.pass = true
	verifier.mu.Unlock()
	if err := chromedp.Run(ctx, chromedp.Click(`[data-bgn-draft] .bgn-b[aria-label^="Save"]`)); err != nil {
		t.Fatal(err)
	}
	until(`location.href === ` + jsonString(live) + ` && document.querySelector('h1') && document.querySelector('h1').textContent.includes('newest first')`)
	until(`!!document.querySelector('[data-bashy-genie-btn="live"]')`)
	if text(`document.querySelector('[data-bgn-draft], [data-bgn-draft-note]') ? 'bar' : ''`) != "" {
		t.Error("an editing bar is left on the live page after Save")
	}
	if code, _ := draftState(); code != http.StatusNotFound {
		t.Errorf("draft after Save = %d, want 404", code)
	}
	if verifier.count() != 2 {
		t.Errorf("verifier ran %d times, want 2 (once per Save)", verifier.count())
	}

	// A second session ends with Cancel: armed on the first click, done on
	// the second; the live app is unchanged.
	liveBefore := text(`document.documentElement.outerHTML`)
	if err := chromedp.Run(ctx, chromedp.Click(`[data-bashy-genie-btn="live"]`), chromedp.WaitVisible(`[data-bgn-edit] .bgn-b`),
		chromedp.Click(`[data-bgn-edit] .bgn-b`), chromedp.WaitVisible(`[data-bgn-draft]`)); err != nil {
		t.Fatal(err)
	}
	until(`location.href === ` + jsonString(draft))
	if err := chromedp.Run(ctx, chromedp.Click(`[data-bgn-draft] [data-bashy-genie-btn="draft"]`), chromedp.WaitVisible(`[data-bashy-genie-panel] textarea`),
		chromedp.SendKeys(`[data-bashy-genie-panel] textarea`, "add an export button"),
		chromedp.Click(`[data-bashy-genie-panel] .bgn-b.bgn-p`)); err != nil {
		t.Fatal(err)
	}
	until(bar + `.querySelector('.bgn-count').textContent === 'Editing draft: 1 wish'`)
	if err := chromedp.Run(ctx, chromedp.Click(`[data-bgn-draft] .bgn-danger`)); err != nil {
		t.Fatal(err)
	}
	until(bar + `.querySelector('.bgn-danger').textContent.includes('discard')`)
	if err := chromedp.Run(ctx, chromedp.Click(`[data-bgn-draft] .bgn-danger`)); err != nil {
		t.Fatal(err)
	}
	until(`location.href === ` + jsonString(live) + ` && !!document.querySelector('[data-bashy-genie-btn="live"]')`)
	if got := text(`document.documentElement.outerHTML`); got != liveBefore {
		t.Error("the live page changed after Cancel")
	}
	if code, _ := draftState(); code != http.StatusNotFound {
		t.Errorf("draft after Cancel = %d, want 404", code)
	}
	assertNoJSErrors(t, "edit session", errs())
}
