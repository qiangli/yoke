// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build verifydom

package webconsole

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"github.com/qiangli/yoke/pkg/board"
	"github.com/qiangli/yoke/pkg/capability"
)

// The Runs and Leaderboard sections on /sprint/ collapse with the SAME idiom the
// bottom panels use (a bd-panel-head <button> over a bd-panel-body div, state in
// state.open). These are browser tests because the failure modes are all in the
// DOM: #bd-lanes is `display:grid`, which outranks the UA [hidden] rule, so a
// hidden flag set on the wrong element toggles state while the section stays on
// screen — invisible to any byte-level test.

// sectionProbe reports the observable state of one collapsible section: its
// toggle button and whether its content is actually rendered on screen.
const sectionProbe = `(function(sec, content){
  const s = document.getElementById(sec);
  const b = s && s.querySelector(':scope > button.bd-panel-head');
  const c = document.getElementById(content);
  const body = c && c.closest('.bd-panel-body');
  return JSON.stringify({
    button: !!b,
    tag: b ? b.tagName : '',
    type: b ? b.getAttribute('type') : '',
    expanded: b ? b.getAttribute('aria-expanded') : '',
    controls: b ? b.getAttribute('aria-controls') : '',
    name: b ? b.textContent.trim() : '',
    bodyID: body ? body.id : '',
    bodyHidden: body ? body.hidden : null,
    shown: c ? c.getBoundingClientRect().height > 0 : false,
    text: c ? c.textContent : '',
  });
})`

type sectionView struct {
	Button     bool   `json:"button"`
	Tag        string `json:"tag"`
	Type       string `json:"type"`
	Expanded   string `json:"expanded"`
	Controls   string `json:"controls"`
	Name       string `json:"name"`
	BodyID     string `json:"bodyID"`
	BodyHidden *bool  `json:"bodyHidden"`
	Shown      bool   `json:"shown"`
	Text       string `json:"text"`
}

func probeSection(t *testing.T, ctx context.Context, sec, content string) sectionView {
	t.Helper()
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(sectionProbe+`(`+jsq(sec)+`,`+jsq(content)+`)`, &raw)); err != nil {
		t.Fatalf("probe %s: %v", sec, err)
	}
	var v sectionView
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("probe %s: %v (%s)", sec, err, raw)
	}
	return v
}

func jsq(s string) string { b, _ := json.Marshal(s); return string(b) }

func clickToggle(t *testing.T, ctx context.Context, sec string) {
	t.Helper()
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`document.querySelector('#`+sec+` > button.bd-panel-head')?.click()`, nil)); err != nil {
		t.Fatalf("click %s: %v", sec, err)
	}
}

// stubBoardWithLanes is stubBoard plus one lane holding one run, so the Runs
// section has real content whose visibility can be measured.
func stubBoardWithLanes(t *testing.T) {
	t.Helper()
	stubBoard(t)
	base := collectBoardFn
	stubBoardWith(t, func() *board.Board {
		b, _ := base(context.Background())
		b.Lanes = []board.Lane{{ID: "working", Title: "Working", Cards: []board.Card{
			{Layer: "run", ID: "r1", Label: "the visible run", State: "working"}}}}
		return b
	})
}

type collapseCase struct{ sec, content, name string }

var collapseCases = []collapseCase{
	{"bd-sec-runs", "bd-lanes", "Runs"},
	{"bd-sec-leaderboard", "bd-leaderboard", "Leaderboard"},
}

func testSectionCollapses(t *testing.T, c collapseCase, marker string) {
	stubBoardWithLanes(t)
	seedLeaderboard(t, leaderboardRecords(), leaderboardMatrix())
	base, ctx, errs := domEnv(t, Options{})

	if err := chromedp.Run(ctx, chromedp.Navigate(base+"/sprint/"), chromedp.Sleep(2*time.Second)); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	// Default OPEN: an upgrade must not silently empty anyone's board.
	v := probeSection(t, ctx, c.sec, c.content)
	if !v.Button {
		t.Fatalf("%s has no toggle button", c.name)
	}
	if !v.Shown || v.BodyHidden == nil || *v.BodyHidden || v.Expanded != "true" {
		t.Fatalf("%s must default open, got %+v", c.name, v)
	}
	if !has(v.Text, marker) {
		t.Fatalf("%s content missing %q: %q", c.name, marker, v.Text)
	}

	clickToggle(t, ctx, c.sec)
	v = probeSection(t, ctx, c.sec, c.content)
	if v.BodyHidden == nil || !*v.BodyHidden || v.Shown || v.Expanded != "false" {
		t.Fatalf("%s: click did not collapse it (the [hidden] flag must reach a body the cascade cannot override): %+v", c.name, v)
	}

	// The choice is remembered per viewer across a reload.
	if err := chromedp.Run(ctx, chromedp.Navigate(base+"/sprint/"), chromedp.Sleep(2*time.Second)); err != nil {
		t.Fatalf("reload: %v", err)
	}
	v = probeSection(t, ctx, c.sec, c.content)
	if v.Shown || v.Expanded != "false" {
		t.Errorf("%s: collapse did not survive a reload: %+v", c.name, v)
	}

	clickToggle(t, ctx, c.sec)
	v = probeSection(t, ctx, c.sec, c.content)
	if !v.Shown || v.Expanded != "true" || !has(v.Text, marker) {
		t.Errorf("%s: click did not reopen it with its content: %+v", c.name, v)
	}
	assertNoJSErrors(t, c.name+" collapse", errs())
}

func TestRunsSectionCollapses(t *testing.T) {
	testSectionCollapses(t, collapseCases[0], "the visible run")
}

func TestLeaderboardSectionCollapses(t *testing.T) {
	testSectionCollapses(t, collapseCases[1], "beta:two")
}

// A refresh while collapsed must leave the section collapsed, keep updating its
// data underneath, and show the fresh data on reopen. Collapsing is
// presentation; it must never gate loading.
func TestCollapsedSectionStillUpdatesOnRefresh(t *testing.T) {
	stubBoardWithLanes(t)
	seedLeaderboard(t, leaderboardRecords(), leaderboardMatrix())
	base, ctx, errs := domEnv(t, Options{})

	if err := chromedp.Run(ctx, chromedp.Navigate(base+"/sprint/"), chromedp.Sleep(2*time.Second)); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	for _, c := range collapseCases {
		clickToggle(t, ctx, c.sec)
	}

	// Change the leaderboard's ledger, and the board's lane, behind the page.
	recs := leaderboardRecords()
	pass := true
	for range 8 {
		recs = append(recs, capability.RunRecord{Agent: "delta:four", Source: capability.SourceWeave, GatePass: &pass})
	}
	seedLeaderboard(t, recs, leaderboardMatrix())
	const swapLane = `(function(){
	  const of = window.fetch;
	  window.fetch = (u, o) => of(u, o).then((r) => {
	    if (!new URL(u).pathname.endsWith('/api/sprint')) return r;
	    return r.json().then((d) => {
	      d.lanes = [{id: 'working', title: 'Working', cards: [{layer: 'run', id: 'r2', label: 'the refreshed run', state: 'working'}]}];
	      return new Response(JSON.stringify(d), {status: 200});
	    });
	  });
	})()`
	var focused string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(swapLane, nil),
		chromedp.Focus(`#bd-sec-runs > button.bd-panel-head`),
		chromedp.Evaluate(`load().then(() => new Promise((res) => setTimeout(res, 800)))`, nil, awaitPromise),
		chromedp.Evaluate(`document.activeElement?.closest('#bd-sec-runs') ? 'runs-button' : String(document.activeElement?.tagName)`, &focused),
	); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	runs := probeSection(t, ctx, "bd-sec-runs", "bd-lanes")
	lb := probeSection(t, ctx, "bd-sec-leaderboard", "bd-leaderboard")
	for name, v := range map[string]sectionView{"Runs": runs, "Leaderboard": lb} {
		if v.Shown || v.Expanded != "false" || v.BodyHidden == nil || !*v.BodyHidden {
			t.Errorf("%s reopened itself on refresh: %+v", name, v)
		}
	}
	if !has(runs.Text, "the refreshed run") || has(runs.Text, "the visible run") {
		t.Errorf("collapsed Runs stopped updating: %q", runs.Text)
	}
	if !has(lb.Text, "delta:four") {
		t.Errorf("collapsed Leaderboard stopped updating: %q", lb.Text)
	}
	if focused != "runs-button" {
		t.Errorf("a refresh dropped keyboard focus from the toggle (now on %s)", focused)
	}

	for _, c := range collapseCases {
		clickToggle(t, ctx, c.sec)
	}
	runs = probeSection(t, ctx, "bd-sec-runs", "bd-lanes")
	lb = probeSection(t, ctx, "bd-sec-leaderboard", "bd-leaderboard")
	if !runs.Shown || !has(runs.Text, "the refreshed run") {
		t.Errorf("reopened Runs is stale or hidden: %+v", runs)
	}
	if !lb.Shown || !has(lb.Text, "delta:four") {
		t.Errorf("reopened Leaderboard is stale or hidden: %+v", lb)
	}
	assertNoJSErrors(t, "collapsed refresh", errs())
}

// The toggles are real buttons: keyboard reachable, named, and announcing
// their state. Also: the page must render with browser storage that throws (a
// private window), and the Duty Ladder section beside them must be untouched.
func TestCollapseTogglesAreButtonsWithAriaExpanded(t *testing.T) {
	stubBoardWithLanes(t)
	seedLeaderboard(t, leaderboardRecords(), leaderboardMatrix())
	base, ctx, errs := domEnv(t, Options{})

	if err := chromedp.Run(ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(`
			  Storage.prototype.getItem = () => { throw new Error('storage denied'); };
			  Storage.prototype.setItem = () => { throw new Error('storage denied'); };`).Do(ctx)
			return err
		}),
		chromedp.Navigate(base+"/sprint/"), chromedp.Sleep(2*time.Second)); err != nil {
		t.Fatalf("chromedp: %v", err)
	}

	for _, c := range collapseCases {
		v := probeSection(t, ctx, c.sec, c.content)
		if v.Tag != "BUTTON" || v.Type != "button" {
			t.Errorf("%s toggle = <%s type=%q>, want <button type=button>", c.name, v.Tag, v.Type)
		}
		if !has(v.Name, c.name) {
			t.Errorf("%s toggle accessible name = %q", c.name, v.Name)
		}
		if v.Expanded != "true" || !v.Shown {
			t.Errorf("%s must render open with storage denied: %+v", c.name, v)
		}
		if v.Controls == "" || v.Controls != v.BodyID {
			t.Errorf("%s aria-controls = %q, want its body id %q", c.name, v.Controls, v.BodyID)
		}

		// Keyboard: focus + Enter is a native button activation.
		if err := chromedp.Run(ctx,
			chromedp.Focus(`#`+c.sec+` > button.bd-panel-head`),
			chromedp.KeyEvent(kb.Enter)); err != nil {
			t.Fatalf("keyboard %s: %v", c.name, err)
		}
		v = probeSection(t, ctx, c.sec, c.content)
		if v.Expanded != "false" || v.Shown {
			t.Errorf("%s: Enter on the focused toggle did not collapse it: %+v", c.name, v)
		}
	}

	var dutyOK string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`(function(){
	  const d = document.getElementById('bd-leaderboard-duty');
	  const h = d.previousElementSibling;
	  return (h && h.tagName === 'H2' && h.textContent === 'Duty Ladder' && !d.closest('.bd-panel-body') &&
	    !h.querySelector('button')) ? 'ok' : 'changed';
	})()`, &dutyOK)); err != nil {
		t.Fatalf("duty: %v", err)
	}
	if dutyOK != "ok" {
		t.Errorf("the Duty Ladder section must be left alone")
	}
	assertNoJSErrors(t, "collapse toggles", errs())
}

func has(s, sub string) bool { return strings.Contains(s, sub) }
