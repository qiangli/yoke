// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build verifydom

package webconsole

import (
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/qiangli/yoke/pkg/ladder"
)

// TestDutyLeaderboardSectionRendersRowsAndApproxMarker verifies that the
// duty ladder renders in the DOM, including inseparable rows marked with "≈".
func TestDutyLeaderboardSectionRendersRowsAndApproxMarker(t *testing.T) {
	stubBoard(t)
	seedLeaderboard(t, leaderboardRecords(), leaderboardMatrix())

	// Create overlapping events for two agents: with 2 deliveries each,
	// their rating intervals overlap and the second row gets "≈".
	var events []ladder.Event
	events = append(events, testDutyDeliveries("agent-a:m1", 1, 2, 1.0, 0)...)
	events = append(events, testDutyDeliveries("agent-b:m2", 1, 2, 1.0, 0)...)
	events = append(events, testDutyCertEvents("agent-a:m1", 1, ladder.CertL1, ladder.CertL2)...)
	events = append(events, testDutyCertEvents("agent-b:m2", 1, ladder.CertL1, ladder.CertL2)...)
	seedLadderEvents(t, events, nil)

	base, ctx, errs := domEnv(t, Options{})

	var rowCount, tableContent, classicRowCount string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.Sleep(2*time.Second),
		chromedp.Evaluate(`String(document.querySelector('#bd-leaderboard-duty table')?.querySelectorAll('tbody tr').length || 0)`, &rowCount),
		chromedp.Evaluate(`document.querySelector('#bd-leaderboard-duty table')?.textContent || ''`, &tableContent),
		chromedp.Evaluate(`String(document.querySelector('#bd-leaderboard table')?.querySelectorAll('tbody tr').length || 0)`, &classicRowCount),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "duty leaderboard section", errs())

	if rowCount == "0" || rowCount == "" {
		t.Fatalf("duty table has %s rows, want >= 2", rowCount)
	}
	if !strings.Contains(tableContent, "≈") {
		t.Errorf("duty table content missing approx marker '≈':\n%s", tableContent)
	}
	if classicRowCount != "2" {
		t.Errorf("classic leaderboard row count = %s, want 2 (must remain unchanged below)", classicRowCount)
	}
}

// TestDutyLeaderboardEmptyStoreShowsNoteAndClassicLeaderboard verifies that
// when no rated events exist, the duty section shows the empty-store note and
// the classic leaderboard below it renders unchanged.
func TestDutyLeaderboardEmptyStoreShowsNoteAndClassicLeaderboard(t *testing.T) {
	stubBoard(t)
	seedLeaderboard(t, leaderboardRecords(), leaderboardMatrix())
	seedLadderEvents(t, []ladder.Event{}, nil)

	base, ctx, errs := domEnv(t, Options{})

	var dutyEmptyNote, classicRowCount string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/sprint/"),
		chromedp.Sleep(2*time.Second),
		chromedp.Evaluate(`document.querySelector('#bd-leaderboard-duty p.empty')?.textContent || ''`, &dutyEmptyNote),
		chromedp.Evaluate(`String(document.querySelector('#bd-leaderboard table')?.querySelectorAll('tbody tr').length || 0)`, &classicRowCount),
	); err != nil {
		t.Fatalf("chromedp: %v", err)
	}
	assertNoJSErrors(t, "duty leaderboard empty section", errs())

	if !strings.Contains(dutyEmptyNote, "no rated events yet") {
		t.Errorf("empty store note = %q, want 'no rated events yet'", dutyEmptyNote)
	}
	if classicRowCount != "2" {
		t.Errorf("classic leaderboard row count = %s, want 2 rows rendered below", classicRowCount)
	}
}
