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

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
	"github.com/qiangli/yoke/pkg/board"
)

// The sprint filter bar on /sprint/ (Story #1732) narrows the SPRINTS section
// client-side: facets derived from the payload, AND across facets, OR within,
// persisted per viewer and in the URL hash, with a reset. These are browser
// tests because every failure mode is in the DOM — a checkbox that toggles
// state while the cards stay, a facet rebuilt out from under a focused input,
// a bar that overflows a phone — and none of it is visible to a byte test.

// filterBoard is four live sprints plus one finished, shaped so every facet has
// at least two values and an absent one: epics alpha/alpha/beta/none, managers
// ada/none/bea/ada, a stale lease, and a gate on two of them. The finished
// sprint only arrives under all=1, which is what the recompute test needs.
func filterBoard() *board.Board {
	return &board.Board{
		SchemaVersion: board.SchemaVersion, Role: "steward", Scope: "machine-global",
		Title: "Bashy Steward Board", GeneratedAt: time.Now().UTC(),
		Sprints: []board.Sprint{
			{ID: 101, Title: "alpha one", Epic: "alpha", Column: "doing", Manager: "ada", GateState: "complete"},
			{ID: 102, Title: "alpha two", Epic: "alpha", Column: "review"},
			{ID: 103, Title: "beta one", Epic: "beta", Column: "doing", Conductor: "bea", LeaseStale: true, GateState: "pending"},
			{ID: 104, Title: "orphan work", Column: "doing", Manager: "ada"},
			{ID: 105, Title: "old finished", Epic: "beta", Column: "done", Manager: "bea"},
		},
		Todos: []board.Todo{
			{ID: "s101a", Number: 1, Title: "story of alpha one", Status: "todo", SprintID: 101},
			{ID: "s103a", Number: 2, Title: "story of beta one", Status: "doing", SprintID: 103},
		},
	}
}

func stubFilterBoard(t *testing.T) {
	t.Helper()
	stubBoardWith(t, filterBoard)
}

// filterProbe reports what the bar and the cards actually show.
const filterProbe = `(function(){
  const bar = document.getElementById('bd-filter');
  const facets = {};
  for (const g of bar ? bar.querySelectorAll('.bd-facet') : []) {
    facets[g.dataset.facet] = [...g.querySelectorAll('label.chk')].map((l) => ({
      value: l.querySelector('input').value,
      label: l.querySelector('.v').textContent,
      count: Number(l.querySelector('.n').textContent),
      checked: l.querySelector('input').checked,
    }));
  }
  const host = document.getElementById('bd-sprints');
  return JSON.stringify({
    bar: !!bar,
    open: bar ? bar.open : false,
    state: bar ? bar.querySelector('summary .state').textContent : '',
    note: bar ? bar.querySelector('.note').textContent : '',
    resetDisabled: bar ? bar.querySelector('#bd-filter-reset').disabled : null,
    header: host.querySelector(':scope > header .n').textContent,
    cards: [...host.querySelectorAll('.bd-sprint .id')].map((x) => x.textContent),
    empty: host.querySelector('.cards .empty') ? host.querySelector('.cards .empty').textContent : '',
    hash: location.hash,
    stored: (() => { try { return localStorage.getItem('bashy.board.filter') || ''; } catch (_) { return 'ERR'; } })(),
    facets,
  });
})()`

type facetOpt struct {
	Value   string `json:"value"`
	Label   string `json:"label"`
	Count   int    `json:"count"`
	Checked bool   `json:"checked"`
}

type filterView struct {
	Bar           bool                  `json:"bar"`
	Open          bool                  `json:"open"`
	State         string                `json:"state"`
	Note          string                `json:"note"`
	ResetDisabled *bool                 `json:"resetDisabled"`
	Header        string                `json:"header"`
	Cards         []string              `json:"cards"`
	Empty         string                `json:"empty"`
	Hash          string                `json:"hash"`
	Stored        string                `json:"stored"`
	Facets        map[string][]facetOpt `json:"facets"`
}

func probeFilter(t *testing.T, ctx context.Context) filterView {
	t.Helper()
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(filterProbe, &raw)); err != nil {
		t.Fatalf("probe filter: %v", err)
	}
	var v filterView
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("probe filter: %v (%s)", err, raw)
	}
	return v
}

func (v filterView) opt(t *testing.T, facet, value string) facetOpt {
	t.Helper()
	for _, o := range v.Facets[facet] {
		if o.Value == value {
			return o
		}
	}
	t.Fatalf("facet %s has no value %q: %+v", facet, value, v.Facets[facet])
	return facetOpt{}
}

func (v filterView) values(facet string) []string {
	var out []string
	for _, o := range v.Facets[facet] {
		out = append(out, o.Value)
	}
	return out
}

// facetBox finds one checkbox by facet + value in JS.
func facetBox(facet, value string) string {
	return `document.querySelector('#bd-filter .bd-facet[data-facet=' + ` + jsq(facet) + ` + '] input[value=' + JSON.stringify(` + jsq(value) + `) + ']')`
}

func clickBox(t *testing.T, ctx context.Context, facet, value string) {
	t.Helper()
	if err := chromedp.Run(ctx, chromedp.Evaluate(facetBox(facet, value)+`.click()`, nil)); err != nil {
		t.Fatalf("click %s=%s: %v", facet, value, err)
	}
}

// clickOnly clicks the NAME beside a checkbox — the "only this one" gesture.
func clickOnly(t *testing.T, ctx context.Context, facet, value string) {
	t.Helper()
	if err := chromedp.Run(ctx, chromedp.Evaluate(facetBox(facet, value)+`.parentNode.querySelector('.v').click()`, nil)); err != nil {
		t.Fatalf("click only %s=%s: %v", facet, value, err)
	}
}

func clickFacetLink(t *testing.T, ctx context.Context, facet, text string) {
	t.Helper()
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`[...document.querySelectorAll('#bd-filter .bd-facet[data-facet='+`+jsq(facet)+`+'] legend .lnk')].find((b) => b.textContent === `+jsq(text)+`).click()`, nil)); err != nil {
		t.Fatalf("click %s %s: %v", facet, text, err)
	}
}

func openSprintPage(t *testing.T, ctx context.Context, u string) {
	t.Helper()
	if err := chromedp.Run(ctx, chromedp.Navigate(u), chromedp.Sleep(2*time.Second)); err != nil {
		t.Fatalf("navigate %s: %v", u, err)
	}
}

func eqStrings(a, b []string) bool { return strings.Join(a, "\x00") == strings.Join(b, "\x00") }

// Facets come from the payload: every distinct value that occurs, with its
// count, the absent value included, and NOTHING that does not occur — no
// hard-coded kanban columns, no "done" while history is hidden.
func TestSprintFilterFacetsDeriveFromPayload(t *testing.T) {
	stubFilterBoard(t)
	base, ctx, errs := domEnv(t, Options{})
	openSprintPage(t, ctx, base+"/sprint/")

	v := probeFilter(t, ctx)
	if !v.Bar {
		t.Fatal("no #bd-filter bar above the sprint cards")
	}
	if !eqStrings(v.values("epic"), []string{"", "alpha", "beta"}) {
		t.Errorf("epic facet = %v, want (no epic), alpha, beta", v.values("epic"))
	}
	if o := v.opt(t, "epic", ""); o.Label != "(no epic)" || o.Count != 1 || !o.Checked {
		t.Errorf("(no epic) option = %+v, want labelled, count 1, checked", o)
	}
	if o := v.opt(t, "epic", "alpha"); o.Count != 2 || !o.Checked {
		t.Errorf("alpha option = %+v, want count 2, checked", o)
	}
	if !eqStrings(v.values("col"), []string{"doing", "review"}) {
		t.Errorf("column facet = %v, want only the columns present (doing, review) — not a hard-coded list and not the hidden done", v.values("col"))
	}
	if !eqStrings(v.values("mgr"), []string{"", "ada", "bea"}) {
		t.Errorf("manager facet = %v, want (unclaimed), ada, bea (conductor counts as manager)", v.values("mgr"))
	}
	if o := v.opt(t, "mgr", ""); o.Label != "(unclaimed)" || o.Count != 1 {
		t.Errorf("(unclaimed) option = %+v", o)
	}
	if o := v.opt(t, "mgr", "ada"); o.Count != 2 {
		t.Errorf("ada option = %+v, want count 2", o)
	}
	if !eqStrings(v.values("gate"), []string{"", "complete", "pending"}) {
		t.Errorf("gate facet = %v", v.values("gate"))
	}
	if v.Header != "4 of 4 sprints · 1 finished hidden" {
		t.Errorf("header = %q, want %q", v.Header, "4 of 4 sprints · 1 finished hidden")
	}
	if len(v.Cards) != 4 || v.State != "" || v.ResetDisabled == nil || !*v.ResetDisabled {
		t.Errorf("default view must show every card with no active filter: %+v", v)
	}
	if !v.Open {
		t.Errorf("the bar must default open at desktop width")
	}
	assertNoJSErrors(t, "facets", errs())
}

// Facets recompute when "include history" changes the payload: the done
// column and the finished sprint's manager appear only under all=1.
func TestSprintFilterFacetsRecomputeOnHistoryToggle(t *testing.T) {
	stubFilterBoard(t)
	base, ctx, errs := domEnv(t, Options{})
	openSprintPage(t, ctx, base+"/sprint/")

	if err := chromedp.Run(ctx,
		chromedp.Click(`#f-all`),
		chromedp.Sleep(1500*time.Millisecond),
	); err != nil {
		t.Fatalf("toggle history: %v", err)
	}
	v := probeFilter(t, ctx)
	if !eqStrings(v.values("col"), []string{"doing", "done", "review"}) {
		t.Errorf("column facet after all=1 = %v, want doing, done, review", v.values("col"))
	}
	if o := v.opt(t, "epic", "beta"); o.Count != 2 {
		t.Errorf("beta count after all=1 = %d, want 2", o.Count)
	}
	if v.Header != "5 of 5 sprints" || len(v.Cards) != 5 {
		t.Errorf("header/cards after all=1 = %q / %v", v.Header, v.Cards)
	}
	assertNoJSErrors(t, "recompute", errs())
}

// AND across facets, OR within one, counts in the header, "only this one" on
// the name, all/none on the group, text + stale, and an empty result that names
// the filters to clear.
func TestSprintFilterCombinesAndOrWithCounts(t *testing.T) {
	stubFilterBoard(t)
	base, ctx, errs := domEnv(t, Options{})
	openSprintPage(t, ctx, base+"/sprint/")

	// Untick one epic: OR within the facet keeps the other two values.
	clickBox(t, ctx, "epic", "alpha")
	v := probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#103", "#104"}) {
		t.Fatalf("epic minus alpha: cards = %v, want #103 #104", v.Cards)
	}
	if v.Header != "2 of 4 sprints · 1 finished hidden" {
		t.Errorf("header = %q", v.Header)
	}
	if !has(v.State, "2 of 4") || !has(v.State, "epic") {
		t.Errorf("summary state = %q, want the count and the active facet", v.State)
	}
	if !has(v.Note, "Runs lanes below are not filtered") {
		t.Errorf("the bar must say plainly that the runs lanes do not follow it: %q", v.Note)
	}
	// Facet counts stay the payload's, so a reader can see what a tick would add back.
	if o := v.opt(t, "epic", "alpha"); o.Checked || o.Count != 2 {
		t.Errorf("alpha after untick = %+v", o)
	}

	// AND with a second facet: manager ada ∩ epic∉{alpha} = #104 only.
	clickBox(t, ctx, "mgr", "")
	clickBox(t, ctx, "mgr", "bea")
	v = probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#104"}) {
		t.Fatalf("epic∉alpha AND mgr=ada: cards = %v, want #104", v.Cards)
	}

	// The manager facet is now ada-only; "all" restores the group without
	// touching the epic facet.
	clickFacetLink(t, ctx, "mgr", "all")
	v = probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#103", "#104"}) {
		t.Fatalf("mgr all: cards = %v, want #103 #104", v.Cards)
	}

	// "none" empties the group — nothing matches, and the empty line says what to clear.
	clickFacetLink(t, ctx, "mgr", "none")
	v = probeFilter(t, ctx)
	if len(v.Cards) != 0 || !has(v.Empty, "No sprint matches") || !has(v.Empty, "epic") || !has(v.Empty, "manager") {
		t.Fatalf("mgr none: cards = %v empty = %q, want an empty result naming epic and manager", v.Cards, v.Empty)
	}
	if v.Header != "0 of 4 sprints · 1 finished hidden" {
		t.Errorf("header = %q", v.Header)
	}

	// Clicking a NAME shows only that value, whatever the group held before.
	clickOnly(t, ctx, "mgr", "ada")
	clickFacetLink(t, ctx, "epic", "all")
	v = probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#101", "#104"}) {
		t.Fatalf("only ada: cards = %v, want #101 #104", v.Cards)
	}
	if o := v.opt(t, "mgr", "bea"); o.Checked {
		t.Errorf("only ada must untick bea: %+v", o)
	}
	// Ticking the remaining values back covers the whole facet, which is the
	// default again — so a value that appears later is not excluded.
	clickBox(t, ctx, "mgr", "")
	clickBox(t, ctx, "mgr", "bea")
	v = probeFilter(t, ctx)
	if len(v.Cards) != 4 || v.Hash != "#" && v.Hash != "" {
		t.Fatalf("re-ticking every manager must be the default again: cards=%v hash=%q", v.Cards, v.Hash)
	}

	// Text match on title or number; stale-only on top of it.
	if err := chromedp.Run(ctx,
		chromedp.SendKeys(`#bd-filter-q`, "one"),
		chromedp.Sleep(300*time.Millisecond),
	); err != nil {
		t.Fatalf("type: %v", err)
	}
	v = probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#101", "#103"}) {
		t.Fatalf("q=one: cards = %v, want #101 #103", v.Cards)
	}
	if err := chromedp.Run(ctx, chromedp.Click(`#bd-filter-stale`), chromedp.Sleep(300*time.Millisecond)); err != nil {
		t.Fatalf("stale: %v", err)
	}
	v = probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#103"}) {
		t.Fatalf("q=one AND stale: cards = %v, want #103", v.Cards)
	}
	// The story chips under a shown card follow it; a hidden card's stories go with it.
	var chips string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`[...document.querySelectorAll('#bd-sprints .bd-sprint .refs .ref')].map((c) => c.textContent).join(',')`, &chips)); err != nil {
		t.Fatal(err)
	}
	if chips != "#2" {
		t.Errorf("stories shown = %q, want only #2 (the story of the one visible sprint)", chips)
	}
	// A number, with or without the hash.
	if err := chromedp.Run(ctx,
		chromedp.Click(`#bd-filter-stale`),
		chromedp.Evaluate(`document.getElementById('bd-filter-q').value = ''`, nil),
		chromedp.SendKeys(`#bd-filter-q`, "#102"),
		chromedp.Sleep(300*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	v = probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#102"}) {
		t.Fatalf("q=#102: cards = %v", v.Cards)
	}
	assertNoJSErrors(t, "combine", errs())
}

// The selection is in the URL hash (a filtered view is a link), survives a
// reload with no hash (localStorage), the hash wins over storage, and Reset
// restores the defaults everywhere.
func TestSprintFilterPersistsInHashAndStorageAndResets(t *testing.T) {
	stubFilterBoard(t)
	base, ctx, errs := domEnv(t, Options{})
	openSprintPage(t, ctx, base+"/sprint/")

	clickOnly(t, ctx, "epic", "alpha")
	if err := chromedp.Run(ctx, chromedp.SendKeys(`#bd-filter-q`, "two"), chromedp.Sleep(300*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	v := probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#102"}) {
		t.Fatalf("setup: cards = %v", v.Cards)
	}
	if !has(v.Hash, "epic=alpha") || !has(v.Hash, "q=two") {
		t.Errorf("hash = %q, want the selection in it", v.Hash)
	}
	if !has(v.Stored, "epic=alpha") || !has(v.Stored, "q=two") {
		t.Errorf("stored = %q, want the selection persisted per viewer", v.Stored)
	}

	// Reload with NO hash: the viewer's last choice comes back.
	openSprintPage(t, ctx, base+"/sprint/")
	v = probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#102"}) {
		t.Fatalf("after reload: cards = %v, want the persisted #102", v.Cards)
	}
	if o := v.opt(t, "epic", "beta"); o.Checked {
		t.Errorf("after reload beta must stay unticked: %+v", o)
	}
	var q string
	if err := chromedp.Run(ctx, chromedp.Value(`#bd-filter-q`, &q)); err != nil {
		t.Fatal(err)
	}
	if q != "two" {
		t.Errorf("search box after reload = %q, want two", q)
	}
	if !has(v.Hash, "epic=alpha") {
		t.Errorf("a restored filter must be mirrored back into the hash so the view is linkable: %q", v.Hash)
	}

	// A link with its own filter wins over storage: the absent value travels as
	// the empty string (mgr=), and the "none" selection on its own key.
	openSprintPage(t, ctx, base+"/sprint/#mgr=&mgr=bea")
	v = probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#102", "#103"}) {
		t.Fatalf("hash mgr=,bea: cards = %v, want #102 #103", v.Cards)
	}
	if o := v.opt(t, "epic", "beta"); !o.Checked {
		t.Errorf("a link's filter replaces the stored one, it does not merge: %+v", o)
	}
	openSprintPage(t, ctx, base+"/sprint/#none=gate")
	v = probeFilter(t, ctx)
	if len(v.Cards) != 0 || !has(v.Empty, "gate") {
		t.Fatalf("hash none=gate: cards = %v empty = %q", v.Cards, v.Empty)
	}

	// Reset: defaults, hash cleared, storage cleared, and it survives a reload.
	if err := chromedp.Run(ctx, chromedp.Click(`#bd-filter-reset`), chromedp.Sleep(300*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	v = probeFilter(t, ctx)
	if len(v.Cards) != 4 || v.Stored != "" || has(v.Hash, "gate") || has(v.Hash, "epic") {
		t.Fatalf("after reset: cards=%v stored=%q hash=%q", v.Cards, v.Stored, v.Hash)
	}
	if v.ResetDisabled == nil || !*v.ResetDisabled {
		t.Errorf("reset must be disabled once nothing is narrowed")
	}
	openSprintPage(t, ctx, base+"/sprint/")
	v = probeFilter(t, ctx)
	if len(v.Cards) != 4 {
		t.Fatalf("after reset + reload: cards=%v", v.Cards)
	}
	// Existing hash keys keep working beside the filter's.
	openSprintPage(t, ctx, base+"/sprint/#all=1&epic=beta")
	v = probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#103", "#105"}) || !has(v.Hash, "all=1") {
		t.Fatalf("all=1&epic=beta: cards=%v hash=%q", v.Cards, v.Hash)
	}
	assertNoJSErrors(t, "persist", errs())
}

// The 15s refresh must not pull the search box out from under a typing reader:
// the bar's frame survives a re-render, only the facet groups are rebuilt.
func TestSprintFilterSurvivesRefreshWithFocus(t *testing.T) {
	stubFilterBoard(t)
	base, ctx, errs := domEnv(t, Options{})
	openSprintPage(t, ctx, base+"/sprint/")

	var focused string
	var q string
	if err := chromedp.Run(ctx,
		chromedp.Focus(`#bd-filter-q`),
		chromedp.SendKeys(`#bd-filter-q`, "alp"),
		chromedp.Evaluate(`load().then(() => new Promise((res) => setTimeout(res, 500)))`, nil, awaitPromise),
		chromedp.Evaluate(`document.activeElement ? document.activeElement.id : ''`, &focused),
		chromedp.Value(`#bd-filter-q`, &q),
	); err != nil {
		t.Fatal(err)
	}
	if focused != "bd-filter-q" || q != "alp" {
		t.Errorf("after refresh focus=%q value=%q, want the search box still focused with its text", focused, q)
	}
	v := probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#101", "#102"}) {
		t.Errorf("filter must still apply after a refresh: %v", v.Cards)
	}
	assertNoJSErrors(t, "refresh", errs())
}

// Phone width: the bar collapses into its disclosure by default and never
// widens the page; dark theme: it is styled from the theme variables, so its
// surface follows the page.
func TestSprintFilterPhoneAndDarkTheme(t *testing.T) {
	stubFilterBoard(t)
	base, ctx, errs := domEnv(t, Options{})

	var overflow, summaryShown string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		emulation.SetDeviceMetricsOverride(390, 844, 3, true),
		chromedp.Navigate(base+"/sprint/"),
		chromedp.Sleep(2*time.Second),
		chromedp.Evaluate(`String(document.documentElement.scrollWidth > window.innerWidth)`, &overflow),
		chromedp.Evaluate(`String(document.querySelector('#bd-filter > summary').getBoundingClientRect().height > 0)`, &summaryShown),
	); err != nil {
		t.Fatal(err)
	}
	v := probeFilter(t, ctx)
	if v.Open {
		t.Errorf("at phone width the facets must start collapsed into the disclosure")
	}
	if summaryShown != "true" {
		t.Errorf("the disclosure summary must be visible at phone width")
	}
	if overflow == "true" {
		t.Errorf("the filter bar must not widen the page past the phone viewport")
	}
	// Open it: the facet groups stack and still fit.
	var facetsShown string
	if err := chromedp.Run(ctx,
		chromedp.Click(`#bd-filter > summary`),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.Evaluate(`String(document.querySelector('#bd-filter .facets').getBoundingClientRect().height > 0)`, &facetsShown),
		chromedp.Evaluate(`String(document.documentElement.scrollWidth > window.innerWidth)`, &overflow),
	); err != nil {
		t.Fatal(err)
	}
	if facetsShown != "true" || overflow == "true" {
		t.Errorf("opened at phone width: facets shown=%s overflow=%s", facetsShown, overflow)
	}

	// Dark theme: the bar's background is the dark card, not a light one left
	// over from a hard-coded colour.
	var light, dark string
	if err := chromedp.Run(ctx,
		emulation.SetDeviceMetricsOverride(1280, 900, 1, false),
		chromedp.Evaluate(`document.documentElement.setAttribute('data-theme','light'); getComputedStyle(document.getElementById('bd-filter')).backgroundColor`, &light),
		chromedp.Evaluate(`document.documentElement.setAttribute('data-theme','dark'); getComputedStyle(document.getElementById('bd-filter')).backgroundColor`, &dark),
	); err != nil {
		t.Fatal(err)
	}
	if light == dark || light == "" || light == "rgba(0, 0, 0, 0)" {
		t.Errorf("the bar must take its surface from the theme: light=%q dark=%q", light, dark)
	}
	assertNoJSErrors(t, "phone/dark", errs())
}

// collisionBoard is the fixture for the codec: a sprint whose epic AND manager
// are literally "_", one with neither, and one whose manager needs escaping on
// the wire. The first codec wrote the absent value as "_", so #301 and #302
// were the same string in a link.
func collisionBoard() *board.Board {
	return &board.Board{
		SchemaVersion: board.SchemaVersion, Role: "steward", Scope: "machine-global",
		Title: "Bashy Steward Board", GeneratedAt: time.Now().UTC(),
		Sprints: []board.Sprint{
			{ID: 301, Title: "underscore", Epic: "_", Column: "doing", Manager: "_"},
			{ID: 302, Title: "nothing", Column: "doing"},
			{ID: 303, Title: "escaped", Epic: "alpha", Column: "doing", Manager: "ops & co=1"},
		},
	}
}

// A value literally named "_" is a value, not the absent one: the two are
// different checkboxes, different links, and each survives a reload as itself.
// Every value travels exactly as it is — the one with a space, an ampersand
// and an equals sign included — so a link made from the bar reads back to the
// selection it was made from.
func TestSprintFilterUnderscoreIsAValueNotTheAbsentOne(t *testing.T) {
	stubBoardWith(t, collisionBoard)
	base, ctx, errs := domEnv(t, Options{})
	openSprintPage(t, ctx, base+"/sprint/")

	v := probeFilter(t, ctx)
	if o := v.opt(t, "epic", "_"); o.Label != "_" || o.Count != 1 {
		t.Errorf("epic _ option = %+v, want its own box labelled _ with count 1", o)
	}
	if o := v.opt(t, "epic", ""); o.Label != "(no epic)" || o.Count != 1 {
		t.Errorf("(no epic) option = %+v", o)
	}

	// Only the underscore epic: the link says epic=_ and reads back as #301.
	clickOnly(t, ctx, "epic", "_")
	v = probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#301"}) || v.Hash != "#epic=_" || v.Stored != "epic=_" {
		t.Fatalf("only epic _: cards=%v hash=%q stored=%q, want #301 / #epic=_ / epic=_", v.Cards, v.Hash, v.Stored)
	}
	openSprintPage(t, ctx, base+"/sprint/")
	v = probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#301"}) {
		t.Fatalf("reload after only epic _: cards=%v, want #301 — not the no-epic sprint", v.Cards)
	}
	if o := v.opt(t, "epic", ""); o.Checked {
		t.Errorf("(no epic) must stay unticked after the reload: %+v", o)
	}

	// Only the absent epic: the link says epic= and reads back as #302.
	clickOnly(t, ctx, "epic", "")
	v = probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#302"}) || v.Hash != "#epic=" || v.Stored != "epic=" {
		t.Fatalf("only (no epic): cards=%v hash=%q stored=%q, want #302 / #epic= / epic=", v.Cards, v.Hash, v.Stored)
	}
	openSprintPage(t, ctx, base+"/sprint/")
	v = probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#302"}) {
		t.Fatalf("reload after only (no epic): cards=%v, want #302 — not the underscore sprint", v.Cards)
	}
	if o := v.opt(t, "epic", "_"); o.Checked {
		t.Errorf("epic _ must stay unticked after the reload: %+v", o)
	}

	// Links written by hand, each to its own sprint; both together to both.
	for _, c := range []struct {
		hash string
		want []string
	}{
		{"#epic=_", []string{"#301"}},
		{"#epic=", []string{"#302"}},
		{"#epic=&epic=_", []string{"#301", "#302"}},
		{"#mgr=_", []string{"#301"}},
		{"#mgr=", []string{"#302"}},
		{"#mgr=ops+%26+co%3D1", []string{"#303"}},
	} {
		openSprintPage(t, ctx, base+"/sprint/"+c.hash)
		v = probeFilter(t, ctx)
		if !eqStrings(v.Cards, c.want) {
			t.Errorf("link %s: cards=%v, want %v", c.hash, v.Cards, c.want)
		}
	}

	// A value that needs escaping round-trips through the bar's own link.
	clickOnly(t, ctx, "mgr", "ops & co=1")
	v = probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#303"}) || v.Hash != "#mgr=ops+%26+co%3D1" {
		t.Fatalf("only mgr 'ops & co=1': cards=%v hash=%q", v.Cards, v.Hash)
	}
	openSprintPage(t, ctx, base+"/sprint/"+v.Hash)
	v = probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#303"}) {
		t.Fatalf("the bar's own link read back: cards=%v, want #303", v.Cards)
	}
	if o := v.opt(t, "mgr", "ops & co=1"); !o.Checked {
		t.Errorf("the escaped manager must come back ticked: %+v", o)
	}
	assertNoJSErrors(t, "underscore", errs())
}

// "none" is the one selection with nothing to write, so it rides its own key:
// it is linkable, it survives a reload, "all" takes it out of the link and the
// store again, and a hand-written none= beside values is the values.
func TestSprintFilterNoneAllReloadRoundtrip(t *testing.T) {
	stubFilterBoard(t)
	base, ctx, errs := domEnv(t, Options{})
	openSprintPage(t, ctx, base+"/sprint/")

	clickFacetLink(t, ctx, "epic", "none")
	v := probeFilter(t, ctx)
	if len(v.Cards) != 0 || v.Hash != "#none=epic" || v.Stored != "none=epic" {
		t.Fatalf("epic none: cards=%v hash=%q stored=%q, want none / #none=epic / none=epic", v.Cards, v.Hash, v.Stored)
	}
	// Reload with no hash: still none, every epic box unticked, the empty line
	// names the facet to clear, and the link is mirrored back.
	openSprintPage(t, ctx, base+"/sprint/")
	v = probeFilter(t, ctx)
	if len(v.Cards) != 0 || v.Hash != "#none=epic" || !has(v.Empty, "epic") {
		t.Fatalf("reload after none: cards=%v hash=%q empty=%q", v.Cards, v.Hash, v.Empty)
	}
	for _, o := range v.Facets["epic"] {
		if o.Checked {
			t.Errorf("after none + reload every epic box must be unticked: %+v", o)
		}
	}
	// The absent value is not "none": ticking it alone is epic= and shows #104.
	clickBox(t, ctx, "epic", "")
	v = probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#104"}) || v.Hash != "#epic=" {
		t.Fatalf("none then tick (no epic): cards=%v hash=%q, want #104 / #epic=", v.Cards, v.Hash)
	}

	// Hand-written links: two facets at none; none= beside values is the values.
	openSprintPage(t, ctx, base+"/sprint/#none=epic&none=mgr")
	v = probeFilter(t, ctx)
	if len(v.Cards) != 0 || !has(v.Empty, "epic") || !has(v.Empty, "manager") {
		t.Fatalf("none=epic&none=mgr: cards=%v empty=%q", v.Cards, v.Empty)
	}
	openSprintPage(t, ctx, base+"/sprint/#none=mgr&mgr=ada")
	v = probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#101", "#104"}) {
		t.Fatalf("none=mgr&mgr=ada: cards=%v, want ada's #101 #104", v.Cards)
	}
	// The old sentinel is just a name now: no manager is called "_", so this
	// link shows nothing rather than the unclaimed sprint.
	openSprintPage(t, ctx, base+"/sprint/#mgr=_")
	v = probeFilter(t, ctx)
	if len(v.Cards) != 0 || !has(v.Empty, "manager") {
		t.Fatalf("mgr=_ with no such manager: cards=%v empty=%q, want an empty result naming manager", v.Cards, v.Empty)
	}

	// "all" on a none'd facet: the key leaves the link and the store.
	openSprintPage(t, ctx, base+"/sprint/#none=epic&q=one")
	clickFacetLink(t, ctx, "epic", "all")
	v = probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#101", "#103"}) || has(v.Hash, "none") || has(v.Hash, "epic") || v.Stored != "q=one" {
		t.Fatalf("epic all: cards=%v hash=%q stored=%q, want #101 #103 with no epic key anywhere", v.Cards, v.Hash, v.Stored)
	}
	openSprintPage(t, ctx, base+"/sprint/")
	v = probeFilter(t, ctx)
	if !eqStrings(v.Cards, []string{"#101", "#103"}) || has(v.Hash, "none") {
		t.Fatalf("reload after all: cards=%v hash=%q", v.Cards, v.Hash)
	}
	assertNoJSErrors(t, "none/all", errs())
}
