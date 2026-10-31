// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build verifydom

package webconsole

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// domSuggester is the shaping agent for the example-wish DOM test: it
// answers the read-only example-wish brief for the notes app, fails it for
// every other app (the launcher), and leaves real wishes to fakeShaper.
type domSuggester struct {
	fakeShaper
	mu   sync.Mutex
	apps []string
}

func (d *domSuggester) Delegate(ctx context.Context, req DelegateRequest) (DelegateResult, error) {
	if !strings.Contains(req.Task, "<<<PAGE") {
		return d.fakeShaper.Delegate(ctx, req)
	}
	d.mu.Lock()
	d.apps = append(d.apps, req.Task)
	d.mu.Unlock()
	if strings.Contains(req.Task, `web app "Notes" (notes)`) {
		return DelegateResult{Agent: "codex:fake", Output: "make the ship faster\nadd a delete button\n"}, nil
	}
	return DelegateResult{}, errors.New("no examples today")
}

func (d *domSuggester) asked(app string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, t := range d.apps {
		if strings.Contains(t, "("+app+")") || strings.Contains(t, `"`+app+`"`) {
			return true
		}
	}
	return false
}

// TestDOMGenieExamples drives the wish form's prompt and placeholder
// (S526.B1g): the panel shows the one fixed generic prompt; the placeholder
// becomes the first generated example once the server answers, and
// nothing is prefilled; with generation failing it reads the neutral
// "Describe what you'd like"; no hardcoded "newest item" text remains
// anywhere in the panel. No script may throw.
func TestDOMGenieExamples(t *testing.T) {
	source := newNotesSource(t)
	genie := &domSuggester{}
	notes := Panel{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: source,
		Auth: AuthSystem, Source: "registered", Available: true}
	base, ctx, errors := domEnv(t, Options{Panels: []Panel{notes},
		Shape: ShapeOptions{Home: t.TempDir(), WorkRoot: t.TempDir(), Delegate: genie, SuggestTimeout: 5 * time.Second}})

	until := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("never true: %s", what)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	eval := func(expr string, into any) {
		t.Helper()
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, into)); err != nil {
			t.Fatal(err)
		}
	}
	const panel = `document.querySelector('[data-bashy-genie-panel]')`
	const placeholder = panel + `.querySelector('textarea').placeholder`

	// The notes app: the fixed prompt, then the generated example.
	var prompt, ph, text, value string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/notes/"),
		chromedp.WaitVisible(`[data-bashy-genie-btn]`),
		chromedp.Click(`[data-bashy-genie-btn]`),
		chromedp.WaitVisible(`[data-bashy-genie-panel] textarea`),
		chromedp.Text(`[data-bgn-prompt]`, &prompt),
	); err != nil {
		t.Fatal(err)
	}
	if prompt != "What would you like to change, add or make?" {
		t.Errorf("prompt = %q", prompt)
	}
	until("placeholder is the first generated example", func() bool {
		eval(placeholder, &ph)
		return ph == "make the ship faster"
	})
	eval(panel+`.querySelector('textarea').value`, &value)
	eval(panel+`.textContent + ' ' + `+placeholder, &text)
	if value != "" {
		t.Errorf("textarea prefilled with %q", value)
	}
	for _, old := range []string{"newest item", "whole app", "e.g."} {
		if strings.Contains(text, old) {
			t.Errorf("panel still carries %q:\n%s", old, text)
		}
	}

	// The launcher: generation fails, the placeholder stays neutral.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/"),
		chromedp.WaitVisible(`[data-bashy-genie-btn]`),
		chromedp.Click(`[data-bashy-genie-btn]`),
		chromedp.WaitVisible(`[data-bashy-genie-panel] textarea`),
		chromedp.Text(`[data-bgn-prompt]`, &prompt),
	); err != nil {
		t.Fatal(err)
	}
	if prompt != "What would you like to change, add or make?" {
		t.Errorf("launcher prompt = %q", prompt)
	}
	until("the launcher's examples were asked for", func() bool { return genie.asked("console") })
	time.Sleep(300 * time.Millisecond) // the failed answer reaches the page
	eval(placeholder, &ph)
	if ph != "Describe what you'd like" {
		t.Errorf("launcher placeholder = %q, want the neutral one", ph)
	}
	assertNoJSErrors(t, "genie examples", errors())
}
