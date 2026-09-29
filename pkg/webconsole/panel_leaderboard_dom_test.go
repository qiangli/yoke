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

// The Leaderboard section on /sprint/ renders the ranked tier as a real table
// in the DOM — the byte-level API tests cannot see whether board.js actually
// drew it, and a section that throws renders nothing while every byte test
// stays green (the blind spot this file exists for).
func TestLeaderboardSectionRendersRankedRows(t *testing.T) {
	stubBoard(t)
	seedLeaderboard(t, leaderboardRecords(), leaderboardMatrix())
	base, ctx, errs := domEnv(t, Options{})

	var summary, firstAgent, firstRepeat, rowCount string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.Sleep(2*time.Second),
		chromedp.Evaluate(`document.querySelector('#bd-leaderboard .meta')?.textContent || ''`, &summary),
		chromedp.Evaluate(`String(document.querySelector('#bd-leaderboard table')?.querySelectorAll('tbody tr').length)`, &rowCount),
		chromedp.Evaluate(`document.querySelector('#bd-leaderboard table tbody tr td')?.textContent || ''`, &firstAgent),
		chromedp.Evaluate(`document.querySelector('#bd-leaderboard table tbody tr')?.lastChild?.textContent || ''`, &firstRepeat),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "leaderboard section", errs())

	// The one-line summary states the evidence base before any number is read.
	if !strings.Contains(summary, "ledger records") || !strings.Contains(summary, "ranked at n>=") {
		t.Errorf("leaderboard summary = %q, want the records/agents/threshold line", summary)
	}
	// The ranked tier is the FIRST table in the section, in Compute's order:
	// beta:two (7/7) outranks alpha:one (5/6) by Wilson lower bound.
	if rowCount != "2" {
		t.Errorf("ranked table has %s rows, want 2", rowCount)
	}
	if firstAgent != "beta:two" {
		t.Errorf("first ranked agent = %q, want beta:two — the page must keep Compute's order", firstAgent)
	}
	// No events-mode record exists, so repeat renders as the em dash.
	if firstRepeat != "—" {
		t.Errorf("first ranked repeat cell = %q, want the em dash", firstRepeat)
	}
}
