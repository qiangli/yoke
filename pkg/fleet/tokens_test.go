package fleet

// Red tests for the scoped-credential surface (S490 story 2607):
//
//   - TokenClient create/list/revoke against an httptest fake of the
//     cloudbox wire contract POST/GET /api/v1/tokens, DELETE /api/v1/tokens/:id.
//   - The attenuation rule: subset mint ok, any scope outside the caller
//     token's scopes refused with 403, any *:admin scope refused.
//   - NewTokenCmd: the minted value goes to the vault via the secrets
//     client and NEVER appears on stdout/stderr; --stdout is the only
//     way to print it.
//   - master-token mint: mints coord-<host>, writes the master-token file
//     (0600, base label), never prints the value, idempotent re-run.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- contract fake -------------------------------------------------------

type fakeTokenRow struct {
	id      string
	name    string
	owner   string
	scopes  []string
	expires time.Time
	value   string
}

// fakeCloudbox is an httptest server implementing exactly the wire contract:
// POST /api/v1/tokens, GET /api/v1/tokens, DELETE /api/v1/tokens/:id, plus
// the secrets vault surface POST/GET /api/v1/secrets the --to-secret path
// writes through.
type fakeCloudbox struct {
	t       *testing.T
	mu      sync.Mutex
	next    int
	byID    map[string]*fakeTokenRow
	byValue map[string]*fakeTokenRow // static caller rows + minted rows
	secrets map[string]string
	// lastVaultBearer is the Authorization header of the last vault PUT.
	// It proves WHICH credential the --to-secret path wrote with.
	lastVaultBearer string
}

var validTokenName = regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)

func newFakeCloudbox(t *testing.T) *fakeCloudbox {
	t.Helper()
	f := &fakeCloudbox{
		byID:    map[string]*fakeTokenRow{},
		byValue: map[string]*fakeTokenRow{},
		secrets: map[string]string{},
	}
	// Static caller rows; minted rows are registered under their value too.
	// caller-token is an Outpost pairing-style credential: it mints the
	// task scopes it holds but carries NO secrets:write, so the vault
	// write surface refuses it with 403 exactly like a real host.
	// secrets-token is the vault credential the secrets chain resolves
	// ($BASHY_SECRETS_TOKEN in tests).
	f.byValue["caller-token"] = &fakeTokenRow{owner: "u1", scopes: []string{"tasks:read", "tasks:write"}}
	f.byValue["poor-token"] = &fakeTokenRow{owner: "u2", scopes: []string{"tasks:read"}}
	f.byValue["secrets-token"] = &fakeTokenRow{owner: "u1", scopes: []string{"secrets:read", "secrets:write"}}
	return f
}

func (f *fakeCloudbox) caller(r *http.Request) (*fakeTokenRow, bool) {
	v := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	v = strings.TrimSpace(v)
	c, ok := f.byValue[v]
	return c, ok && v != ""
}

func (f *fakeCloudbox) writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"error":%q}`, msg)
}

func (f *fakeCloudbox) serveTokens(w http.ResponseWriter, r *http.Request) {
	caller, ok := f.caller(r)
	if !ok {
		f.writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tokens":
		var body struct {
			Name   string   `json:"name"`
			Scopes []string `json:"scopes"`
			TTL    string   `json:"ttl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.writeErr(w, http.StatusBadRequest, "invalid body")
			return
		}
		if !validTokenName.MatchString(body.Name) {
			f.writeErr(w, http.StatusBadRequest, "invalid name")
			return
		}
		if len(body.Scopes) == 0 {
			f.writeErr(w, http.StatusBadRequest, "empty scopes")
			return
		}
		ttl, ok := parseFakeTTL(body.TTL)
		if !ok {
			f.writeErr(w, http.StatusBadRequest, "invalid ttl")
			return
		}
		held := map[string]bool{}
		for _, s := range caller.scopes {
			held[s] = true
		}
		for _, s := range body.Scopes {
			if strings.HasSuffix(s, ":admin") {
				f.writeErr(w, http.StatusForbidden, "scope "+s+" is not mintable over Bearer")
				return
			}
			if !held[s] {
				f.writeErr(w, http.StatusForbidden, "scope "+s+" is not held by the calling token")
				return
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, row := range f.byID {
			if row.owner == caller.owner && row.name == body.Name {
				f.writeErr(w, http.StatusConflict, "duplicate name")
				return
			}
		}
		f.next++
		id := fmt.Sprintf("uuid-%d", f.next)
		val := "minted-" + id
		exp := time.Now().UTC().Add(ttl).Truncate(time.Second)
		row := &fakeTokenRow{id: id, name: body.Name, owner: caller.owner, scopes: body.Scopes, expires: exp, value: val}
		f.byID[id] = row
		f.byValue[val] = row
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"id": id, "name": body.Name, "scopes": body.Scopes,
			"expires_at": exp.Format(time.RFC3339), "token": val,
		})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tokens":
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []map[string]any{}
		for _, row := range f.byID {
			if row.owner != caller.owner {
				continue
			}
			out = append(out, map[string]any{
				"id": row.id, "name": row.name, "scopes": row.scopes,
				"expires_at": row.expires.Format(time.RFC3339),
				"created":    row.expires.Add(-time.Hour).Format(time.RFC3339),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/tokens/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/tokens/")
		f.mu.Lock()
		defer f.mu.Unlock()
		row, ok := f.byID[id]
		if !ok || row.owner != caller.owner {
			f.writeErr(w, http.StatusNotFound, "not found")
			return
		}
		if "Bearer "+row.value == r.Header.Get("Authorization") {
			f.writeErr(w, http.StatusConflict, "cannot delete the calling token")
			return
		}
		delete(f.byID, id)
		delete(f.byValue, row.value)
		w.WriteHeader(http.StatusNoContent)
	default:
		f.writeErr(w, http.StatusNotFound, "not found")
	}
}

func parseFakeTTL(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	var n int
	var unit byte
	if _, err := fmt.Sscanf(s, "%d%c", &n, &unit); err != nil || n <= 0 {
		return 0, false
	}
	var d time.Duration
	switch unit {
	case 'h':
		d = time.Duration(n) * time.Hour
	case 'd':
		d = time.Duration(n) * 24 * time.Hour
	default:
		return 0, false
	}
	if d > 365*24*time.Hour {
		return 0, false
	}
	return d, true
}

func (f *fakeCloudbox) serveSecrets(w http.ResponseWriter, r *http.Request) {
	caller, ok := f.caller(r)
	if !ok {
		f.writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/api/v1/secrets" {
		// The vault write surface requires secrets:write: a pairing-style
		// caller token is refused with 403, exactly like a real host.
		held := false
		for _, s := range caller.scopes {
			if s == "secrets:write" {
				held = true
				break
			}
		}
		if !held {
			f.writeErr(w, http.StatusForbidden, "scope secrets:write is not held by the calling token")
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/secrets":
		var body struct {
			Secrets []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"secrets"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.writeErr(w, http.StatusBadRequest, "invalid body")
			return
		}
		f.lastVaultBearer = r.Header.Get("Authorization")
		for _, s := range body.Secrets {
			f.secrets[s.Name] = s.Value
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/secrets/"):
		name := strings.TrimPrefix(r.URL.Path, "/api/v1/secrets/")
		v, ok := f.secrets[name]
		if !ok {
			f.writeErr(w, http.StatusNotFound, "not found")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"name": name, "value": v})
	default:
		f.writeErr(w, http.StatusNotFound, "not found")
	}
}

func (f *fakeCloudbox) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/tokens", f.serveTokens)
	mux.HandleFunc("/api/v1/tokens/", f.serveTokens)
	mux.HandleFunc("/api/v1/secrets", f.serveSecrets)
	mux.HandleFunc("/api/v1/secrets/", f.serveSecrets)
	return mux
}

// tokenHost gives a test a hermetic config home plus env pointing the
// ResolveCloud ladder at the fake (BASHY_CLOUDBOX_URL + BASHY_FLEET_TOKEN)
// and the secrets chain at the fake's vault credential
// ($BASHY_SECRETS_TOKEN, exactly like a host with `bashy secret` set up).
func tokenHost(t *testing.T, srv *httptest.Server, token string) {
	t.Helper()
	home := t.TempDir()
	cfg := filepath.Join(home, ".config")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("BASHY_CLOUDBOX_URL", srv.URL)
	t.Setenv("BASHY_FLEET_TOKEN", token)
	t.Setenv("BASHY_SECRETS_TOKEN", "secrets-token")
	t.Setenv("BASHY_API_KEY", "")
}

func startFake(t *testing.T) (*fakeCloudbox, *httptest.Server) {
	t.Helper()
	f := newFakeCloudbox(t)
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return f, srv
}

// ---- attenuation rule (client level) -------------------------------------

func TestTokenCreateSubsetOK(t *testing.T) {
	_, srv := startFake(t)
	tokenHost(t, srv, "caller-token")
	c, err := NewTokenClient("", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.Create("fleet-coordination", []string{"tasks:read", "tasks:write"}, "90d")
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "fleet-coordination" || m.Token == "" || m.ID == "" {
		t.Fatalf("minted = %+v", m)
	}
	if _, err := time.Parse(time.RFC3339, m.ExpiresAt); err != nil {
		t.Fatalf("expires_at %q is not RFC3339: %v", m.ExpiresAt, err)
	}
}

func TestTokenCreateEscalationRefused(t *testing.T) {
	_, srv := startFake(t)
	tokenHost(t, srv, "poor-token") // holds only tasks:read
	c, err := NewTokenClient("", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Create("too-much", []string{"tasks:read", "tasks:write"}, "90d")
	if err == nil {
		t.Fatal("expected a 403 for a scope the caller lacks")
	}
	if !strings.Contains(err.Error(), "tasks:write") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("error should name the scope and the 403, got: %v", err)
	}
}

func TestTokenCreateAdminNeverMintable(t *testing.T) {
	_, srv := startFake(t)
	tokenHost(t, srv, "caller-token")
	c, err := NewTokenClient("", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"tasks:admin", "secrets:admin"} {
		if _, err := c.Create("admin-grab", []string{scope}, "90d"); err == nil {
			t.Fatalf("expected a 403 for %s", scope)
		} else if !strings.Contains(err.Error(), "403") {
			t.Fatalf("expected a 403 for %s, got: %v", scope, err)
		}
	}
}

func TestTokenCreateValidation(t *testing.T) {
	_, srv := startFake(t)
	tokenHost(t, srv, "caller-token")
	c, err := NewTokenClient("", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		scopes []string
		ttl    string
	}{
		{"", []string{"tasks:read"}, "90d"},          // empty name
		{"BAD NAME!", []string{"tasks:read"}, "90d"}, // bad charset
		{"ok-name", nil, "90d"},                      // empty scopes
		{"ok-name", []string{"tasks:read"}, ""},      // missing ttl
		{"ok-name", []string{"tasks:read"}, "90x"},   // bad ttl unit
		{"ok-name", []string{"tasks:read"}, "400d"},  // over max
	} {
		if _, err := c.Create(tc.name, tc.scopes, tc.ttl); err == nil {
			t.Fatalf("Create(%q, %v, %q): expected an error", tc.name, tc.scopes, tc.ttl)
		}
	}
}

func TestTokenDuplicateNameConflict(t *testing.T) {
	_, srv := startFake(t)
	tokenHost(t, srv, "caller-token")
	c, err := NewTokenClient("", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Create("dup", []string{"tasks:read"}, "24h"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Create("dup", []string{"tasks:read"}, "24h"); err == nil {
		t.Fatal("expected a 409 on duplicate name")
	} else if !strings.Contains(err.Error(), "409") {
		t.Fatalf("expected a 409, got: %v", err)
	}
}

func TestTokenListAndRevoke(t *testing.T) {
	_, srv := startFake(t)
	tokenHost(t, srv, "caller-token")
	c, err := NewTokenClient("", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.Create("ephemeral", []string{"tasks:read"}, "24h")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "ephemeral" || rows[0].ID != m.ID {
		t.Fatalf("list = %+v", rows)
	}
	if err := c.Revoke(m.ID); err != nil {
		t.Fatal(err)
	}
	rows, err = c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("list after revoke = %+v", rows)
	}
	// Revoking an unknown id is 404.
	if err := c.Revoke("uuid-999"); err == nil {
		t.Fatal("expected a 404 for an unknown id")
	}
}

func TestTokenRevokeSelfRefused(t *testing.T) {
	f, srv := startFake(t)
	tokenHost(t, srv, "caller-token")
	c, err := NewTokenClient("", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.Create("selfish", []string{"tasks:read"}, "24h")
	if err != nil {
		t.Fatal(err)
	}
	// Authenticate AS the minted token, then try to delete it: 409.
	tokenHost(t, srv, m.Token)
	c2, err := NewTokenClient("", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c2.Revoke(m.ID); err == nil {
		t.Fatal("expected a 409 when deleting the calling token")
	} else if !strings.Contains(err.Error(), "409") {
		t.Fatalf("expected a 409, got: %v", err)
	}
	_ = f
}

func TestTokenListOwnerScoped(t *testing.T) {
	_, srv := startFake(t)
	tokenHost(t, srv, "caller-token") // owner u1
	c, err := NewTokenClient("", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Create("u1-only", []string{"tasks:read"}, "24h"); err != nil {
		t.Fatal(err)
	}
	tokenHost(t, srv, "poor-token") // owner u2
	c2, err := NewTokenClient("", "")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := c2.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == "u1-only" {
			t.Fatalf("u2 listed u1's token: %+v", rows)
		}
	}
	// Cross-owner revoke is 404, not 403.
	if err := c2.Revoke("uuid-1"); err == nil {
		t.Fatal("expected a 404 on cross-owner revoke")
	} else if !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected a 404, got: %v", err)
	}
}

// ---- cobra: create never prints the value --------------------------------

func runTokenCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := NewTokenCmd()
	var out, errB strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&errB)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errB.String(), err
}

func TestTokenCmdCreateToSecret(t *testing.T) {
	f, srv := startFake(t)
	tokenHost(t, srv, "caller-token")
	stdout, stderr, err := runTokenCmd(t, "create",
		"--name", "fleet-coordination",
		"--scope", "tasks:read,tasks:write",
		"--ttl", "90d",
		"--to-secret", "DHNT_FLEET_COORD_TOKEN")
	if err != nil {
		t.Fatalf("create: %v (stdout=%q stderr=%q)", err, stdout, stderr)
	}
	f.mu.Lock()
	got := f.secrets["DHNT_FLEET_COORD_TOKEN"]
	f.mu.Unlock()
	if got == "" {
		t.Fatal("the vault never received the minted value")
	}
	// The value went to the vault; it must not appear on either stream.
	if strings.Contains(stdout, got) || strings.Contains(stderr, got) {
		t.Fatalf("the token value was printed (stdout=%q stderr=%q)", stdout, stderr)
	}
	// But the metadata is reported.
	for _, want := range []string{"fleet-coordination", "tasks:read", "tasks:write"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout %q lacks %q", stdout, want)
		}
	}
	// The vault holds a token that authenticates.
	tokenHost(t, srv, got)
	c, err := NewTokenClient("", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.List(); err != nil {
		t.Fatalf("minted token does not authenticate: %v", err)
	}
}

// The vault write must resolve through the secrets chain, never the
// minting caller's token: on a real host the caller is the Outpost
// pairing token (outpost scopes, NO secrets:write), which the vault
// refuses with 403. The fake models that split — its vault surface
// accepts only the secrets-token credential and rejects the caller
// token — so this fails while the write uses callerToken and passes
// once it walks secrets.Config{}.Resolve().
func TestTokenCmdCreateToSecretUsesSecretsChain(t *testing.T) {
	f, srv := startFake(t)
	tokenHost(t, srv, "caller-token") // minting caller: NO secrets:write
	stdout, stderr, err := runTokenCmd(t, "create",
		"--name", "fleet-coordination",
		"--scope", "tasks:read,tasks:write",
		"--ttl", "90d",
		"--to-secret", "CHAIN_PROBE")
	if err != nil {
		t.Fatalf("create: %v (stdout=%q stderr=%q)", err, stdout, stderr)
	}
	f.mu.Lock()
	got := f.secrets["CHAIN_PROBE"]
	bearer := f.lastVaultBearer
	f.mu.Unlock()
	if got == "" {
		t.Fatal("the vault never received the minted value")
	}
	if bearer != "Bearer secrets-token" {
		t.Fatalf("vault write used %q, want the secrets-chain credential", bearer)
	}
	if strings.Contains(stdout, got) || strings.Contains(stderr, got) {
		t.Fatalf("the token value was printed (stdout=%q stderr=%q)", stdout, stderr)
	}
}

// A failed vault write still mints first and reports the orphaned row's
// name AND id (so it can be revoked) without ever printing the value.
func TestTokenCmdCreateToSecretFailureKeepsID(t *testing.T) {
	f, srv := startFake(t)
	tokenHost(t, srv, "caller-token")
	os.Unsetenv("BASHY_SECRETS_TOKEN") // break the secrets chain
	stdout, stderr, err := runTokenCmd(t, "create",
		"--name", "doomed-secret",
		"--scope", "tasks:read",
		"--ttl", "24h",
		"--to-secret", "NOWHERE")
	if err == nil {
		t.Fatal("expected the vault write to fail without a secrets credential")
	}
	msg := err.Error()
	if !strings.Contains(msg, "but the vault write failed") {
		t.Fatalf("error should say the vault write failed, got: %v", err)
	}
	if !strings.Contains(msg, "doomed-secret") {
		t.Fatalf("error should name the minted token, got: %v", err)
	}
	f.mu.Lock()
	var id, val string
	for _, row := range f.byID {
		if row.name == "doomed-secret" {
			id, val = row.id, row.value
		}
	}
	f.mu.Unlock()
	if id == "" {
		t.Fatal("the token was not minted before the vault write failed")
	}
	if !strings.Contains(msg, id) {
		t.Fatalf("error should keep the token id %s visible (to revoke it), got: %v", id, err)
	}
	for _, s := range []string{msg, stdout, stderr} {
		if val != "" && strings.Contains(s, val) {
			t.Fatalf("the token value leaked: %q", s)
		}
	}
}

func TestTokenCmdCreateStdoutOnlyWithFlag(t *testing.T) {
	_, srv := startFake(t)
	tokenHost(t, srv, "caller-token")
	// Without --to-secret and without --stdout there is nowhere safe to put
	// the value: refuse rather than print it.
	_, _, err := runTokenCmd(t, "create",
		"--name", "fleet-coordination",
		"--scope", "tasks:read,tasks:write",
		"--ttl", "90d")
	if err == nil {
		t.Fatal("expected create without --to-secret/--stdout to fail")
	}
	// With --stdout the human asked for it: the value prints.
	stdout, _, err := runTokenCmd(t, "create",
		"--name", "fleet-coordination",
		"--scope", "tasks:read,tasks:write",
		"--ttl", "90d", "--stdout")
	if err != nil {
		t.Fatalf("create --stdout: %v", err)
	}
	if !strings.Contains(stdout, "minted-uuid-") {
		t.Fatalf("expected the value on stdout with --stdout, got %q", stdout)
	}
}

func TestTokenCmdEscalationSurfaced(t *testing.T) {
	_, srv := startFake(t)
	tokenHost(t, srv, "poor-token")
	stdout, stderr, err := runTokenCmd(t, "create",
		"--name", "x", "--scope", "tasks:read,tasks:write",
		"--ttl", "90d", "--stdout")
	if err == nil {
		t.Fatal("expected the escalation to fail")
	}
	if !strings.Contains(err.Error(), "tasks:write") {
		t.Fatalf("error should name the missing scope, got: %v", err)
	}
	if strings.Contains(stdout, "minted-") || strings.Contains(stderr, "minted-") {
		t.Fatal("no token value may print on failure")
	}
}

func TestTokenCmdListHidesValues(t *testing.T) {
	_, srv := startFake(t)
	tokenHost(t, srv, "caller-token")
	c, err := NewTokenClient("", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.Create("listed", []string{"tasks:read"}, "24h")
	if err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runTokenCmd(t, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(stdout, "listed") {
		t.Fatalf("list lacks the token name: %q", stdout)
	}
	if strings.Contains(stdout, m.Token) {
		t.Fatalf("list printed the token value: %q", stdout)
	}
}

func TestTokenCmdRevoke(t *testing.T) {
	_, srv := startFake(t)
	tokenHost(t, srv, "caller-token")
	c, err := NewTokenClient("", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.Create("doomed", []string{"tasks:read"}, "24h")
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runTokenCmd(t, "revoke", m.ID)
	if err != nil {
		t.Fatalf("revoke: %v (stdout=%q stderr=%q)", err, stdout, stderr)
	}
	if rows, err := c.List(); err != nil || len(rows) != 0 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

// ---- master-token mint ----------------------------------------------------

func TestMasterTokenMintWritesFile(t *testing.T) {
	_, srv := startFake(t)
	tokenHost(t, srv, "caller-token")
	// --from names a pre-migration agent.json whose credential can mint.
	src := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(src, []byte(`{"server_addr":"`+srv.URL+`","access_token":"caller-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := NewMasterTokenCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"mint", "--from", src, "--host-name", "NOVIDESIGN"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("mint: %v (%s)", err, out.String())
	}
	path := MasterTokenPath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	val := strings.TrimSpace(lines[0])
	if val == "" || !strings.HasPrefix(val, "minted-uuid-") {
		t.Fatalf("file does not hold the minted value: %q", data)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, want 0600", fi.Mode())
		}
	}
	if strings.Contains(out.String(), val) {
		t.Fatalf("the minted value was printed: %q", out.String())
	}
	// The saved credential resolves through the #599 ladder.
	if _, tok := ResolveCloud("", ""); tok == "" {
		t.Fatal("ResolveCloud finds no credential after mint")
	}
}

func TestMasterTokenMintIdempotent(t *testing.T) {
	f, srv := startFake(t)
	tokenHost(t, srv, "caller-token")
	src := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(src, []byte(`{"server_addr":"`+srv.URL+`","access_token":"caller-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mint := func() {
		t.Helper()
		cmd := NewMasterTokenCmd()
		cmd.SetOut(&strings.Builder{})
		cmd.SetErr(&strings.Builder{})
		cmd.SetArgs([]string{"mint", "--from", src, "--host-name", "noviwin1"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("mint: %v", err)
		}
	}
	mint()
	first, _ := os.ReadFile(MasterTokenPath())
	mint()
	second, _ := os.ReadFile(MasterTokenPath())
	if string(first) == string(second) {
		t.Fatal("re-run should rotate the file's token")
	}
	// One token per host: the replaced coord-noviwin1 row was revoked.
	f.mu.Lock()
	n := 0
	for _, row := range f.byID {
		if row.name == "coord-noviwin1" {
			n++
		}
	}
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("coord-noviwin1 rows = %d, want 1", n)
	}
}

func TestMasterTokenMintWithoutFromUsesPairing(t *testing.T) {
	_, srv := startFake(t)
	// No --from: the host is still paired to the base, so ResolveCloud's
	// pairing token is the caller credential.
	tokenHost(t, srv, "")
	t.Setenv("BASHY_FLEET_TOKEN", "caller-token")
	cmd := NewMasterTokenCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"mint", "--host-name", "h1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("mint: %v (%s)", err, out.String())
	}
	if _, err := os.Stat(MasterTokenPath()); err != nil {
		t.Fatalf("no master-token file: %v", err)
	}
}

func TestMasterTokenSaveStillWorks(t *testing.T) {
	pairedHost(t, "sat1.dhnt.io")
	src := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(src, []byte(`{"server_addr":"ai.dhnt.io","access_token":"old-master"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := NewMasterTokenCmd()
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	cmd.SetArgs([]string{"save", "--from", src})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("save: %v", err)
	}
}
