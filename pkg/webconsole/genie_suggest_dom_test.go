// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build verifydom

package webconsole

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// domNextShaper is fakeShaper whose answer lists follow-up wishes (NEXT:
// lines), and counts its calls so the test can prove a panel open makes
// none.
type domNextShaper struct {
	fakeShaper
	nmu  sync.Mutex
	next string
}

func (d *domNextShaper) Delegate(ctx context.Context, req DelegateRequest) (DelegateResult, error) {
	res, err := d.fakeShaper.Delegate(ctx, req)
	if err != nil {
		return res, err
	}
	d.nmu.Lock()
	res.Output += d.next
	d.nmu.Unlock()
	return res, nil
}

func (d *domNextShaper) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.tasks)
}

// TestDOMGenieExamples drives the wish form's prompt and placeholder
// (S526.B1g, revision): the panel shows the one fixed generic prompt; on
// the base, with nothing stored, the placeholder reads the neutral
// "Describe what you'd like" and Genie is not called; after a wish is
// applied and saved, the placeholder becomes the first follow-up Genie
// listed with that wish (NEXT:), still without a call on open; a version
// without NEXT shows a follow-up derived from its wish. Nothing is
// prefilled; no hardcoded "newest item" text remains; no script may throw.
func TestDOMGenieExamples(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source := newNotesSource(t)
	genie := &domNextShaper{next: "NEXT:\n- make the ship faster\n- add a delete button\n"}
	notes := Panel{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: source,
		Auth: AuthSystem, Source: "registered", Available: true}
	base, ctx, errors := domEnv(t, Options{Panels: []Panel{notes},
		Shape: ShapeOptions{Home: home, WorkRoot: t.TempDir(), Delegate: genie,
			Verifier: &switchVerifier{pass: true}, Activator: &fakeActivator{home: home}}})
	client := &http.Client{Timeout: 10 * time.Second}
	post := func(path string, want int) {
		t.Helper()
		resp, err := client.Post(base+path, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("POST %s = %d, want %d", path, resp.StatusCode, want)
		}
	}
	wish := func(text string) {
		t.Helper()
		resp, err := client.Post(base+"/api/shape/notes/draft/wishes", "application/json",
			strings.NewReader(`{"request":"`+text+`","page":"/","element":""}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("wish = %d", resp.StatusCode)
		}
		deadline := time.Now().Add(20 * time.Second)
		for {
			r2, err := client.Get(base + "/api/shape/notes/draft")
			if err != nil {
				t.Fatal(err)
			}
			var v draftView
			_ = json.NewDecoder(r2.Body).Decode(&v)
			r2.Body.Close()
			if !v.Busy {
				if v.LastError != "" {
					t.Fatalf("the wish made no step: %s", v.LastError)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the wish never settled: %+v", v)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
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
	openPanel := func(url string) string {
		t.Helper()
		var prompt string
		if err := chromedp.Run(ctx,
			chromedp.Navigate(url),
			chromedp.WaitVisible(`[data-bashy-genie-btn]`),
			chromedp.Click(`[data-bashy-genie-btn]`),
			chromedp.WaitVisible(`[data-bashy-genie-panel] textarea`),
			chromedp.Text(`[data-bgn-prompt]`, &prompt),
		); err != nil {
			t.Fatal(err)
		}
		return prompt
	}

	// The base: the fixed prompt, the neutral placeholder, no call.
	var ph, text, value string
	if prompt := openPanel(base + "/notes/"); prompt != "What would you like to change, add or make?" {
		t.Errorf("prompt = %q", prompt)
	}
	time.Sleep(500 * time.Millisecond) // the (empty) answer reaches the page
	eval(placeholder, &ph)
	if ph != "Describe what you'd like" {
		t.Errorf("base placeholder = %q, want the neutral one", ph)
	}
	eval(panel+`.textContent + ' ' + `+placeholder, &text)
	for _, old := range []string{"newest item", "whole app", "e.g."} {
		if strings.Contains(text, old) {
			t.Errorf("panel still carries %q:\n%s", old, text)
		}
	}
	if n := genie.calls(); n != 0 {
		t.Fatalf("opening the panel called Genie %d times", n)
	}

	// A wish applied and saved: its NEXT: lines are the placeholder on the
	// next open, with no call made for them.
	post("/api/shape/notes/draft", http.StatusCreated)
	wish("show newest notes first")
	post("/api/shape/notes/draft/save", http.StatusOK)
	if prompt := openPanel(base + "/notes/"); prompt != "What would you like to change, add or make?" {
		t.Errorf("prompt after a version = %q", prompt)
	}
	until("placeholder is the first saved follow-up", func() bool {
		eval(placeholder, &ph)
		return ph == "make the ship faster"
	})
	eval(panel+`.querySelector('textarea').value`, &value)
	if value != "" {
		t.Errorf("textarea prefilled with %q", value)
	}
	if n := genie.calls(); n != 1 {
		t.Fatalf("calls after one wish and two opens = %d, want 1", n)
	}

	// A version without NEXT: a follow-up derived from its wish.
	genie.nmu.Lock()
	genie.next = ""
	genie.nmu.Unlock()
	post("/api/shape/notes/draft", http.StatusCreated)
	wish("add a delete button")
	post("/api/shape/notes/draft/save", http.StatusOK)
	openPanel(base + "/notes/")
	until("placeholder is derived from the last wish", func() bool {
		eval(placeholder, &ph)
		return ph == "Change the delete button"
	})
	if n := genie.calls(); n != 2 {
		t.Fatalf("calls after two wishes and three opens = %d, want 2", n)
	}
	assertNoJSErrors(t, "genie examples", errors())
}
