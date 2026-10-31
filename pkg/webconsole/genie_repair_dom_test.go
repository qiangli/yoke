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

	"github.com/qiangli/yoke/pkg/shape/contain"
	shape "github.com/qiangli/yoke/pkg/shape/state"
)

// S549.R1a slice A, page side: a page whose api fetch fails shows the repair
// overlay; an incident turning activated reloads the page; a failed incident
// shows the honest failure text with the incident id.
func TestDOMRepairOverlay(t *testing.T) {
	source := newNotesSource(t)
	// The BASE revision already fails api/list: loading the page fires the
	// app's own fetch, which answers 500 with an incident id.
	commitFile(t, source, "api/list.md", "# list\n\n```bsh\necho unit-boom-marker >&2\nexit 3\n```\n")
	notes := Panel{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: source,
		Auth: AuthSystem, Source: "registered", Available: true}
	home := t.TempDir()
	base, ctx, errs := domEnv(t, Options{Panels: []Panel{notes},
		Shape: ShapeOptions{Home: home, WorkRoot: t.TempDir(), Runner: []string{"/bin/sh"}, Level: contain.LevelProcess}})

	principal := repairPrincipal(t, base)
	incidentOf := func() Incident {
		t.Helper()
		st, err := shape.OpenAt(home, "notes", principal)
		if err != nil {
			t.Fatal(err)
		}
		s, err := openIncidents(st)
		if err != nil {
			t.Fatal(err)
		}
		list, err := s.List()
		if err != nil || len(list) != 1 {
			t.Fatalf("incidents = %+v, %v: want the one fetch failure", list, err)
		}
		return list[0]
	}
	attempt := func(id, result string) {
		t.Helper()
		st, err := shape.OpenAt(home, "notes", principal)
		if err != nil {
			t.Fatal(err)
		}
		s, err := openIncidents(st)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Attempt(id, RepairAttempt{Candidate: "c1", Result: result, Revision: "r1", At: time.Now()}); err != nil {
			t.Fatal(err)
		}
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
				var text string
				_ = chromedp.Run(ctx, chromedp.Evaluate(`(document.querySelector('[data-bgn-repair-text]') || {}).textContent || ''`, &text))
				t.Fatalf("never true: %s (repair text %q)", expr, text)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// A failing api fetch after the component is live shows the overlay
	// waiting. (The page's own load-time fetch runs before the deferred
	// component installs its wrapper, so the test fires its own; the
	// incident below pins the 500.)
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/notes/"),
		chromedp.WaitVisible(`[data-bashy-genie-btn]`),
		chromedp.Evaluate(`fetch('api/list').then((r) => r.status)`, nil),
		chromedp.WaitVisible(`[data-bgn-repair]`),
	); err != nil {
		t.Fatal(err)
	}
	until(`(document.querySelector('[data-bgn-repair-text]') || {}).textContent.includes('please wait')`)
	in := incidentOf()
	if in.Kind != IncidentAPI || in.Status != http.StatusInternalServerError {
		t.Fatalf("incident = %+v, want the api 500", in)
	}
	var overlayID string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`document.querySelector('[data-bgn-repair]').getAttribute('data-incident')`, &overlayID),
	); err != nil {
		t.Fatal(err)
	}
	if overlayID != in.ID {
		t.Fatalf("overlay incident = %q, want %q", overlayID, in.ID)
	}

	// The repair lands: the page reloads. A marker set after the flip can
	// only disappear through a reload.
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`window.__repairMark = 1; true`, nil),
	); err != nil {
		t.Fatal(err)
	}
	attempt(in.ID, "activated")
	until(`typeof window.__repairMark === 'undefined'`)
	until(`!document.querySelector('[data-bgn-repair]:not([hidden])')`)
	assertNoJSErrors(t, "repair overlay", errs())
}

// TestDOMRepairFailed pins the terminal overlay: an incident that exhausted
// its attempts says it could not fix the error, that the previous version
// is still active, and names the incident.
func TestDOMRepairFailed(t *testing.T) {
	source := newNotesSource(t)
	commitFile(t, source, "api/list.md", "# list\n\n```bsh\nexit 3\n```\n")
	notes := Panel{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: source,
		Auth: AuthSystem, Source: "registered", Available: true}
	home := t.TempDir()
	base, ctx, errs := domEnv(t, Options{Panels: []Panel{notes},
		Shape: ShapeOptions{Home: home, WorkRoot: t.TempDir(), Runner: []string{"/bin/sh"}, Level: contain.LevelProcess}})

	principal := repairPrincipal(t, base)
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/notes/"),
		chromedp.WaitVisible(`[data-bashy-genie-btn]`),
		chromedp.Evaluate(`fetch('api/list').then((r) => r.status)`, nil),
		chromedp.WaitVisible(`[data-bgn-repair]`),
	); err != nil {
		t.Fatal(err)
	}
	st, err := shape.OpenAt(home, "notes", principal)
	if err != nil {
		t.Fatal(err)
	}
	s, err := openIncidents(st)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("incidents = %+v, %v", list, err)
	}
	id := list[0].ID
	for _, result := range []string{"failed", "failed"} {
		if _, err := s.Attempt(id, RepairAttempt{Candidate: "c1", Result: result, Reason: "verifier", At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		var text string
		if err := chromedp.Run(ctx, chromedp.Evaluate(`(document.querySelector('[data-bgn-repair-text]') || {}).textContent || ''`, &text)); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(text, "could not fix") && strings.Contains(text, "still active") && strings.Contains(text, id) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failure text never showed: %q", text)
		}
		time.Sleep(100 * time.Millisecond)
	}
	assertNoJSErrors(t, "repair failure", errs())
}

// A swap or navigation that aborts in-flight asset loads must never report
// an incident: an asset error firing while the page is going away (pagehide)
// is the navigation, not an incident. The control proves the reporting path
// works in-test: the same synthetic asset error without pagehide shows the
// overlay and records one incident.
func TestDOMRepairIgnoresUnloadAborts(t *testing.T) {
	source := newNotesSource(t)
	notes := Panel{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: source,
		Auth: AuthSystem, Source: "registered", Available: true}
	home := t.TempDir()
	base, ctx, errs := domEnv(t, Options{Panels: []Panel{notes},
		Shape: ShapeOptions{Home: home, WorkRoot: t.TempDir(), Runner: []string{"/bin/sh"}, Level: contain.LevelProcess}})

	principal := repairPrincipal(t, base)
	count := func() int {
		t.Helper()
		st, err := shape.OpenAt(home, "notes", principal)
		if err != nil {
			t.Fatal(err)
		}
		s, err := openIncidents(st)
		if err != nil {
			t.Fatal(err)
		}
		list, err := s.List()
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}
	// fireAssetError dispatches a synthetic asset error on an appended
	// script element: the capture-phase listener sees it with its element,
	// exactly like a failed load, without any network behind it.
	const fireAssetError = `(function () { var s = document.createElement('script'); document.body.append(s); s.dispatchEvent(new Event('error')); return true; })()`

	// Control: without an unload, the same error reports and shows the overlay.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/notes/"),
		chromedp.WaitVisible(`[data-bashy-genie-btn]`),
		chromedp.Evaluate(fireAssetError, nil),
		chromedp.WaitVisible(`[data-bgn-repair]`),
	); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 1 {
		t.Fatalf("incidents after a live asset error = %d, want 1", got)
	}

	// The abort case: a fresh page starts going away, then the asset error
	// fires. Nothing is reported and no overlay shows.
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/notes/"),
		chromedp.WaitVisible(`[data-bashy-genie-btn]`),
		chromedp.Evaluate(`window.dispatchEvent(new Event('pagehide')); true`, nil),
		chromedp.Evaluate(fireAssetError, nil),
		chromedp.Sleep(2500*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	var overlay bool
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`!!document.querySelector('[data-bgn-repair]:not([hidden])')`, &overlay),
	); err != nil {
		t.Fatal(err)
	}
	if overlay {
		t.Error("repair overlay shown for an asset error during unload")
	}
	if got := count(); got != 1 {
		t.Errorf("incidents after an unload abort = %d, want still 1", got)
	}
	assertNoJSErrors(t, "unload aborts", errs())
}

// repairPrincipal reads the loopback owner's principal from the Shape state:
// the same identity the page's incident polls and reports use.
func repairPrincipal(t *testing.T, base string) string {
	t.Helper()
	resp, err := http.Get(base + "/api/shape/notes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st struct {
		Principal string `json:"principal"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil || st.Principal == "" {
		t.Fatalf("shape state principal = %+v, %v", st, err)
	}
	return st.Principal
}
