// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build verifydom

package webconsole

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestDOMAccountsSummaryAndDetail tests rendering of summary rows and expanding detail pane.
func TestDOMAccountsSummaryAndDetail(t *testing.T) {
	store, _ := setupTestAccountsStore(t)
	doID, _ := seedSampleAccounts(t, store)

	base, ctx, errs := domEnv(t, Options{})

	var alias, provider, spend, quota, rowCount string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/accounts/"),
		chromedp.WaitVisible(`#acc-table`, chromedp.ByQuery),
		chromedp.Sleep(500*time.Millisecond),
		chromedp.Evaluate(`String(document.querySelectorAll("#acc-tbody tr.acc-row").length)`, &rowCount),
		chromedp.Evaluate(`document.querySelector("#acc-tbody tr.acc-row .acc-alias")?.textContent || ''`, &alias),
		chromedp.Evaluate(`document.querySelector("#acc-tbody tr.acc-row .acc-provider")?.textContent || ''`, &provider),
		chromedp.Evaluate(`document.querySelector("#acc-tbody tr.acc-row .acc-spend")?.textContent || ''`, &spend),
		chromedp.Evaluate(`document.querySelector("#acc-tbody tr.acc-row .acc-quota")?.textContent || ''`, &quota),
	); err != nil {
		t.Fatalf("chromedp summary: %v", err)
	}
	assertNoJSErrors(t, "accounts summary", errs())

	if rowCount != "2" {
		t.Errorf("rendered row count = %s, want 2", rowCount)
	}
	if alias == "" {
		t.Errorf("alias is empty")
	}

	// Expand detail pane for DO account
	var detailID, cliExample, relVal, classification, coverage string
	if err := chromedp.Run(ctx,
		chromedp.Click(`tr[data-id="`+doID+`"] button.acc-expand-btn`, chromedp.ByQuery),
		chromedp.WaitVisible(`#acc-detail-`+doID, chromedp.ByQuery),
		chromedp.Sleep(500*time.Millisecond),
		chromedp.Evaluate(`document.querySelector("#acc-detail-`+doID+` .acc-detail-id")?.textContent || ''`, &detailID),
		chromedp.Evaluate(`document.querySelector("#acc-detail-`+doID+` .acc-cli-example")?.textContent || ''`, &cliExample),
		chromedp.Evaluate(`document.querySelector("#acc-detail-`+doID+` a.acc-safe-link")?.getAttribute("rel") || ''`, &relVal),
		chromedp.Evaluate(`document.querySelector("#acc-detail-`+doID+` .acc-metric-classification")?.textContent || ''`, &classification),
		chromedp.Evaluate(`document.querySelector("#acc-detail-`+doID+` .acc-metric-coverage")?.textContent || ''`, &coverage),
	); err != nil {
		t.Fatalf("chromedp detail: %v", err)
	}
	assertNoJSErrors(t, "accounts detail", errs())

	if detailID != doID {
		t.Errorf("detail ID = %q, want %q", detailID, doID)
	}
	if !strings.Contains(cliExample, "bashy account show") {
		t.Errorf("cliExample = %q, want bashy account show", cliExample)
	}
	if relVal != "noopener" {
		t.Errorf("safe link rel = %q, want noopener", relVal)
	}
	if classification == "" || coverage == "" {
		t.Errorf("usage metric classification/coverage not visible: class=%q cov=%q", classification, coverage)
	}
}

// TestDOMAccountsFilter tests filtering by search text, provider, and empty search results.
func TestDOMAccountsFilter(t *testing.T) {
	store, _ := setupTestAccountsStore(t)
	seedSampleAccounts(t, store)

	base, ctx, errs := domEnv(t, Options{})

	var countBefore, countAfterFilter, countEmpty string
	var emptyDisplay string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/accounts/"),
		chromedp.WaitVisible(`#acc-table`, chromedp.ByQuery),
		chromedp.Sleep(500*time.Millisecond),
		chromedp.Evaluate(`String(document.querySelectorAll("#acc-tbody tr.acc-row").length)`, &countBefore),

		// Filter by provider DigitalOcean
		chromedp.SetValue(`#acc-filter-provider`, "digitalocean", chromedp.ByQuery),
		chromedp.Sleep(200*time.Millisecond),
		chromedp.Evaluate(`String(document.querySelectorAll("#acc-tbody tr.acc-row").length)`, &countAfterFilter),

		// Filter by non-existent query
		chromedp.SetValue(`#acc-search`, "nonexistentquery123", chromedp.ByQuery),
		chromedp.Sleep(200*time.Millisecond),
		chromedp.Evaluate(`String(document.querySelectorAll("#acc-tbody tr.acc-row").length)`, &countEmpty),
		chromedp.Evaluate(`getComputedStyle(document.getElementById("acc-empty")).display`, &emptyDisplay),
	); err != nil {
		t.Fatalf("chromedp filter: %v", err)
	}
	assertNoJSErrors(t, "accounts filter", errs())

	if countBefore != "2" {
		t.Errorf("countBefore = %s, want 2", countBefore)
	}
	if countAfterFilter != "1" {
		t.Errorf("countAfterFilter = %s, want 1", countAfterFilter)
	}
	if countEmpty != "0" {
		t.Errorf("countEmpty = %s, want 0", countEmpty)
	}
	if emptyDisplay == "none" {
		t.Errorf("empty box should be visible when no search results match")
	}
}

// TestDOMAccountsEmptyState tests rendering when no accounts exist in store.
func TestDOMAccountsEmptyState(t *testing.T) {
	setupTestAccountsStore(t) // empty store
	base, ctx, errs := domEnv(t, Options{})

	var emptyDisplay, tableDisplay string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/accounts/"),
		chromedp.WaitVisible(`#acc-empty`, chromedp.ByQuery),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.Evaluate(`getComputedStyle(document.getElementById("acc-empty")).display`, &emptyDisplay),
		chromedp.Evaluate(`getComputedStyle(document.getElementById("acc-table")).display`, &tableDisplay),
	); err != nil {
		t.Fatalf("chromedp empty state: %v", err)
	}
	assertNoJSErrors(t, "accounts empty state", errs())

	if emptyDisplay == "none" {
		t.Errorf("empty box is not displayed")
	}
	if tableDisplay != "none" {
		t.Errorf("table should be hidden in empty state")
	}
}

// TestDOMAccountsLoadingState tests the loading spinner while fetching.
func TestDOMAccountsLoadingState(t *testing.T) {
	setupTestAccountsStore(t)
	base, ctx, _ := domEnvWrapped(t, Options{}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/accounts" {
				time.Sleep(1 * time.Second)
			}
			next.ServeHTTP(w, r)
		})
	})

	var busy string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/accounts/"),
		chromedp.WaitVisible(`.acc-spinner`, chromedp.ByQuery),
		chromedp.Evaluate(`document.getElementById("acc-page")?.getAttribute("aria-busy") || ''`, &busy),
	); err != nil {
		t.Fatalf("chromedp loading state: %v", err)
	}
	if busy != "true" {
		t.Errorf("expected aria-busy=true during loading, got %q", busy)
	}
}

// TestDOMAccountsErrorState tests alert display when API returns an error.
func TestDOMAccountsErrorState(t *testing.T) {
	setupTestAccountsStore(t)
	base, ctx, _ := domEnvWrapped(t, Options{}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/accounts" {
				http.Error(w, "internal server failure", http.StatusInternalServerError)
				return
			}
			next.ServeHTTP(w, r)
		})
	})

	var errDisplay, errText string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/accounts/"),
		chromedp.WaitVisible(`#acc-error`, chromedp.ByQuery),
		chromedp.Evaluate(`getComputedStyle(document.getElementById("acc-error")).display`, &errDisplay),
		chromedp.Evaluate(`document.getElementById("acc-error")?.textContent || ''`, &errText),
	); err != nil {
		t.Fatalf("chromedp error state: %v", err)
	}
	if errDisplay == "none" {
		t.Errorf("error box should be visible")
	}
	if !strings.Contains(errText, "Could not load accounts") {
		t.Errorf("errorText = %q", errText)
	}
}

// TestDOMAccountsStaleState tests notice display when an account is stale.
func TestDOMAccountsStaleState(t *testing.T) {
	store, _ := setupTestAccountsStore(t)
	seedSampleAccounts(t, store) // AWS account is seeded as stale with LastError: "refresh_failed"
	base, ctx, errs := domEnv(t, Options{})

	var staleDisplay, staleText string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/accounts/"),
		chromedp.WaitVisible(`#acc-stale`, chromedp.ByQuery),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.Evaluate(`getComputedStyle(document.getElementById("acc-stale")).display`, &staleDisplay),
		chromedp.Evaluate(`document.getElementById("acc-stale")?.textContent || ''`, &staleText),
	); err != nil {
		t.Fatalf("chromedp stale state: %v", err)
	}
	assertNoJSErrors(t, "accounts stale state", errs())

	if staleDisplay == "none" {
		t.Errorf("stale notice should be visible")
	}
	if !strings.Contains(staleText, "stale") {
		t.Errorf("staleText = %q", staleText)
	}
}
