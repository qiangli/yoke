// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/accounts"
	"github.com/qiangli/yoke/pkg/accounts/accountstest"
)

// trapAdapter fails the test immediately if ANY adapter method is called.
// This proves that GET routes (list, detail, resources, usage) read store
// and snapshots ONLY and NEVER call adapters.
type trapAdapter struct {
	t *testing.T
}

func (a *trapAdapter) Name() string { return "digitalocean" }
func (a *trapAdapter) Capabilities() accounts.Capability {
	return accounts.Discover | accounts.Verify | accounts.Resources | accounts.Usage | accounts.Billing | accounts.Select
}
func (a *trapAdapter) Discover(context.Context) ([]accounts.Observation, error) {
	a.t.Fatal("adapter Discover called during cached read")
	return nil, nil
}
func (a *trapAdapter) Verify(context.Context, accounts.Account) (accounts.Observation, error) {
	a.t.Fatal("adapter Verify called during cached read")
	return accounts.Observation{}, nil
}
func (a *trapAdapter) Resources(context.Context, accounts.Account) ([]accounts.ResourceRef, error) {
	a.t.Fatal("adapter Resources called during cached read")
	return nil, nil
}
func (a *trapAdapter) Usage(context.Context, accounts.Account, string) ([]accounts.Metric, error) {
	a.t.Fatal("adapter Usage called during cached read")
	return nil, nil
}
func (a *trapAdapter) Billing(context.Context, accounts.Account, string) ([]accounts.Metric, error) {
	a.t.Fatal("adapter Billing called during cached read")
	return nil, nil
}
func (a *trapAdapter) Select(context.Context, accounts.Account, accounts.Operation) (accounts.Selection, error) {
	a.t.Fatal("adapter Select called during cached read")
	return accounts.Selection{}, nil
}

func setupTestAccountsStore(t *testing.T) (*accounts.Store, *accounts.Registry) {
	t.Helper()
	dir := t.TempDir()
	store := accounts.Open(filepath.Join(dir, "accounts.json"))
	reg := accounts.NewRegistry()

	prevStore := accountsStoreFn
	prevReg := accountsRegistryFn
	accountsStoreFn = func() *accounts.Store { return store }
	accountsRegistryFn = func() *accounts.Registry { return reg }
	t.Cleanup(func() {
		accountsStoreFn = prevStore
		accountsRegistryFn = prevReg
	})

	return store, reg
}

func seedSampleAccounts(t *testing.T, store *accounts.Store) (string, string) {
	t.Helper()
	now := time.Now().UTC()

	// Account 1: DigitalOcean with snapshot, quota, spend, resources
	err := store.Observe("digitalocean", []accounts.Observation{
		{
			Account: accounts.Account{
				Category:          accounts.DigitalOcean,
				Provider:          "digitalocean",
				ProviderAccountID: "do_12345",
				Name:              "Production Team",
				Username:          "prod-lead",
				Email:             "prod@example.com",
				ProfileURL:        "https://cloud.digitalocean.com/settings",
				DashboardURL:      "https://cloud.digitalocean.com/projects",
				Team:              "platform",
				Role:              "owner",
				Status:            accounts.Resolved,
				Connections: []accounts.Connection{
					{
						ID:            "conn_do1",
						CredentialRef: "secret:do-token",
						Scopes:        []string{"read", "write"},
						AuthMethod:    "bearer",
					},
				},
				Resources: []accounts.ResourceRef{
					{ID: "drop_101", Kind: "droplet", Name: "app-worker-1", Source: "do"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("seed DO account: %v", err)
	}

	accs, _ := store.List()
	var doAcc accounts.Account
	for _, a := range accs {
		if a.ProviderAccountID == "do_12345" {
			doAcc = a
			break
		}
	}
	if err := store.PatchOverride(doAcc.ID, map[string]string{"alias": "do-prod"}, nil); err != nil {
		t.Fatalf("patch alias: %v", err)
	}
	doAcc, _ = store.Get(doAcc.ID)

	spendVal := 42.50
	quotaVal := 25.0
	windowEnd := now.Add(7 * 24 * time.Hour)
	err = store.SaveSnapshot(accounts.Snapshot{
		AccountID: doAcc.ID,
		Resources: []accounts.ResourceRef{
			{ID: "drop_101", Kind: "droplet", Name: "app-worker-1", Source: "do"},
			{ID: "drop_102", Kind: "droplet", Name: "app-worker-2", Source: "do"},
		},
		Metrics: map[string][]accounts.Metric{
			"month": {
				{
					Name:           "cost",
					Value:          &spendVal,
					Unit:           "usd",
					Currency:       "USD",
					Source:         "digitalocean/billing",
					Classification: accounts.ProviderReported,
					Coverage:       accounts.Complete,
					ObservedAt:     now,
				},
				{
					Name:           "droplet_limit",
					Value:          &quotaVal,
					Unit:           "droplets",
					Source:         "digitalocean/account",
					Classification: accounts.ProviderReported,
					Coverage:       accounts.Partial,
					ObservedAt:     now,
					WindowEnd:      &windowEnd,
				},
			},
			"week": {
				{
					Name:           "cost",
					Value:          &spendVal,
					Unit:           "usd",
					Currency:       "USD",
					Source:         "digitalocean/billing",
					Classification: accounts.ProviderReported,
					Coverage:       accounts.Complete,
					ObservedAt:     now,
				},
			},
		},
		ObservedAt: now,
	})
	if err != nil {
		t.Fatalf("save DO snapshot: %v", err)
	}

	// Account 2: AWS with nil metrics (unavailable metrics test)
	awsAcc, err := store.Add(accounts.Account{
		Alias:             "aws-dev",
		Category:          accounts.AWS,
		Provider:          "aws",
		ProviderAccountID: "112233445566",
		Name:              "Development Profile",
		Team:              "infra",
		Role:              "engineer",
		Status:            accounts.Unresolved,
		ProfileURL:        "https://console.aws.amazon.com/",
		Connections: []accounts.Connection{
			{
				ID:            "conn_aws1",
				Profile:       "dev-profile",
				CredentialRef: "profile:dev",
			},
		},
	})
	if err != nil {
		t.Fatalf("seed AWS account: %v", err)
	}

	err = store.SaveSnapshot(accounts.Snapshot{
		AccountID: awsAcc.ID,
		Metrics: map[string][]accounts.Metric{
			"month": {
				{
					Name:           "quota",
					Value:          nil, // nil value!
					Unit:           "instances",
					Source:         "aws/limits",
					Classification: accounts.Estimate,
					Coverage:       accounts.Unknown,
					ObservedAt:     now,
				},
			},
		},
		ObservedAt: now.Add(-48 * time.Hour), // stale!
		LastError:  "refresh_failed",
	})
	if err != nil {
		t.Fatalf("save AWS snapshot: %v", err)
	}

	return doAcc.ID, awsAcc.ID
}

// TestAccountsGetNeverCallsAdapter verifies list/detail/resources/usage read the
// store and snapshots only. If any adapter is called, trapAdapter t.Fatal triggers.
func TestAccountsGetNeverCallsAdapter(t *testing.T) {
	store, reg := setupTestAccountsStore(t)
	if err := reg.Register(&trapAdapter{t: t}); err != nil {
		t.Fatalf("register trap adapter: %v", err)
	}

	doID, awsID := seedSampleAccounts(t, store)
	h := newTestHandler(t, Options{})

	// 1. List
	rec := do(h, "GET", "/api/accounts", "127.0.0.1:5555", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/accounts = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	accountstest.AssertNoSecret(t, body)

	var list accountsListView
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Total != 2 || len(list.Accounts) != 2 {
		t.Fatalf("list count = %d, want 2", list.Total)
	}

	// Verify DO summary row
	var doSummary AccountSummary
	for _, a := range list.Accounts {
		if a.ID == doID {
			doSummary = a
			break
		}
	}
	if doSummary.Alias != "do-prod" || doSummary.Provider != "digitalocean" {
		t.Fatalf("unexpected DO summary: %+v", doSummary)
	}
	if doSummary.Spend != "$42.50" {
		t.Errorf("spend = %q, want $42.50", doSummary.Spend)
	}
	if !strings.Contains(doSummary.Quota, "25") {
		t.Errorf("quota = %q, want contains 25", doSummary.Quota)
	}
	if doSummary.Initials == "" {
		t.Errorf("initials empty for DO summary")
	}

	// 2. Detail
	rec = do(h, "GET", "/api/accounts/"+doID, "127.0.0.1:5555", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail = %d, want 200", rec.Code)
	}
	accountstest.AssertNoSecret(t, rec.Body.String())

	var detail accountDetailView
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.Account.ID != doID {
		t.Fatalf("detail account id = %q, want %q", detail.Account.ID, doID)
	}
	if detail.SafeLinks.DashboardURL != "https://cloud.digitalocean.com/projects" {
		t.Errorf("safe dashboard url = %q", detail.SafeLinks.DashboardURL)
	}
	if len(detail.CLIExamples) == 0 {
		t.Errorf("missing CLI examples in detail")
	}
	// Check credential references only
	for _, c := range detail.Account.Connections {
		if c.CredentialRef != "secret:do-token" {
			t.Errorf("unexpected credential ref: %q", c.CredentialRef)
		}
	}

	// 3. Resources
	rec = do(h, "GET", "/api/accounts/"+doID+"/resources", "127.0.0.1:5555", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET resources = %d, want 200", rec.Code)
	}
	accountstest.AssertNoSecret(t, rec.Body.String())

	var resData struct {
		Resources []accounts.ResourceRef `json:"resources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resData); err != nil {
		t.Fatalf("decode resources: %v", err)
	}
	if len(resData.Resources) != 2 {
		t.Fatalf("resources count = %d, want 2", len(resData.Resources))
	}

	// 4. Usage
	rec = do(h, "GET", "/api/accounts/"+doID+"/usage?period=month", "127.0.0.1:5555", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET usage = %d, want 200", rec.Code)
	}
	accountstest.AssertNoSecret(t, rec.Body.String())

	var usageData struct {
		Metrics []accounts.Metric `json:"metrics"`
		Period  string            `json:"period"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &usageData); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	if len(usageData.Metrics) != 2 || usageData.Period != "month" {
		t.Fatalf("metrics = %+v, period = %q", usageData.Metrics, usageData.Period)
	}
	for _, m := range usageData.Metrics {
		if m.Classification == "" || m.Coverage == "" {
			t.Errorf("metric classification/coverage missing: %+v", m)
		}
	}

	_ = awsID
}

// TestAccountsUnavailableMetricsRenderAsUnavailableNeverZero asserts that
// unavailable metrics render as "unavailable" and NEVER as 0 or $0.
func TestAccountsUnavailableMetricsRenderAsUnavailableNeverZero(t *testing.T) {
	store, _ := setupTestAccountsStore(t)
	_, awsID := seedSampleAccounts(t, store)
	h := newTestHandler(t, Options{})

	// List
	rec := do(h, "GET", "/api/accounts", "127.0.0.1:5555", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET list = %d", rec.Code)
	}
	var list accountsListView
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}

	var awsSummary AccountSummary
	for _, a := range list.Accounts {
		if a.ID == awsID {
			awsSummary = a
			break
		}
	}
	if awsSummary.Quota != "unavailable" {
		t.Errorf("aws quota = %q, want unavailable (never 0)", awsSummary.Quota)
	}
	if awsSummary.Spend != "unavailable" {
		t.Errorf("aws spend = %q, want unavailable (never 0)", awsSummary.Spend)
	}
	if !awsSummary.IsStale {
		t.Errorf("aws account should be marked stale")
	}

	// Usage
	rec = do(h, "GET", "/api/accounts/"+awsID+"/usage?period=month", "127.0.0.1:5555", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET usage = %d", rec.Code)
	}
	var usageData struct {
		Metrics []accounts.Metric `json:"metrics"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &usageData); err != nil {
		t.Fatal(err)
	}
	if len(usageData.Metrics) != 1 {
		t.Fatalf("metrics count = %d, want 1", len(usageData.Metrics))
	}
	if usageData.Metrics[0].Value != nil {
		t.Errorf("expected nil metric value, got %v", usageData.Metrics[0].Value)
	}
}

// mockRefreshAdapter records refresh calls and simulates successful verification.
type mockRefreshAdapter struct {
	verifyCount int
	resCount    int
	usageCount  int
	billCount   int
}

func (m *mockRefreshAdapter) Name() string { return "digitalocean" }
func (m *mockRefreshAdapter) Capabilities() accounts.Capability {
	return accounts.Verify | accounts.Resources | accounts.Usage | accounts.Billing
}
func (m *mockRefreshAdapter) Discover(context.Context) ([]accounts.Observation, error) {
	return nil, nil
}
func (m *mockRefreshAdapter) Verify(_ context.Context, a accounts.Account) (accounts.Observation, error) {
	m.verifyCount++
	obs := a
	obs.Alias = ""
	obs.Status = accounts.Resolved
	return accounts.Observation{Account: obs, SourceKey: a.ProviderAccountID}, nil
}
func (m *mockRefreshAdapter) Resources(_ context.Context, _ accounts.Account) ([]accounts.ResourceRef, error) {
	m.resCount++
	return []accounts.ResourceRef{{ID: "res_new", Kind: "droplet", Name: "refreshed-node"}}, nil
}
func (m *mockRefreshAdapter) Usage(_ context.Context, _ accounts.Account, period string) ([]accounts.Metric, error) {
	m.usageCount++
	val := 99.0
	return []accounts.Metric{{
		Name:           "tokens",
		Value:          &val,
		Unit:           "tokens",
		Source:         "mock",
		Classification: accounts.ProviderReported,
		Coverage:       accounts.Complete,
		Period:         period,
	}}, nil
}
func (m *mockRefreshAdapter) Billing(_ context.Context, _ accounts.Account, period string) ([]accounts.Metric, error) {
	m.billCount++
	val := 55.5
	return []accounts.Metric{{
		Name:           "cost",
		Value:          &val,
		Unit:           "usd",
		Currency:       "USD",
		Source:         "mock",
		Classification: accounts.ProviderReported,
		Coverage:       accounts.Complete,
		Period:         period,
	}}, nil
}
func (m *mockRefreshAdapter) Select(context.Context, accounts.Account, accounts.Operation) (accounts.Selection, error) {
	return accounts.Selection{}, nil
}

// TestAccountsRefreshProtectedAndBounded tests auth/CSRF protection and bounded refresh execution.
func TestAccountsRefreshProtectedAndBounded(t *testing.T) {
	store, reg := setupTestAccountsStore(t)
	adapter := &mockRefreshAdapter{}
	if err := reg.Register(adapter); err != nil {
		t.Fatalf("register mock adapter: %v", err)
	}

	doID, _ := seedSampleAccounts(t, store)
	h := newTestHandler(t, Options{})

	// 1. Cross-Origin Origin header rejected with 403
	body := `{"id":"` + doID + `"}`
	hdr := http.Header{
		"Content-Type": {"application/json"},
		"Origin":       {"http://evil.example.com"},
	}
	rec := do(h, "POST", "/api/accounts/refresh", "127.0.0.1:5555", hdr)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin refresh = %d, want 403 Forbidden", rec.Code)
	}

	// 2. Cross-site Sec-Fetch-Site rejected with 403
	hdr = http.Header{
		"Content-Type":   {"application/json"},
		"Sec-Fetch-Site": {"cross-site"},
	}
	rec = do(h, "POST", "/api/accounts/refresh", "127.0.0.1:5555", hdr)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site Sec-Fetch-Site refresh = %d, want 403 Forbidden", rec.Code)
	}

	// 3. Remote peer address without auth rejected with 403
	hdr = http.Header{
		"Content-Type": {"application/json"},
	}
	rec = do(h, "POST", "/api/accounts/refresh", "192.168.1.100:5555", hdr)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote unauthenticated refresh = %d, want 403 Forbidden", rec.Code)
	}

	// 4. Same-origin loopback request accepted and runs bounded refresh
	r := httptest.NewRequest("POST", "/api/accounts/refresh", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("valid refresh = %d (%s), want 200", w.Code, w.Body.String())
	}
	accountstest.AssertNoSecret(t, w.Body.String())

	if adapter.verifyCount != 1 {
		t.Errorf("adapter.Verify count = %d, want 1", adapter.verifyCount)
	}
	if adapter.resCount != 1 {
		t.Errorf("adapter.Resources count = %d, want 1", adapter.resCount)
	}

	// Check updated snapshot in store
	snap, err := store.Snapshot(doID)
	if err != nil {
		t.Fatalf("get snapshot after refresh: %v", err)
	}
	if len(snap.Resources) != 1 || snap.Resources[0].ID != "res_new" {
		t.Errorf("snapshot resources not updated: %+v", snap.Resources)
	}
}

// TestAccountsPageAndRedirect tests GET /accounts/ HTML page and /accounts redirect.
func TestAccountsPageAndRedirect(t *testing.T) {
	setupTestAccountsStore(t)
	h := newTestHandler(t, Options{})

	// Redirect
	rec := do(h, "GET", "/accounts", "127.0.0.1:5555", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /accounts = %d, want 302", rec.Code)
	}

	// HTML page
	rec = do(h, "GET", "/accounts/", "127.0.0.1:5555", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /accounts/ = %d, want 200", rec.Code)
	}
	page := rec.Body.String()
	if !strings.Contains(page, "accounts.js") {
		t.Errorf("page missing accounts.js reference")
	}
	if !strings.Contains(page, "Accounts — bashy") {
		t.Errorf("page missing title")
	}
}

// TestAccountsFiltering tests query parameter filters on GET /api/accounts.
func TestAccountsFiltering(t *testing.T) {
	store, _ := setupTestAccountsStore(t)
	seedSampleAccounts(t, store)
	h := newTestHandler(t, Options{})

	// Filter by provider
	rec := do(h, "GET", "/api/accounts?provider=digitalocean", "127.0.0.1:5555", nil)
	var list accountsListView
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if list.Total != 1 || list.Accounts[0].Provider != "digitalocean" {
		t.Fatalf("filter provider failed: %+v", list)
	}

	// Filter by team
	rec = do(h, "GET", "/api/accounts?team=infra", "127.0.0.1:5555", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if list.Total != 1 || list.Accounts[0].Team != "infra" {
		t.Fatalf("filter team failed: %+v", list)
	}

	// Search q
	rec = do(h, "GET", "/api/accounts?q=dev-profile", "127.0.0.1:5555", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if list.Total != 1 || list.Accounts[0].Alias != "aws-dev" {
		t.Fatalf("search q failed: %+v", list)
	}
}

// TestAccountsSafeLinksValidation verifies safe URL schemes (http/https only)
// and rejecting javascript: or data: URIs.
func TestAccountsSafeLinksValidation(t *testing.T) {
	store, _ := setupTestAccountsStore(t)
	acc, err := store.Add(accounts.Account{
		Alias:        "unsafe-test",
		Category:     accounts.Other,
		Provider:     "custom",
		ProfileURL:   "javascript:alert(1)",
		DashboardURL: "data:text/html,evil",
		Status:       accounts.Resolved,
	})
	if err != nil {
		t.Fatal(err)
	}

	h := newTestHandler(t, Options{})
	rec := do(h, "GET", "/api/accounts/"+acc.ID, "127.0.0.1:5555", nil)
	var detail accountDetailView
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)

	if detail.SafeLinks.ProfileURL != "" {
		t.Errorf("unsafe profile URL was not rejected: %q", detail.SafeLinks.ProfileURL)
	}
	if detail.SafeLinks.DashboardURL != "" {
		t.Errorf("unsafe dashboard URL was not rejected: %q", detail.SafeLinks.DashboardURL)
	}
}

// TestAccountsSecretSentinelNeverEscapes verifies the canary secret is never returned.
func TestAccountsSecretSentinelNeverEscapes(t *testing.T) {
	store, _ := setupTestAccountsStore(t)
	doID, _ := seedSampleAccounts(t, store)

	h := newTestHandler(t, Options{})

	for _, path := range []string{
		"/accounts/",
		"/api/accounts",
		"/api/accounts/" + doID,
		"/api/accounts/" + doID + "/resources",
		"/api/accounts/" + doID + "/usage?period=month",
	} {
		rec := do(h, "GET", path, "127.0.0.1:5555", nil)
		accountstest.AssertNoSecret(t, rec.Body.String())
	}
}
