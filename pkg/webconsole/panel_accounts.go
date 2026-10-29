// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/accounts"
	"github.com/qiangli/yoke/pkg/coopauth"
)

const accountsSchemaVersion = "bashy-console-accounts-v1"

// Seams for hermetic tests.
var (
	accountsStoreFn    func() *accounts.Store
	accountsRegistryFn func() *accounts.Registry
)

func (s *server) accountsStore() *accounts.Store {
	if accountsStoreFn != nil {
		return accountsStoreFn()
	}
	return accounts.Open("")
}

func (s *server) accountsRegistry() *accounts.Registry {
	if accountsRegistryFn != nil {
		return accountsRegistryFn()
	}
	return accounts.DefaultRegistry()
}

func (s *server) wireAccountsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /accounts/", s.handleAccountsPage)
	mux.Handle("GET /accounts", redirectTo("/accounts/"))
	mux.HandleFunc("GET /api/accounts", s.handleAccountsList)
	mux.HandleFunc("GET /api/accounts/{id}", s.handleAccountDetail)
	mux.HandleFunc("GET /api/accounts/{id}/resources", s.handleAccountResources)
	mux.HandleFunc("GET /api/accounts/resources", s.handleAccountResources)
	mux.HandleFunc("GET /api/accounts/{id}/usage", s.handleAccountUsage)
	mux.HandleFunc("GET /api/accounts/usage", s.handleAccountUsage)
	mux.HandleFunc("POST /api/accounts/refresh", s.handleAccountRefresh)
	mux.HandleFunc("POST /api/accounts/{id}/refresh", s.handleAccountRefresh)
}

func (s *server) handleAccountsPage(w http.ResponseWriter, r *http.Request) {
	s.servePageFile(w, r, "accounts.html")
}

// AccountSummary is one row on the Accounts page. Unavailable metrics render
// as "unavailable", never 0.
type AccountSummary struct {
	ID                string            `json:"id"`
	Alias             string            `json:"alias"`
	Category          accounts.Category `json:"category"`
	Provider          string            `json:"provider"`
	ProviderAccountID string            `json:"provider_account_id,omitempty"`
	Name              string            `json:"name,omitempty"`
	Username          string            `json:"username,omitempty"`
	AvatarURL         string            `json:"avatar_url,omitempty"`
	Initials          string            `json:"initials"`
	Team              string            `json:"team,omitempty"`
	Profile           string            `json:"profile,omitempty"`
	Status            accounts.Status   `json:"status"`
	Quota             string            `json:"quota"`
	Reset             string            `json:"reset,omitempty"`
	Spend             string            `json:"spend"`
	Currency          string            `json:"currency,omitempty"`
	FreshnessAge      string            `json:"freshness_age"`
	ObservedAt        *time.Time        `json:"observed_at,omitempty"`
	LastError         string            `json:"last_error,omitempty"`
	IsStale           bool              `json:"is_stale,omitempty"`
}

type accountsListView struct {
	SchemaVersion string           `json:"schema_version"`
	Principal     string           `json:"principal,omitempty"`
	Accounts      []AccountSummary `json:"accounts"`
	Total         int              `json:"total"`
}

// handleAccountsList serves GET /api/accounts: summary rows reading store +
// snapshots ONLY. Never calls an adapter.
func (s *server) handleAccountsList(w http.ResponseWriter, r *http.Request) {
	store := s.accountsStore()
	accs, err := store.List()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to list accounts: " + err.Error()})
		return
	}

	filterProvider := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("provider")))
	filterCategory := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("category")))
	filterTeam := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("team")))
	filterStatus := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	searchQuery := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))

	principal, _ := PrincipalFromRequest(r)

	rows := make([]AccountSummary, 0, len(accs))
	for _, a := range accs {
		if filterProvider != "" && strings.ToLower(a.Provider) != filterProvider {
			continue
		}
		if filterCategory != "" && strings.ToLower(string(a.Category)) != filterCategory {
			continue
		}
		if filterTeam != "" && strings.ToLower(a.Team) != filterTeam {
			continue
		}
		if filterStatus != "" && strings.ToLower(string(a.Status)) != filterStatus {
			continue
		}
		if searchQuery != "" {
			var connParts []string
			for _, c := range a.Connections {
				connParts = append(connParts, c.Profile, c.Endpoint, c.CredentialRef)
			}
			combined := strings.ToLower(a.Alias + " " + a.Name + " " + a.Provider + " " + a.Team + " " + a.Username + " " + a.Email + " " + a.Role + " " + a.ID + " " + strings.Join(connParts, " "))
			if !strings.Contains(combined, searchQuery) {
				continue
			}
		}

		var snapPtr *accounts.Snapshot
		if snap, err := store.Snapshot(a.ID); err == nil {
			snapPtr = &snap
		}
		rows = append(rows, accountSummary(a, snapPtr))
	}

	writeJSON(w, http.StatusOK, accountsListView{
		SchemaVersion: accountsSchemaVersion,
		Principal:     principal,
		Accounts:      rows,
		Total:         len(rows),
	})
}

type accountDetailView struct {
	SchemaVersion string             `json:"schema_version"`
	Principal     string             `json:"principal,omitempty"`
	Account       accounts.Account   `json:"account"`
	Summary       AccountSummary     `json:"summary"`
	Snapshot      *accounts.Snapshot `json:"snapshot,omitempty"`
	SafeLinks     struct {
		ProfileURL   string `json:"profile_url,omitempty"`
		DashboardURL string `json:"dashboard_url,omitempty"`
	} `json:"safe_links"`
	CLIExamples []string `json:"cli_examples"`
}

// handleAccountDetail serves GET /api/accounts/{id}: full detail by id reading
// store + snapshots ONLY. Safe links rel=noopener with http(s) scheme check.
func (s *server) handleAccountDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = r.URL.Query().Get("id")
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "account id required"})
		return
	}

	store := s.accountsStore()
	a, err := store.Get(id)
	if err != nil {
		if errors.Is(err, accounts.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	var snapPtr *accounts.Snapshot
	if snap, err := store.Snapshot(a.ID); err == nil {
		snapPtr = &snap
	}

	principal, _ := PrincipalFromRequest(r)
	summary := accountSummary(a, snapPtr)

	v := accountDetailView{
		SchemaVersion: accountsSchemaVersion,
		Principal:     principal,
		Account:       a,
		Summary:       summary,
		Snapshot:      snapPtr,
		CLIExamples: []string{
			"bashy account show " + a.ID,
			"bashy account usage " + a.ID,
			"bashy account refresh " + a.ID,
		},
	}

	if isSafeURL(a.ProfileURL) {
		v.SafeLinks.ProfileURL = a.ProfileURL
	}
	if isSafeURL(a.DashboardURL) {
		v.SafeLinks.DashboardURL = a.DashboardURL
	}

	writeJSON(w, http.StatusOK, v)
}

// handleAccountResources serves GET /api/accounts/{id}/resources (and query ?id=).
// Reads store + snapshots ONLY. Never calls an adapter.
func (s *server) handleAccountResources(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = r.URL.Query().Get("id")
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "account id required"})
		return
	}

	store := s.accountsStore()
	a, err := store.Get(id)
	if err != nil {
		if errors.Is(err, accounts.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	snap, err := store.Snapshot(a.ID)
	var res []accounts.ResourceRef
	var obsAt *time.Time
	var lastErr string
	if err == nil {
		res = snap.Resources
		obsAt = &snap.ObservedAt
		lastErr = snap.LastError
	} else {
		res = a.Resources
		obsAt = a.ObservedAt
		lastErr = a.LastError
	}
	if res == nil {
		res = []accounts.ResourceRef{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": "bashy-console-account-resources-v1",
		"account_id":     a.ID,
		"resources":      res,
		"observed_at":    obsAt,
		"last_error":     lastErr,
	})
}

// handleAccountUsage serves GET /api/accounts/{id}/usage?period=day|week|month.
// Reads snapshot metrics ONLY. Never calls an adapter. Unavailable metrics
// render as "unavailable", never 0.
func (s *server) handleAccountUsage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = r.URL.Query().Get("id")
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "account id required"})
		return
	}

	period := r.URL.Query().Get("period")
	if period == "" {
		period = "month"
	}
	if period != "day" && period != "week" && period != "month" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "period must be day, week, or month"})
		return
	}

	store := s.accountsStore()
	a, err := store.Get(id)
	if err != nil {
		if errors.Is(err, accounts.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	snap, err := store.Snapshot(a.ID)
	var metrics []accounts.Metric
	var obsAt *time.Time
	var lastErr string
	if err == nil {
		if snap.Metrics != nil {
			metrics = snap.Metrics[period]
		}
		obsAt = &snap.ObservedAt
		lastErr = snap.LastError
	}
	if metrics == nil {
		metrics = []accounts.Metric{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": "bashy-console-account-usage-v1",
		"account_id":     a.ID,
		"period":         period,
		"metrics":        metrics,
		"observed_at":    obsAt,
		"last_error":     lastErr,
	})
}

// handleAccountRefresh serves POST /api/accounts/refresh (and POST /api/accounts/{id}/refresh).
// Protected exactly like other state-changing panel POSTs (auth + CSRF/origin check).
func (s *server) handleAccountRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAccountsRefresh(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "refresh refused: authentication or CSRF check failed"})
		return
	}

	var body struct {
		ID  string `json:"id"`
		All bool   `json:"all"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body)
	}
	if body.ID == "" {
		body.ID = r.PathValue("id")
	}
	if body.ID == "" {
		body.ID = r.URL.Query().Get("id")
	}
	if !body.All && r.URL.Query().Get("all") == "true" {
		body.All = true
	}
	if body.ID == "" && !body.All {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "specify account id or all: true"})
		return
	}

	store := s.accountsStore()
	reg := s.accountsRegistry()

	if body.All {
		refreshed, err := accounts.RefreshAll(r.Context(), store, reg)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "refresh failed: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"schema_version": "bashy-console-account-refresh-v1",
			"refreshed":      refreshed,
			"total":          len(refreshed),
		})
		return
	}

	a, err := store.Get(body.ID)
	if err != nil {
		if errors.Is(err, accounts.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	item, err := accounts.RefreshOne(r.Context(), store, reg, a)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "refresh failed: " + err.Error()})
		return
	}
	var snapPtr *accounts.Snapshot
	if snap, err := store.Snapshot(item.ID); err == nil {
		snapPtr = &snap
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": "bashy-console-account-refresh-v1",
		"account":        item,
		"snapshot":       snapPtr,
	})
}

// authorizeAccountsRefresh enforces authentication and CSRF/origin checks.
func (s *server) authorizeAccountsRefresh(r *http.Request) bool {
	if !isSameOrigin(r) {
		return false
	}
	if coopauth.ArrivedViaCloud(r) {
		return true
	}
	if s.sessions != nil {
		if c, err := r.Cookie(sessionCookie); err == nil {
			if subject, ok := s.sessions.Validate(c.Value); ok {
				if _, deviceID, isDevice := splitDeviceSubject(subject); isDevice {
					if s.pairing != nil {
						if st, err := s.pairing.load(); err == nil {
							if d, live := st.findDevice(deviceID, time.Now()); live && d.Peer == nil {
								return s.scopeAllows(d.Scope, "/accounts/")
							}
						}
					}
					return false
				}
				return true
			}
		}
	}
	if !s.requireLogin && coopauth.IsLoopbackAddr(r.RemoteAddr) {
		return true
	}
	return false
}

// isSameOrigin verifies the Origin or Referer header against the server's external base.
func isSameOrigin(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		ref := strings.TrimSpace(r.Header.Get("Referer"))
		if ref == "" {
			return true
		}
		u, err := url.Parse(ref)
		if err != nil || u.Host == "" {
			return false
		}
		origin = u.Scheme + "://" + u.Host
	}

	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}

	ext := coopauth.ExternalBase(r)
	if strings.EqualFold(origin, ext) {
		return true
	}

	if coopauth.IsLoopbackAddr(r.RemoteAddr) && (isLoopbackHost(u.Hostname()) || strings.EqualFold(u.Host, r.Host)) {
		return true
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}

	for _, h := range strings.Split(r.Header.Get(coopauth.HdrForwardedHost), ",") {
		if h = strings.TrimSpace(h); h != "" && strings.EqualFold(u.Host, h) {
			return true
		}
	}
	return false
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func isSafeURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")
}

func computeInitials(a accounts.Account) string {
	candidates := []string{a.Alias, a.Name, a.Username, a.Provider}
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		words := strings.Fields(c)
		if len(words) >= 2 {
			r1 := []rune(words[0])[0]
			r2 := []rune(words[1])[0]
			return strings.ToUpper(string([]rune{r1, r2}))
		}
		runes := []rune(c)
		if len(runes) >= 2 {
			return strings.ToUpper(string(runes[:2]))
		}
		return strings.ToUpper(string(runes[:1]))
	}
	return "AC"
}

func accountSummary(a accounts.Account, snapPtr *accounts.Snapshot) AccountSummary {
	profile := a.Role
	if len(a.Connections) > 0 && a.Connections[0].Profile != "" {
		profile = a.Connections[0].Profile
	}

	quota := "unavailable"
	reset := "unavailable"
	spend := "unavailable"
	currency := ""
	freshness := "never"
	var obsAt *time.Time
	var lastErr string
	isStale := false

	if a.ObservedAt != nil {
		obsAt = a.ObservedAt
		freshness = formatAge(*a.ObservedAt)
	}
	if a.LastError != "" {
		lastErr = a.LastError
	}

	if snapPtr != nil {
		if !snapPtr.ObservedAt.IsZero() {
			obsAt = &snapPtr.ObservedAt
			freshness = formatAge(snapPtr.ObservedAt)
			if time.Since(snapPtr.ObservedAt) > 24*time.Hour {
				isStale = true
			}
		}
		if snapPtr.LastError != "" {
			lastErr = snapPtr.LastError
			isStale = true
		}

		q, r := extractQuota(snapPtr)
		if q != "" {
			quota = q
		}
		if r != "" {
			reset = r
		}
		s, c := extractSpend(snapPtr)
		if s != "" {
			spend = s
			currency = c
		}
	} else if a.Status == accounts.Unresolved || a.Status == accounts.Error {
		isStale = true
	}

	var avatar string
	if isSafeURL(a.AvatarURL) {
		avatar = a.AvatarURL
	}

	return AccountSummary{
		ID:                a.ID,
		Alias:             a.Alias,
		Category:          a.Category,
		Provider:          a.Provider,
		ProviderAccountID: a.ProviderAccountID,
		Name:              a.Name,
		Username:          a.Username,
		AvatarURL:         avatar,
		Initials:          computeInitials(a),
		Team:              a.Team,
		Profile:           profile,
		Status:            a.Status,
		Quota:             quota,
		Reset:             reset,
		Spend:             spend,
		Currency:          currency,
		FreshnessAge:      freshness,
		ObservedAt:        obsAt,
		LastError:         lastErr,
		IsStale:           isStale,
	}
}

func extractSpend(snap *accounts.Snapshot) (spend, currency string) {
	if snap == nil || snap.Metrics == nil {
		return "unavailable", ""
	}
	for _, period := range []string{"month", "week", "day"} {
		for _, m := range snap.Metrics[period] {
			name := strings.ToLower(m.Name)
			unit := strings.ToLower(m.Unit)
			isSpendMetric := m.Currency != "" || unit == "usd" || unit == "currency" ||
				strings.Contains(name, "spend") || strings.Contains(name, "cost") ||
				strings.Contains(name, "balance") || strings.Contains(name, "billing")
			if isSpendMetric {
				if m.Value == nil {
					return "unavailable", m.Currency
				}
				cur := m.Currency
				if cur == "" && unit == "usd" {
					cur = "USD"
				}
				if cur == "USD" || cur == "" {
					return fmt.Sprintf("$%.2f", *m.Value), cur
				}
				return fmt.Sprintf("%.2f %s", *m.Value, cur), cur
			}
		}
	}
	return "unavailable", ""
}

func extractQuota(snap *accounts.Snapshot) (quota, reset string) {
	if snap == nil || snap.Metrics == nil {
		return "unavailable", "unavailable"
	}
	for _, period := range []string{"month", "week", "day"} {
		for _, m := range snap.Metrics[period] {
			name := strings.ToLower(m.Name)
			isQuotaMetric := strings.Contains(name, "limit") || strings.Contains(name, "quota")
			if isQuotaMetric {
				r := "unavailable"
				if m.WindowEnd != nil && !m.WindowEnd.IsZero() {
					r = m.WindowEnd.UTC().Format("Jan 02")
				}
				if m.Value == nil {
					return "unavailable", r
				}
				unit := m.Unit
				if unit == "" || unit == "count" {
					return fmt.Sprintf("%.0f", *m.Value), r
				}
				return fmt.Sprintf("%.0f %s", *m.Value, unit), r
			}
		}
	}
	return "unavailable", "unavailable"
}

func formatAge(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	if d < 0 || d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	days := int(d.Hours() / 24)
	return fmt.Sprintf("%dd ago", days)
}

func formatReset(t time.Time) string {
	if t.IsZero() {
		return "unavailable"
	}
	return t.UTC().Format(time.RFC3339)
}
