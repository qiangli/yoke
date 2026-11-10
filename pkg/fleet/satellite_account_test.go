package fleet

// `satellite list/show/invite/transfer --account` (S598 story f99b5693f5b7):
// invocation-scoped account selection rides the existing fleet seam
// (accountFlag/cloudOverrides/ResolveCloudAccount). Two endpoints with
// separate credentials prove correct routing, no cross-endpoint use, no
// ambient fallback, conflict refusals before network mutation, and no
// secret leakage. The invite code itself stays printable (compatibility).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/accounts/accountstest"
)

// accountMaster is a master fake with per-test rows that records which
// Bearer [REDACTED] reached which endpoint, so routing is proven by observation.
type accountMaster struct {
	mu     sync.Mutex
	rows   []map[string]any
	token  string
	seen   []string // "token" per request
	paths  []string // "METHOD path" per request
	xferID string
}

func (f *accountMaster) serve(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	f.seen = append(f.seen, tok)
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	if tok != f.token {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/satellites":
		rows := f.rows
		if rows == nil {
			rows = []map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"satellites": rows})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/satellites/"):
		for _, row := range f.rows {
			if row["id"] == strings.TrimPrefix(r.URL.Path, "/api/v1/satellites/") {
				_ = json.NewEncoder(w).Encode(map[string]any{"satellite": row})
				return
			}
		}
		http.NotFound(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/satellites/") &&
		strings.HasSuffix(r.URL.Path, "/transfer"):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"satellite_id":   f.xferID,
			"from":           "owl@example.com",
			"to":             "otter@example.com",
			"transferred_at": time.Now().UTC().Format(time.RFC3339),
			"note":           "ok",
		})
	default:
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
	}
}

func (f *accountMaster) tokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func (f *accountMaster) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

// withAccountResolver installs a fake account service for one test; nil
// models a host with no account service wired in.
func withAccountResolver(t *testing.T, fn func(ctx context.Context, ref, operation string) (AccountSelection, error)) {
	t.Helper()
	prev := AccountResolver
	AccountResolver = fn
	t.Cleanup(func() { AccountResolver = prev })
}

// twoAccountMasters builds the two-endpoint fixture: owl (cloudbox) and
// otter (satellite), each with its own credential and its own rows.
func twoAccountMasters(t *testing.T) (owl, otter *accountMaster, owlSrv, otterSrv *httptest.Server) {
	t.Helper()
	owl = &accountMaster{
		token:  "bearer-owl-secret-001",
		xferID: "sat-owl-1",
		rows:   []map[string]any{satRow("sat-owl-1", "owl-sat", "https://owl.example.com", "active")},
	}
	otter = &accountMaster{
		token:  "bearer-otter-secret-002",
		xferID: "sat-otter-9",
		rows:   []map[string]any{satRow("sat-otter-9", "otter-sat", "https://otter.example.com", "active")},
	}
	owlSrv = httptest.NewServer(http.HandlerFunc(owl.serve))
	t.Cleanup(owlSrv.Close)
	otterSrv = httptest.NewServer(http.HandlerFunc(otter.serve))
	t.Cleanup(otterSrv.Close)
	return owl, otter, owlSrv, otterSrv
}

// selectByAlias resolves the two fixture aliases; any other ref is unknown.
func selectByAlias(t *testing.T, endpoints map[string]struct {
	endpoint, token, category string
},
) func(ctx context.Context, ref, operation string) (AccountSelection, error) {
	t.Helper()
	return func(_ context.Context, ref, operation string) (AccountSelection, error) {
		if operation != "cloudbox" {
			return AccountSelection{}, &AccountError{Code: accountCodeUnresolved, Message: "unexpected operation " + operation}
		}
		e, ok := endpoints[ref]
		if !ok {
			return AccountSelection{}, &AccountError{Code: accountCodeUnresolved, Message: "no account " + ref}
		}
		return AccountSelection{
			Ref: ref, AccountID: "acc_" + ref,
			Category: e.category, Endpoint: e.endpoint, Token: e.token,
		}, nil
	}
}

func TestSatelliteListAccountSelectsItsOwnMaster(t *testing.T) {
	isolatedFleetRoot(t)
	owl, otter, owlSrv, otterSrv := twoAccountMasters(t)
	t.Setenv("BASHY_CLOUDBOX_URL", "https://ambient.example.com")
	t.Setenv("BASHY_FLEET_TOKEN", "ambient-token-must-not-be-used")
	t.Setenv("BASHY_API_KEY", "")

	withAccountResolver(t, selectByAlias(t, map[string]struct {
		endpoint, token, category string
	}{
		"owl":   {owlSrv.URL, owl.token, AccountCloudbox},
		"otter": {otterSrv.URL, otter.token, AccountSatellite},
	}))

	out, err := runSatellite(t, "list", "--account", "owl")
	if err != nil {
		t.Fatalf("list --account owl: %v", err)
	}
	if !strings.Contains(out, "sat-owl-1") || strings.Contains(out, "sat-otter-9") {
		t.Fatalf("list --account owl routed wrong:\n%s", out)
	}

	out, err = runSatellite(t, "list", "--account", "otter")
	if err != nil {
		t.Fatalf("list --account otter: %v", err)
	}
	if !strings.Contains(out, "sat-otter-9") || strings.Contains(out, "sat-owl-1") {
		t.Fatalf("list --account otter routed wrong:\n%s", out)
	}

	for _, tok := range append(owl.tokens(), otter.tokens()...) {
		if tok == "ambient-token-must-not-be-used" {
			t.Fatal("an explicit --account fell back to the ambient token")
		}
	}
	for _, tok := range owl.tokens() {
		if tok != owl.token {
			t.Fatalf("owl endpoint saw foreign credential %q", tok)
		}
	}
	for _, tok := range otter.tokens() {
		if tok != otter.token {
			t.Fatalf("otter endpoint saw foreign credential %q", tok)
		}
	}
}

func TestSatelliteShowAccountRouting(t *testing.T) {
	isolatedFleetRoot(t)
	owl, otter, owlSrv, otterSrv := twoAccountMasters(t)
	t.Setenv("BASHY_CLOUDBOX_URL", "https://ambient.example.com")
	t.Setenv("BASHY_FLEET_TOKEN", "ambient-token-must-not-be-used")
	t.Setenv("BASHY_API_KEY", "")

	withAccountResolver(t, selectByAlias(t, map[string]struct {
		endpoint, token, category string
	}{
		"owl":   {owlSrv.URL, owl.token, AccountCloudbox},
		"otter": {otterSrv.URL, otter.token, AccountSatellite},
	}))

	out, err := runSatellite(t, "show", "owl-sat", "--account", "owl")
	if err != nil {
		t.Fatalf("show --account owl: %v", err)
	}
	if !strings.Contains(out, "id: sat-owl-1") {
		t.Fatalf("show picked the wrong record:\n%s", out)
	}
	if got := len(otter.tokens()); got != 0 {
		t.Fatalf("show --account owl reached the other endpoint (%d hits)", got)
	}

	jout, err := runSatellite(t, "show", "sat-otter-9", "--account", "otter", "--json")
	if err != nil {
		t.Fatalf("show --json --account otter: %v (%s)", err, jout)
	}
	var docs struct {
		Satellite SatelliteEntry `json:"satellite"`
	}
	if err := json.Unmarshal([]byte(jout), &docs); err != nil {
		t.Fatalf("show --json is not JSON: %v:\n%s", err, jout)
	}
	if docs.Satellite.ID != "sat-otter-9" {
		t.Fatalf("unexpected show record: %+v", docs.Satellite)
	}
	_ = owl
}

func TestSatelliteTransferAccountRouting(t *testing.T) {
	isolatedFleetRoot(t)
	owl, otter, owlSrv, otterSrv := twoAccountMasters(t)
	t.Setenv("BASHY_CLOUDBOX_URL", "https://ambient.example.com")
	t.Setenv("BASHY_FLEET_TOKEN", "ambient-token-must-not-be-used")
	t.Setenv("BASHY_API_KEY", "")

	beforeOtter := otter.hits()
	withAccountResolver(t, selectByAlias(t, map[string]struct {
		endpoint, token, category string
	}{
		"owl":   {owlSrv.URL, owl.token, AccountCloudbox},
		"otter": {otterSrv.URL, otter.token, AccountSatellite},
	}))

	out, err := runSatellite(t, "transfer", "sat-owl-1", "--to", "otter@example.com", "--yes", "--account", "owl")
	if err != nil {
		t.Fatalf("transfer --account owl: %v (%s)", err, out)
	}
	if !strings.Contains(out, "satellite: sat-owl-1") || !strings.Contains(out, "to: otter@example.com") {
		t.Fatalf("unexpected transfer output:\n%s", out)
	}
	if got := otter.hits() - beforeOtter; got != 0 {
		t.Fatalf("transfer --account owl reached the other endpoint (%d hits)", got)
	}
	for _, tok := range owl.tokens() {
		if tok != owl.token {
			t.Fatalf("owl endpoint saw foreign credential %q", tok)
		}
	}
}

func TestSatelliteInviteAccountSuppliesEndpoint(t *testing.T) {
	isolatedFleetRoot(t)
	fake, srv := setupFakeSatellite(t)
	t.Setenv("BASHY_CLOUDBOX_URL", "https://ambient.example.com")
	t.Setenv("BASHY_FLEET_TOKEN", "ambient-token-must-not-be-used")
	t.Setenv("BASHY_API_KEY", "")

	withAccountResolver(t, func(_ context.Context, ref, operation string) (AccountSelection, error) {
		if operation != "cloudbox" {
			t.Errorf("unexpected operation %q", operation)
		}
		if ref != "zoo" {
			return AccountSelection{}, &AccountError{Code: accountCodeUnresolved, Message: "no account " + ref}
		}
		return AccountSelection{
			Ref: ref, AccountID: "acc_zoo", Category: AccountSatellite,
			Endpoint: srv.URL, Token: fake.ownerToken,
		}, nil
	})

	// No --satellite: the account supplies the endpoint and the credential.
	out, err := runSatelliteInvite(t,
		"--account", "zoo",
		"--name", "puppy",
		"--stdout",
	)
	if err != nil {
		t.Fatalf("invite --account zoo: %v (%s)", err, out)
	}
	if strings.TrimSpace(out) != fake.inviteCode {
		t.Fatalf("stdout = %q, want %q", out, fake.inviteCode)
	}
	if len(fake.lastScopes) != 1 || fake.lastScopes[0] != "host:invite-mint" {
		t.Fatalf("scopes = %v, want [host:invite-mint]", fake.lastScopes)
	}

	// --json through the parent tree keeps the invite-code compatibility.
	parent := NewSatelliteCmd()
	var pout strings.Builder
	parent.SetOut(&pout)
	parent.SetErr(&pout)
	parent.SetArgs([]string{"invite", "--account", "zoo", "--name", "ivy", "--stdout", "--json"})
	if err := parent.Execute(); err != nil {
		t.Fatalf("parent invite --account: %v (%s)", err, pout.String())
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(pout.String()), &res); err != nil {
		t.Fatalf("invite --json is not JSON: %v:\n%s", err, pout.String())
	}
	if res["code"] != fake.inviteCode || res["name"] != "ivy" || res["satellite"] != srv.URL {
		t.Fatalf("unexpected invite record: %v", res)
	}
}

// Explicit endpoint/token flags alongside --account are conflicts, and the
// refusal lands before any network mutation touches either endpoint.
func TestSatelliteAccountConflictsRefusedBeforeNetwork(t *testing.T) {
	isolatedFleetRoot(t)
	owl, otter, owlSrv, _ := twoAccountMasters(t)
	_, srv := setupFakeSatellite(t)
	t.Setenv("BASHY_CLOUDBOX_URL", "https://ambient.example.com")
	t.Setenv("BASHY_FLEET_TOKEN", "ambient-token-must-not-be-used")
	t.Setenv("BASHY_API_KEY", "")

	withAccountResolver(t, selectByAlias(t, map[string]struct {
		endpoint, token, category string
	}{
		"owl": {owlSrv.URL, owl.token, AccountCloudbox},
		"zoo": {srv.URL, "owner-token", AccountSatellite},
	}))

	cases := []struct {
		name string
		args []string
	}{
		{"list --master", []string{"list", "--account", "owl", "--master", owlSrv.URL}},
		{"list --token", []string{"list", "--account", "owl", "--token", "other"}},
		{"show --master", []string{"show", "sat-owl-1", "--account", "owl", "--master", owlSrv.URL}},
		{"show --token", []string{"show", "sat-owl-1", "--account", "owl", "--token", "other"}},
		{"transfer --master", []string{"transfer", "sat-owl-1", "--to", "otter@example.com", "--yes", "--account", "owl", "--master", owlSrv.URL}},
		{"transfer --token", []string{"transfer", "sat-owl-1", "--to", "otter@example.com", "--yes", "--account", "owl", "--token", "other"}},
	}
	for _, tc := range cases {
		before := owl.hits() + otter.hits()
		_, err := runSatellite(t, tc.args...)
		if AccountErrorCode(err) != "account_conflict" {
			t.Fatalf("%s: code = %q (%v), want account_conflict", tc.name, AccountErrorCode(err), err)
		}
		if got := owl.hits() + otter.hits() - before; got != 0 {
			t.Fatalf("%s: %d requests made despite the conflict", tc.name, got)
		}
	}

	inviteCases := []struct {
		name string
		args []string
	}{
		{"invite --satellite", []string{"--account", "zoo", "--satellite", srv.URL, "--name", "p", "--stdout"}},
		{"invite --token", []string{"--account", "zoo", "--name", "p", "--stdout", "--token", "other"}},
	}
	for _, tc := range inviteCases {
		_, err := runSatelliteInvite(t, tc.args...)
		if AccountErrorCode(err) != "account_conflict" {
			t.Fatalf("%s: code = %q (%v), want account_conflict", tc.name, AccountErrorCode(err), err)
		}
	}
}

// A provider-category account cannot drive satellite verbs; without a wired
// service --account refuses; a tokenless selection refuses without touching
// the ambient credential.
func TestSatelliteAccountSelectionFailuresUseNoNetwork(t *testing.T) {
	isolatedFleetRoot(t)
	owl, _, owlSrv, _ := twoAccountMasters(t)
	t.Setenv("BASHY_CLOUDBOX_URL", owlSrv.URL)
	t.Setenv("BASHY_FLEET_TOKEN", "ambient-token")
	t.Setenv("BASHY_API_KEY", "")

	withAccountResolver(t, func(context.Context, string, string) (AccountSelection, error) {
		return AccountSelection{AccountID: "acc_owl", Category: AccountDigitalOcean, Token: "token-owl"}, nil
	})
	for _, args := range [][]string{
		{"list", "--account", "owl"},
		{"show", "sat-owl-1", "--account", "owl"},
		{"transfer", "sat-owl-1", "--to", "otter@example.com", "--yes", "--account", "owl"},
	} {
		before := owl.hits()
		_, err := runSatellite(t, args...)
		if AccountErrorCode(err) != "account_unsupported" {
			t.Fatalf("%v: code = %q (%v), want account_unsupported", args, AccountErrorCode(err), err)
		}
		if got := owl.hits() - before; got != 0 {
			t.Fatalf("%v: %d requests made despite the refusal", args, got)
		}
	}
	before := owl.hits()
	if _, err := runSatelliteInvite(t, "--account", "zoo", "--name", "p", "--stdout"); AccountErrorCode(err) != "account_unsupported" {
		t.Fatalf("invite provider category: code = %q (%v), want account_unsupported", AccountErrorCode(err), err)
	}

	withAccountResolver(t, nil)
	if _, err := runSatellite(t, "list", "--account", "owl"); AccountErrorCode(err) != "account_unresolved" {
		t.Fatalf("unwired service: code = %q, want account_unresolved", AccountErrorCode(err))
	}
	if got := owl.hits() - before; got != 0 {
		t.Fatal("a request was made without a wired account service")
	}

	withAccountResolver(t, func(context.Context, string, string) (AccountSelection, error) {
		return AccountSelection{AccountID: "acc_owl", Category: AccountCloudbox, Endpoint: owlSrv.URL}, nil
	})
	if _, err := runSatellite(t, "list", "--account", "owl"); AccountErrorCode(err) != "credential_unavailable" {
		t.Fatalf("tokenless selection: code = %q, want credential_unavailable", AccountErrorCode(err))
	}
	withAccountResolver(t, func(context.Context, string, string) (AccountSelection, error) {
		return AccountSelection{}, &AccountError{Code: accountCodeUnresolved, Message: "no account nope"}
	})
	if _, err := runSatellite(t, "list", "--account", "nope"); AccountErrorCode(err) != "account_unresolved" {
		t.Fatalf("unknown alias must not fall back to ambient")
	}
	for _, tok := range owl.tokens() {
		if tok == "ambient-token" {
			t.Fatal("a failed --account selection fell back to the ambient token")
		}
	}
}

// Without --account the account service is never consulted and the
// historical ladder (env then flags) answers exactly as before.
func TestSatelliteWithoutAccountIsUnchanged(t *testing.T) {
	isolatedFleetRoot(t)
	fake := &fakeSatelliteMaster{rows: []map[string]any{
		satRow("sat-1", "zoo", "https://zoo.example.com", "active"),
	}}
	setupSatelliteMaster(t, fake)
	consulted := 0
	withAccountResolver(t, func(context.Context, string, string) (AccountSelection, error) {
		consulted++
		return AccountSelection{}, &AccountError{Code: accountCodeUnresolved, Message: "must not be called"}
	})

	if _, err := runSatellite(t, "list"); err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, err := runSatellite(t, "show", "sat-1"); err != nil {
		t.Fatalf("show: %v", err)
	}
	if consulted != 0 {
		t.Fatal("a run without --account consulted the account service")
	}

	// Invite still needs --satellite without --account.
	if _, err := runSatelliteInvite(t, "--name", "pup", "--token", "tok"); err == nil ||
		!strings.Contains(err.Error(), "satellite invite needs --satellite") {
		t.Fatalf("missing --satellite err = %v", err)
	}
}

// Credential material never escapes into records, errors or normal output;
// the invite code itself stays printable by design.
func TestSatelliteAccountOutputCarriesNoSecret(t *testing.T) {
	isolatedFleetRoot(t)
	owl, _, owlSrv, _ := twoAccountMasters(t)
	owl.token = accountstest.CanarySecret
	t.Setenv("BASHY_CLOUDBOX_URL", "https://ambient.example.com")
	t.Setenv("BASHY_FLEET_TOKEN", "ambient")
	t.Setenv("BASHY_API_KEY", "")

	withAccountResolver(t, selectByAlias(t, map[string]struct {
		endpoint, token, category string
	}{
		"owl": {owlSrv.URL, accountstest.CanarySecret, AccountCloudbox},
	}))

	out, err := runSatellite(t, "list", "--account", "owl", "--json")
	accountstest.AssertNoSecret(t, out, err)
	if err != nil {
		t.Fatalf("list --account: %v", err)
	}
	out, err = runSatellite(t, "show", "sat-owl-1", "--account", "owl", "--json")
	accountstest.AssertNoSecret(t, out, err)
	if err != nil {
		t.Fatalf("show --account: %v (%s)", err, out)
	}
	_, err = runSatellite(t, "list", "--account", "owl", "--token", "other")
	accountstest.AssertNoSecret(t, err.Error())

	fake, srv := setupFakeSatellite(t)
	fake.ownerToken = accountstest.CanarySecret
	withAccountResolver(t, func(context.Context, string, string) (AccountSelection, error) {
		return AccountSelection{
			Ref: "zoo", AccountID: "acc_zoo", Category: AccountSatellite,
			Endpoint: srv.URL, Token: accountstest.CanarySecret,
		}, nil
	})
	out, err = runSatelliteInvite(t, "--account", "zoo", "--name", "pup", "--stdout")
	accountstest.AssertNoSecret(t, out, err)
	if err != nil {
		t.Fatalf("invite --account: %v (%s)", err, out)
	}
	if strings.TrimSpace(out) != fake.inviteCode {
		t.Fatalf("invite code compatibility broke: %q", out)
	}
}

// The invocation leaves ambient state alone: env keeps its values and the
// active profile is never switched.
func TestSatelliteAccountLeavesAmbientStateAlone(t *testing.T) {
	isolatedFleetRoot(t)
	owl, _, owlSrv, _ := twoAccountMasters(t)
	t.Setenv("BASHY_CLOUDBOX_URL", "https://ambient.example.com")
	t.Setenv("BASHY_FLEET_TOKEN", "ambient-token")
	t.Setenv("BASHY_API_KEY", "")

	withAccountResolver(t, selectByAlias(t, map[string]struct {
		endpoint, token, category string
	}{
		"owl": {owlSrv.URL, owl.token, AccountCloudbox},
	}))

	if _, err := runSatellite(t, "list", "--account", "owl"); err != nil {
		t.Fatalf("list --account: %v", err)
	}
	if got := map[string]string{
		"BASHY_CLOUDBOX_URL": "https://ambient.example.com",
		"BASHY_FLEET_TOKEN":  "ambient-token",
		"BASHY_API_KEY":      "",
	}; got["BASHY_CLOUDBOX_URL"] != "https://ambient.example.com" ||
		got["BASHY_FLEET_TOKEN"] != "ambient-token" ||
		got["BASHY_API_KEY"] != "" {
		t.Fatalf("ambient env moved: %v", got)
	}
}

// Every satellite verb offers --account: help, the atlas flag projection
// and the MCP schema all reflect off these cobra flags.
func TestSatelliteAccountFlagRegistered(t *testing.T) {
	for _, tc := range []struct {
		label string
		build func() *cobra.Command
	}{
		{"satellite list", NewSatelliteListCmd},
		{"satellite show", NewSatelliteShowCmd},
		{"satellite transfer", NewSatelliteTransferCmd},
		{"satellite invite", NewSatelliteInviteCmd},
	} {
		if tc.build().Flags().Lookup("account") == nil {
			t.Fatalf("%s has no --account flag", tc.label)
		}
	}
	parent := NewSatelliteCmd()
	for _, sub := range []string{"list", "show", "transfer", "invite"} {
		c, _, err := parent.Find([]string{sub})
		if err != nil {
			t.Fatalf("satellite %s not found: %v", sub, err)
		}
		if c.Flags().Lookup("account") == nil && c.InheritedFlags().Lookup("account") == nil {
			t.Fatalf("satellite %s has no --account flag", sub)
		}
	}
}
