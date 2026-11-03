// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build verifydom

package webconsole

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/qiangli/yoke/pkg/shape/contain"
)

// refusingVerifier refuses the first verification with a reason and passes
// afterwards: the review bar must show the refusal, and the next Approve
// must activate.
type refusingVerifier struct {
	mu    sync.Mutex
	calls int
}

func (v *refusingVerifier) Verify(_ context.Context, req VerifyRequest) (Verdict, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	if v.calls == 1 {
		return Verdict{Pass: false, Detail: "smoke check failed: the heading must keep its text"}, nil
	}
	return Verdict{Pass: true, Evidence: "evidence-" + req.CandidateRev[:8]}, nil
}

// TestDOMGenieReviewBar drives the in-place review (S526.B1g scope
// addition, F4) in a real browser. A ready candidate of the notes app,
// opened at its preview, shows the review bar: the wish, "Reviewing
// candidate <id>", the plain-words summary (coverage + changed files), the
// changed heading outlined, the code diff one click away. Approve shows the
// trusted verifier's refusal with its reason and keeps the candidate;
// Before shows the live page with a note and After returns; a protected
// page reached from the review says so and leads back; Approve again
// activates and lands on the live app with no note left. A second
// candidate is rejected from the bar. No script throws anywhere.
func TestDOMGenieReviewBar(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source := newNotesSource(t)
	notes := Panel{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: source,
		Auth: AuthSystem, Source: "registered", Available: true}
	verifier := &refusingVerifier{}
	base, ctx, errs := domEnv(t, Options{Panels: []Panel{notes},
		Shape: ShapeOptions{Home: home, WorkRoot: t.TempDir(), Runner: []string{"/bin/sh"},
			Level: contain.LevelProcess, Delegate: &fakeShaper{},
			Verifier: verifier, Activator: &fakeActivator{home: home}}})

	// The test's own HTTP client is the same loopback principal as the
	// browser, so a wish made here is the browser's candidate.
	client := &http.Client{Timeout: 10 * time.Second}
	wish := func(text string) Candidate {
		t.Helper()
		resp, err := client.Post(base+"/api/shape/notes/candidates", "application/json",
			strings.NewReader(`{"request":`+jsonString(text)+`,"page":"/","element":"h1 \"Notes\"","protected":false}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var c Candidate
		if err := json.NewDecoder(resp.Body).Decode(&c); err != nil || resp.StatusCode != http.StatusAccepted {
			t.Fatalf("wish = %d, %v", resp.StatusCode, err)
		}
		deadline := time.Now().Add(20 * time.Second)
		for {
			r2, err := client.Get(base + "/api/shape/notes/candidates/" + c.ID)
			if err != nil {
				t.Fatal(err)
			}
			var cur Candidate
			_ = json.NewDecoder(r2.Body).Decode(&cur)
			r2.Body.Close()
			if cur.Status == candReady {
				return cur
			}
			if cur.Status != candRunning || time.Now().After(deadline) {
				t.Fatalf("the wish never became ready: %+v", cur)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	c := wish("show the newest note first")
	preview := base + "/shape/notes/preview/" + c.ID + "/"

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
				var bar string
				_ = chromedp.Run(ctx, chromedp.Evaluate(`((document.querySelector('.bgn-bar') || {}).textContent || '') + ' @ ' + location.href + ' mark=' + sessionStorage.getItem('bashy.genie.review') + ' tag=' + JSON.stringify((document.querySelector('[data-bashy-genie]') || {}).dataset || null)`, &bar))
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
	const bar = `document.querySelector('[data-bgn-review]')`

	// The preview: the review bar, pinned, with the wish and the summary;
	// the sparkle button sits in it, disabled; the changed heading is
	// outlined and the body was moved down under the bar.
	if err := chromedp.Run(ctx, chromedp.Navigate(preview), chromedp.WaitVisible(`[data-bgn-review]`)); err != nil {
		t.Fatal(err)
	}
	until(bar + `.querySelector('.bgn-wish').textContent === 'show the newest note first'`)
	until(bar + `.querySelector('.bgn-sum').textContent.includes('Changes 2 files')`)
	until(`!!document.querySelector('h1.bgn-changed')`)
	head := text(bar + `.querySelector('.bgn-tag').textContent`)
	sum := text(bar + `.querySelector('.bgn-sum').textContent`)
	if head != "Reviewing candidate "+c.ID {
		t.Errorf("bar head = %q", head)
	}
	for _, want := range []string{"Does: newest notes first; updated heading", "Not done: export remains to do", "public/index.html", "api/list.md", "differs from your current version"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary lacks %q: %q", want, sum)
		}
	}
	// The body is moved down by the bar's height (the bar grows as the
	// summary arrives, so this settles rather than holds at once).
	until(`parseFloat(document.body.style.marginTop) >= document.querySelector('[data-bgn-review]').offsetHeight`)
	var inBar, fixed bool
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`!!document.querySelector('[data-bgn-review] [data-bashy-genie-btn="preview"][aria-disabled="true"]')`, &inBar),
		chromedp.Evaluate(`getComputedStyle(document.querySelector('[data-bgn-review]')).position === 'fixed'`, &fixed),
	); err != nil {
		t.Fatal(err)
	}
	if !inBar || !fixed {
		t.Errorf("bar button in bar=%v fixed=%v", inBar, fixed)
	}
	if got := text(`document.querySelector('h1').textContent`); !strings.Contains(got, "newest first") {
		t.Errorf("the preview does not show the candidate's page: h1 %q", got)
	}

	// The code diff, one click away; Escape closes it.
	if err := chromedp.Run(ctx, chromedp.Click(`[data-bgn-review] .bgn-b[aria-label^="Show the code diff"]`), chromedp.WaitVisible(`[data-bgn-diff] pre`)); err != nil {
		t.Fatal(err)
	}
	if got := text(`document.querySelector('[data-bgn-diff] pre').textContent`); !strings.Contains(got, "+++ b/public/index.html") || !strings.Contains(got, "newest first") {
		t.Errorf("diff sheet: %q", got[:min(len(got), 300)])
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape', bubbles: true})); true`, nil)); err != nil {
		t.Fatal(err)
	}
	until(`document.querySelector('[data-bgn-diff]').hidden === true`)

	// Approve: the trusted verifier refuses; the bar says why and the
	// candidate stays.
	if err := chromedp.Run(ctx, chromedp.Click(`[data-bgn-review] .bgn-p`)); err != nil {
		t.Fatal(err)
	}
	until(bar + `.querySelector('.bgn-alert').textContent.includes('Not approved: smoke check failed: the heading must keep its text')`)
	until(`location.href === ` + jsonString(preview))
	if resp, err := client.Get(base + "/api/shape/notes/candidates/" + c.ID); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("candidate after refusal: %v %v", resp, err)
	}

	// Before: the live page, with a note; After: back inside the candidate.
	if err := chromedp.Run(ctx, chromedp.Click(`[data-bgn-review] .bgn-b[aria-label^="Show your current version"]`), chromedp.WaitVisible(`[data-bgn-review-note]`)); err != nil {
		t.Fatal(err)
	}
	until(`location.href === ` + jsonString(base+"/notes/"))
	if got := text(`document.querySelector('[data-bgn-review-note]').textContent`); !strings.Contains(got, "Before: your current version") || !strings.Contains(got, c.ID) {
		t.Errorf("before note: %q", got)
	}
	if got := text(`document.querySelector('h1').textContent`); got != "Notes" {
		t.Errorf("the live page is not the current version: h1 %q", got)
	}
	if err := chromedp.Run(ctx, chromedp.Click(`[data-bgn-review-note] .bgn-p`), chromedp.WaitVisible(`[data-bgn-review]`)); err != nil {
		t.Fatal(err)
	}
	until(`location.href === ` + jsonString(preview))

	// A protected page reached from the review says so and leads back.
	if err := chromedp.Run(ctx, chromedp.Navigate(base+"/shape/notes/")); err != nil {
		t.Fatal(err)
	}
	until(`!!document.querySelector('[data-bgn-review-note]')`)
	if got := text(`document.querySelector('[data-bgn-review-note]').textContent`); !strings.Contains(got, "This page is protected") || !strings.Contains(got, c.ID) {
		t.Errorf("protected note: %q", got)
	}
	// shape.js fetches the candidate list independently of the Genie banner.
	// Wait for the actual card, retaining the same exact-text assertion.
	until(`document.querySelector('#shape-list a.btn') && document.querySelector('#shape-list a.btn').textContent === 'Review in place'`)
	if got := text(`document.querySelector('#shape-list a.btn') ? document.querySelector('#shape-list a.btn').textContent : ''`); got != "Review in place" {
		t.Errorf("Shape page card link = %q, want Review in place", got)
	}
	if err := chromedp.Run(ctx, chromedp.Click(`[data-bgn-review-note] .bgn-p`), chromedp.WaitVisible(`[data-bgn-review]`)); err != nil {
		t.Fatal(err)
	}
	until(`location.href === ` + jsonString(preview))

	// Approve again: verified, activated, the live app shows the change
	// and no note is left behind.
	if err := chromedp.Run(ctx, chromedp.Click(`[data-bgn-review] .bgn-p`)); err != nil {
		t.Fatal(err)
	}
	until(`location.href === ` + jsonString(base+"/notes/") + ` && document.querySelector('h1') && document.querySelector('h1').textContent.includes('newest first')`)
	if got := text(`document.querySelector('[data-bgn-review-note], [data-bgn-review]') ? 'bar' : ''`); got != "" {
		t.Error("a review bar is left on the live page after approval")
	}
	if resp, err := client.Get(base + "/api/shape/notes/candidates/" + c.ID); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("approved candidate still listed: %v %v", resp, err)
	}
	verifier.mu.Lock()
	calls := verifier.calls
	verifier.mu.Unlock()
	if calls != 2 {
		t.Errorf("verifier ran %d times, want 2 (once per Approve)", calls)
	}

	// Reject from the bar: armed on the first click, done on the second.
	c2 := wish("add an export button")
	if err := chromedp.Run(ctx, chromedp.Navigate(base+"/shape/notes/preview/"+c2.ID+"/"), chromedp.WaitVisible(`[data-bgn-review]`),
		chromedp.Click(`[data-bgn-review] .bgn-danger`)); err != nil {
		t.Fatal(err)
	}
	until(bar + `.querySelector('.bgn-danger').textContent.includes('sure')`)
	if err := chromedp.Run(ctx, chromedp.Click(`[data-bgn-review] .bgn-danger`)); err != nil {
		t.Fatal(err)
	}
	until(`location.href === ` + jsonString(base+"/notes/"))
	if resp, err := client.Get(base + "/api/shape/notes/candidates/" + c2.ID); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("rejected candidate still listed: %v %v", resp, err)
	}
	assertNoJSErrors(t, "review bar", errs())
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(errors.New("jsonString: " + err.Error()))
	}
	return string(b)
}
