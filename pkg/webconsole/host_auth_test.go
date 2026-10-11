// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/websession"
)

const (
	synthHostToken = "synthetic-outpost-bearer-0123456789abcdef"
	synthSSOSecret = "synthetic-sso-secret-fedcba9876543210"
	synthOwner     = "owner@example.test"
	synthSharee    = "sharee@example.test"
	synthCloudBase = "/matrix/h/synthhost/app/apps"
	lanPeer        = "192.168.1.44:5555"
	loPeer         = "127.0.0.1:5555"
)

type hostUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	seen []seenReq
}

type seenReq struct{ Path, Auth string }

func (u *hostUpstream) requests() []seenReq {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]seenReq(nil), u.seen...)
}

func (u *hostUpstream) reset() {
	u.mu.Lock()
	u.seen = nil
	u.mu.Unlock()
}

func (u *hostUpstream) addr() string { return strings.TrimPrefix(u.URL, "http://") }

// writeOutpostConfig installs a synthetic outpost agent.json at the canonical
// path under a private XDG_CONFIG_HOME. A nil map writes no config at all.
func writeOutpostConfig(t *testing.T, cfg map[string]any, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	p := filepath.Join(dir, "matrix", "agent.json")
	if cfg == nil {
		return p
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// fullConfig models outpost's current config, including the service_sso key
// minted and persisted for the console route. Mutate the returned map per test.
func fullConfig() map[string]any {
	return map[string]any{
		"mcp_bearer_token": synthHostToken,
		"oauth2_email":     synthOwner,
		"service_sso":      map[string]any{"apps": synthSSOSecret},
	}
}

// newHostUpstream stands in for outpost's admin listener, records the path and
// Authorization of every request that reaches it, points the Host panel at it
// through $OUTPOST_ADMIN_ADDR and installs fullConfig() as the trusted config.
func newHostUpstream(t *testing.T) *hostUpstream {
	t.Helper()
	u := &hostUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.seen = append(u.seen, seenReq{r.URL.Path, r.Header.Get("Authorization")})
		u.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(u.Close)
	t.Setenv("OUTPOST_ADMIN_ADDR", u.addr())
	withOutpostAdminResolver(t, outpostConfig{}, nil, "127.0.0.1:17777")
	writeOutpostConfig(t, fullConfig(), 0o600)
	return u
}

func authOf(t *testing.T, u *hostUpstream, path string) (string, bool) {
	t.Helper()
	for _, r := range u.requests() {
		if r.Path == path {
			return r.Auth, true
		}
	}
	return "", false
}

func wantBridged(t *testing.T, u *hostUpstream, what string) {
	t.Helper()
	if got, seen := authOf(t, u, "/api/status"); !seen || got != "Bearer "+synthHostToken {
		t.Fatalf("%s: upstream Authorization = %q (seen=%v), want the host bearer", what, got, seen)
	}
}

func wantNoCredential(t *testing.T, u *hostUpstream, what string) {
	t.Helper()
	if got, seen := authOf(t, u, "/api/status"); !seen || got != "" {
		t.Fatalf("%s: upstream Authorization = %q (seen=%v), want none — not even the browser's", what, got, seen)
	}
}

func operatorEnv(t *testing.T) (http.Handler, http.Header) {
	t.Helper()
	sessions := websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt"))
	h := newTestHandler(t, Options{RequireLogin: true, Sessions: sessions})
	cookie, err := sessions.Mint("operator")
	if err != nil {
		t.Fatal(err)
	}
	return h, http.Header{"Cookie": {sessionCookie + "=" + cookie}, "Authorization": {"Bearer browser-supplied"}}
}

func TestHostAdminBearerIsAttachedForTheLoopbackOperator(t *testing.T) {
	u := newHostUpstream(t)
	h := newTestHandler(t, Options{})
	hdr := http.Header{"Authorization": {"Bearer browser-supplied"}}
	w := do(h, "GET", "/host/api/status", loPeer, hdr)
	if w.Code != 200 {
		t.Fatalf("host api = %d: %s", w.Code, w.Body.String())
	}
	wantBridged(t, u, "ungated loopback operator")
	if strings.Contains(w.Body.String(), synthHostToken) || strings.Contains(w.Header().Get("Authorization"), synthHostToken) {
		t.Fatal("the token reached the browser")
	}
	for k, vs := range w.Header() {
		for _, v := range vs {
			if strings.Contains(v, synthHostToken) {
				t.Fatalf("token in response header %s", k)
			}
		}
	}
}

func TestHostAdminBearerAdmittedOperatorSessionFromAnyBrowserPeer(t *testing.T) {
	u := newHostUpstream(t)
	h, hdr := operatorEnv(t)
	for _, peer := range []string{loPeer, lanPeer} {
		u.reset()
		if w := do(h, "GET", "/host/api/status", peer, hdr); w.Code != 200 {
			t.Fatalf("operator session from %s = %d", peer, w.Code)
		}
		wantBridged(t, u, "operator cookie from "+peer)
	}
}

func TestHostAdminBearerStaysOnTheAdminAPI(t *testing.T) {
	u := newHostUpstream(t)
	h := newTestHandler(t, Options{})
	hdr := http.Header{"Authorization": {"Bearer browser-supplied"}}
	for _, p := range []string{"/host/", "/host/mcp/", "/host/_periscope/x", "/host/somapp/api/status", "/host/api"} {
		do(h, "GET", p, loPeer, hdr)
	}
	do(h, "GET", "/host/api/%2e%2e/mcp/", loPeer, hdr)
	do(h, "GET", "/host/api//..//mcp/", loPeer, hdr)
	for _, r := range u.requests() {
		if !strings.HasPrefix(r.Path, "/api/") && r.Auth != "" {
			t.Errorf("Authorization %q reached non-admin-API path %q", r.Auth, r.Path)
		}
		if strings.Contains(r.Auth, "browser-supplied") {
			t.Errorf("browser-supplied Authorization forwarded to %q", r.Path)
		}
		if r.Auth != "" && strings.Contains(r.Path, "..") {
			t.Errorf("credential attached to non-canonical path %q", r.Path)
		}
	}
}

func TestHostAdminBearerAbsentOrUnreadableConfigSendsNothing(t *testing.T) {
	cases := map[string]func(*testing.T){
		"no config file": func(t *testing.T) { writeOutpostConfig(t, nil, 0o600) },
		"no token":       func(t *testing.T) { writeOutpostConfig(t, map[string]any{"oauth2_email": synthOwner}, 0o600) },
		"short token":    func(t *testing.T) { writeOutpostConfig(t, map[string]any{"mcp_bearer_token": "abc"}, 0o600) },
		"header-injection token": func(t *testing.T) {
			writeOutpostConfig(t, map[string]any{"mcp_bearer_token": "aaaaaaaaaaaaaaaaaaaa\r\nX-Evil: 1"}, 0o600)
		},
	}
	if runtimeIsUnix() {
		cases["group/world-readable"] = func(t *testing.T) { writeOutpostConfig(t, fullConfig(), 0o644) }
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			u := newHostUpstream(t)
			setup(t)
			h := newTestHandler(t, Options{})
			hdr := http.Header{"Authorization": {"Bearer browser-supplied"}}
			if w := do(h, "GET", "/host/api/status", loPeer, hdr); w.Code != 200 {
				t.Fatalf("host api = %d", w.Code)
			}
			wantNoCredential(t, u, name)
		})
	}
}

func TestHostAdminBearerTokenRotationIsImmediate(t *testing.T) {
	u := newHostUpstream(t)
	h := newTestHandler(t, Options{})
	do(h, "GET", "/host/api/status", loPeer, nil)
	wantBridged(t, u, "before rotation")
	u.reset()
	cfg := fullConfig()
	cfg["mcp_bearer_token"] = "rotated-" + synthHostToken
	writeOutpostConfigAt(t, cfg)
	do(h, "GET", "/host/api/status", loPeer, nil)
	if got, _ := authOf(t, u, "/api/status"); got != "Bearer rotated-"+synthHostToken {
		t.Fatalf("after rotation Authorization = %q", got)
	}
}

// writeOutpostConfigAt rewrites the config already installed by the current
// XDG_CONFIG_HOME, as `outpost` does on rotation.
func writeOutpostConfigAt(t *testing.T, cfg map[string]any) {
	t.Helper()
	p := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "matrix", "agent.json")
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHostAdminBearerNeverGoesToAnExplicitRemoteEndpoint(t *testing.T) {
	writeOutpostConfig(t, fullConfig(), 0o600)
	for _, addr := range []string{"10.20.30.40:17777", "example.test:17777", "0.0.0.0:17777", "192.168.1.9:17777"} {
		t.Setenv("OUTPOST_ADMIN_ADDR", addr)
		withOutpostAdminResolver(t, outpostConfig{}, nil, "127.0.0.1:17777")
		if p := hostBuiltinPanel(); p.hostAdmin {
			t.Errorf("%s: remote admin endpoint is eligible for the credential", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:17777", "[::1]:17777", "localhost:17777", "127.0.0.9:1"} {
		t.Setenv("OUTPOST_ADMIN_ADDR", addr)
		withOutpostAdminResolver(t, outpostConfig{}, nil, "127.0.0.1:17777")
		if p := hostBuiltinPanel(); !p.hostAdmin {
			t.Errorf("%s: same-host loopback endpoint should be eligible", addr)
		}
	}
	t.Setenv("OUTPOST_ADMIN_ADDR", "")
	withOutpostAdminResolver(t, outpostConfig{AdminAddr: "10.0.0.5:17777"}, nil, "127.0.0.1:17777")
	if p := hostBuiltinPanel(); p.hostAdmin {
		t.Error("remote admin_addr from outpost config is eligible for the credential")
	}
}

func TestHostAdminBearerBoundToTheConfiguredAdminEndpoint(t *testing.T) {
	t.Run("default endpoint when the config names none", func(t *testing.T) {
		u := newHostUpstream(t)
		t.Setenv("OUTPOST_ADMIN_ADDR", "")
		withOutpostAdminResolver(t, outpostConfig{}, nil, u.addr()) // default == upstream
		h := newTestHandler(t, Options{})
		do(h, "GET", "/host/api/status", loPeer, nil)
		wantBridged(t, u, "default endpoint")
	})
	t.Run("file admin_addr matches the panel", func(t *testing.T) {
		u := newHostUpstream(t)
		t.Setenv("OUTPOST_ADMIN_ADDR", "")
		cfg := fullConfig()
		cfg["admin_addr"] = u.addr()
		writeOutpostConfigAt(t, cfg)
		withOutpostAdminResolver(t, outpostConfig{AdminAddr: u.addr()}, nil, "127.0.0.1:17777")
		h := newTestHandler(t, Options{})
		do(h, "GET", "/host/api/status", loPeer, nil)
		wantBridged(t, u, "matching admin_addr")
	})
	t.Run("panel on an arbitrary other loopback port gets nothing", func(t *testing.T) {
		u := newHostUpstream(t)
		t.Setenv("OUTPOST_ADMIN_ADDR", "")
		cfg := fullConfig()
		cfg["admin_addr"] = "127.0.0.1:1" // the config says outpost lives elsewhere
		writeOutpostConfigAt(t, cfg)
		// The panel resolves a different loopback endpoint (a stale or foreign
		// `outpost config show`), which is where the proxy dials.
		withOutpostAdminResolver(t, outpostConfig{AdminAddr: u.addr()}, nil, "127.0.0.1:17777")
		h := newTestHandler(t, Options{})
		if w := do(h, "GET", "/host/api/status", loPeer, http.Header{"Authorization": {"Bearer browser-supplied"}}); w.Code != 200 {
			t.Fatalf("host api = %d", w.Code)
		}
		wantNoCredential(t, u, "endpoint mismatch")
	})
	t.Run("env override wins like outpost's own precedence", func(t *testing.T) {
		u := newHostUpstream(t) // env == upstream
		cfg := fullConfig()
		cfg["admin_addr"] = "127.0.0.1:1"
		writeOutpostConfigAt(t, cfg)
		h := newTestHandler(t, Options{})
		do(h, "GET", "/host/api/status", loPeer, nil)
		wantBridged(t, u, "env override")
	})
	t.Run("unusable file admin_addr fails closed", func(t *testing.T) {
		u := newHostUpstream(t)
		t.Setenv("OUTPOST_ADMIN_ADDR", "")
		cfg := fullConfig()
		cfg["admin_addr"] = "not-an-address"
		writeOutpostConfigAt(t, cfg)
		withOutpostAdminResolver(t, outpostConfig{}, nil, u.addr())
		h := newTestHandler(t, Options{})
		do(h, "GET", "/host/api/status", loPeer, nil)
		wantNoCredential(t, u, "bad admin_addr")
	})
}

func TestHostAdminBearerRequiresTheStockHostPanel(t *testing.T) {
	u := newHostUpstream(t)
	port := mustPort(t, u.URL)
	h := newTestHandler(t, Options{Panels: []Panel{
		{Name: "other", Label: "Other", Path: "/other/", Mode: "proxy", Port: port, Available: true, Auth: AuthSystem, Source: "app"},
	}})
	if w := do(h, "GET", "/other/api/status", loPeer, nil); w.Code != 200 {
		t.Fatalf("other = %d", w.Code)
	}
	wantNoCredential(t, u, "third-party panel")
}

func TestHostAdminBearerDeniedWhereTheLauncherGateDenies(t *testing.T) {
	u := newHostUpstream(t)
	locked := newTestHandler(t, Options{RequireLogin: true,
		Sessions: websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt"))})
	for _, peer := range []string{loPeer, lanPeer} {
		if w := do(locked, "GET", "/host/api/status", peer, http.Header{"Accept": {"application/json"}}); w.Code != http.StatusForbidden {
			t.Errorf("login-required, no session, peer %s = %d, want 403", peer, w.Code)
		}
	}
	open := newTestHandler(t, Options{})
	if w := do(open, "GET", "/host/api/status", lanPeer, nil); w.Code != http.StatusForbidden {
		t.Errorf("non-loopback peer = %d, want 403", w.Code)
	}
	if w := do(locked, "GET", "/host/api/status", loPeer, http.Header{"Cookie": {sessionCookie + "=forged"}}); w.Code != http.StatusForbidden {
		t.Errorf("forged cookie = %d, want 403", w.Code)
	}
	if n := len(u.requests()); n != 0 {
		t.Fatalf("%d request(s) reached outpost despite the gate denying", n)
	}
}

func TestHostAdminBearerPairedDevice(t *testing.T) {
	u := newHostUpstream(t)
	h, store := pairEnv(t)

	// Default scope does not include Host: refused by the gate, never forwarded.
	cookie := redeemScan(t, h, store, nil, time.Hour)
	if w := do(h, "GET", "/host/api/status", lanPeer, withCookie(cookie)); w.Code != http.StatusForbidden {
		t.Fatalf("out-of-scope device = %d, want 403", w.Code)
	}
	if n := len(u.requests()); n != 0 {
		t.Fatalf("out-of-scope device forwarded %d request(s)", n)
	}

	// A device paired for Host uses the bridge, from the LAN and from loopback,
	// and its own Authorization header is replaced rather than forwarded.
	hostDev := redeemScan(t, h, store, []string{"host"}, time.Hour)
	for _, peer := range []string{lanPeer, loPeer} {
		u.reset()
		hdr := withCookie(hostDev)
		hdr.Set("Authorization", "Bearer device-supplied")
		if w := do(h, "GET", "/host/api/status", peer, hdr); w.Code != 200 {
			t.Fatalf("in-scope device from %s = %d: %s", peer, w.Code, w.Body.String())
		}
		wantBridged(t, u, "in-scope device from "+peer)
	}
	// Scope is per panel: the Host-scoped device is still refused elsewhere.
	if w := do(h, "GET", "/sprint/", lanPeer, withCookie(hostDev)); w.Code != http.StatusForbidden {
		t.Fatalf("host-scoped device on /sprint/ = %d, want 403", w.Code)
	}
}

func TestHostAdminBearerDeniedDevicesAreNeverForwarded(t *testing.T) {
	u := newHostUpstream(t)
	h, store := pairEnv(t)

	revoked := redeemScan(t, h, store, []string{"host"}, time.Hour)
	expired := redeemScan(t, h, store, []string{"host"}, 50*time.Millisecond)
	time.Sleep(80 * time.Millisecond)
	if w := do(h, "GET", "/host/api/status", lanPeer, withCookie(expired)); w.Code != http.StatusForbidden {
		t.Fatalf("expired device = %d, want 403", w.Code)
	}
	if _, err := store.revokeAll(); err != nil {
		t.Fatal(err)
	}
	if w := do(h, "GET", "/host/api/status", lanPeer, withCookie(revoked)); w.Code != http.StatusForbidden {
		t.Fatalf("revoked device = %d, want 403", w.Code)
	}

	// The operator grant a device inherits ends: the device ends with it.
	live := redeemScan(t, h, store, []string{"host"}, time.Hour)
	if w := do(h, "GET", "/host/api/status", lanPeer, withCookie(live)); w.Code != 200 {
		t.Fatalf("live device = %d", w.Code)
	}
	u.reset()
	if err := store.mutate(func(st *pairState) error {
		st.Grant.Expires = time.Now().Add(-time.Minute)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := do(h, "GET", "/host/api/status", lanPeer, withCookie(live)); w.Code != http.StatusForbidden {
		t.Fatalf("device after operator grant ended = %d, want 403", w.Code)
	}
	if n := len(u.requests()); n != 0 {
		t.Fatalf("%d request(s) reached outpost for denied devices", n)
	}
}

// signedCloud builds headers exactly as outpost stamps them for a tunnel hop.
func signedCloud(secret, user, groups string, ts int64) http.Header {
	tsS := strconv.FormatInt(ts, 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(user + "\n" + groups + "\n" + synthCloudBase + "\n" + tsS))
	return http.Header{
		"X-Forwarded-Prefix":     {synthCloudBase},
		"Remote-User":            {user},
		"Remote-Email":           {user},
		"Remote-Groups":          {groups},
		"X-Outpost-Identity-Ts":  {tsS},
		"X-Outpost-Identity-Sig": {hex.EncodeToString(mac.Sum(nil))},
	}
}

func cloudRequest(t *testing.T, h http.Handler, hdr http.Header) {
	t.Helper()
	hdr.Set("Authorization", "Bearer browser-supplied")
	if w := do(h, "GET", "/host/api/status", loPeer, hdr); w.Code != 200 {
		t.Fatalf("cloud request = %d: %s", w.Code, w.Body.String())
	}
}

func TestHostAdminBearerTrustedSignedCloudOwner(t *testing.T) {
	now := time.Now().Unix()
	t.Run("owner by oauth2_email, secret from service_sso", func(t *testing.T) {
		u := newHostUpstream(t)
		cloudRequest(t, newTestHandler(t, Options{}), signedCloud(synthSSOSecret, synthOwner, "admin", now))
		wantBridged(t, u, "signed owner")
	})
	t.Run("owner is case-insensitive", func(t *testing.T) {
		u := newHostUpstream(t)
		cloudRequest(t, newTestHandler(t, Options{}), signedCloud(synthSSOSecret, strings.ToUpper(synthOwner), "admin", now))
		wantBridged(t, u, "signed owner, upper case")
	})
	t.Run("admin_users is authoritative when populated", func(t *testing.T) {
		u := newHostUpstream(t)
		cfg := fullConfig()
		cfg["admin_users"] = []any{"Second.Admin@example.test"}
		writeOutpostConfigAt(t, cfg)
		h := newTestHandler(t, Options{})
		cloudRequest(t, h, signedCloud(synthSSOSecret, "second.admin@example.test", "user", now))
		wantBridged(t, u, "admin_users member")
		u.reset()
		cloudRequest(t, h, signedCloud(synthSSOSecret, synthOwner, "admin", now))
		wantNoCredential(t, u, "oauth2_email owner not in a populated admin_users")
	})
	t.Run("custom console app name", func(t *testing.T) {
		u := newHostUpstream(t)
		cfg := fullConfig()
		cfg["service_sso"] = map[string]any{"console": synthSSOSecret}
		writeOutpostConfigAt(t, cfg)
		cloudRequest(t, newTestHandler(t, Options{CloudApp: "console"}), signedCloud(synthSSOSecret, synthOwner, "admin", now))
		wantBridged(t, u, "custom app name")
		u.reset()
		cloudRequest(t, newTestHandler(t, Options{}), signedCloud(synthSSOSecret, synthOwner, "admin", now))
		wantNoCredential(t, u, "secret of a different app")
	})
}

func TestHostAdminBearerCloudNeverElevatesAnyoneElse(t *testing.T) {
	now := time.Now().Unix()
	mk := func(mod func(h http.Header)) http.Header {
		h := signedCloud(synthSSOSecret, synthOwner, "admin", now)
		if mod != nil {
			mod(h)
		}
		return h
	}
	cases := map[string]http.Header{
		"signed sharee holding cloud admin tier": signedCloud(synthSSOSecret, synthSharee, "admin", now),
		"wrong secret":                           signedCloud("some-other-secret-0123456789", synthOwner, "admin", now),
		"stale signature":                        signedCloud(synthSSOSecret, synthOwner, "admin", now-3600),
		"future signature":                       signedCloud(synthSSOSecret, synthOwner, "admin", now+3600),
		"unsigned owner stamp": mk(func(h http.Header) {
			h.Del("X-Outpost-Identity-Sig")
		}),
		"missing timestamp": mk(func(h http.Header) { h.Del("X-Outpost-Identity-Ts") }),
		"tampered user":     mk(func(h http.Header) { h.Set("Remote-User", synthSharee); h.Set("Remote-Email", synthSharee) }),
		"tampered groups":   mk(func(h http.Header) { h.Set("Remote-Groups", "user") }),
		"tampered prefix":   mk(func(h http.Header) { h.Set("X-Forwarded-Prefix", synthCloudBase+"x") }),
		"signed sharee with owner forged into the unsigned email": func() http.Header {
			h := signedCloud(synthSSOSecret, synthSharee, "admin", now)
			h.Set("Remote-Email", synthOwner)
			return h
		}(),
		"remote-groups fallback without any identity match": signedCloud(synthSSOSecret, "someone@example.test", "admin", now),
	}
	for name, hdr := range cases {
		t.Run(name, func(t *testing.T) {
			u := newHostUpstream(t)
			cloudRequest(t, newTestHandler(t, Options{}), hdr)
			wantNoCredential(t, u, name)
		})
	}
}

func TestHostAdminBearerCloudNeedsSecretAndOwnerFromTheSameConfig(t *testing.T) {
	now := time.Now().Unix()
	for name, mut := range map[string]func(map[string]any){
		"no sso secret": func(c map[string]any) { delete(c, "service_sso") },
		"secret belongs to another route": func(c map[string]any) {
			c["service_sso"] = map[string]any{"console": synthSSOSecret}
		},
		"empty route secret": func(c map[string]any) {
			c["service_sso"] = map[string]any{"apps": "  "}
		},
		"no owner identity": func(c map[string]any) { delete(c, "oauth2_email") },
	} {
		t.Run(name, func(t *testing.T) {
			u := newHostUpstream(t)
			cfg := fullConfig()
			mut(cfg)
			writeOutpostConfigAt(t, cfg)
			cloudRequest(t, newTestHandler(t, Options{}), signedCloud(synthSSOSecret, synthOwner, "admin", now))
			wantNoCredential(t, u, name)
		})
	}
	t.Run("no config at all", func(t *testing.T) {
		u := newHostUpstream(t)
		if err := os.Remove(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "matrix", "agent.json")); err != nil {
			t.Fatal(err)
		}
		cloudRequest(t, newTestHandler(t, Options{}), signedCloud(synthSSOSecret, synthOwner, "admin", now))
		wantNoCredential(t, u, "no config")
	})
}

func TestHostAdminBearerCloudSpoofFromANonLoopbackPeerIsRefused(t *testing.T) {
	u := newHostUpstream(t)
	h := newTestHandler(t, Options{})
	hdr := signedCloud(synthSSOSecret, synthOwner, "admin", time.Now().Unix())
	if w := do(h, "GET", "/host/api/status", lanPeer, hdr); w.Code != http.StatusForbidden {
		t.Fatalf("LAN peer carrying a (replayed) tunnel stamp = %d, want 403", w.Code)
	}
	if n := len(u.requests()); n != 0 {
		t.Fatalf("%d request(s) reached outpost", n)
	}
}

func TestHostAdminBearerCloudBridgeStillStaysOnTheAdminAPI(t *testing.T) {
	u := newHostUpstream(t)
	h := newTestHandler(t, Options{})
	for _, p := range []string{"/host/mcp/", "/host/api/%2e%2e/mcp/", "/host/"} {
		hdr := signedCloud(synthSSOSecret, synthOwner, "admin", time.Now().Unix())
		do(h, "GET", p, loPeer, hdr)
	}
	for _, r := range u.requests() {
		if r.Auth != "" {
			t.Errorf("credential attached to %q", r.Path)
		}
	}
}

func TestHostAdminTrustedConfigParsing(t *testing.T) {
	writeOutpostConfig(t, fullConfig(), 0o600)
	tr, ok := loadOutpostTrust("apps")
	if !ok || tr.token != synthHostToken || string(tr.ssoSecret) != synthSSOSecret || !tr.owners[synthOwner] {
		t.Fatalf("trust = %+v ok=%v", tr, ok)
	}
	writeOutpostConfigAt(t, map[string]any{"mcp_bearer_token": synthHostToken, "admin_users": []any{"A@x.test", " b@x.test "}, "oauth2_email": synthOwner})
	tr, _ = loadOutpostTrust("apps")
	if tr.owners[synthOwner] || !tr.owners["a@x.test"] || !tr.owners["b@x.test"] {
		t.Fatalf("owners = %v", tr.owners)
	}
	if _, ok := loadOutpostTrust("apps"); !ok {
		t.Fatal("config unexpectedly unreadable")
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "matrix", "agent.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadOutpostTrust("apps"); ok {
		t.Fatal("junk config accepted")
	}
}

func mustPort(t *testing.T, rawURL string) int {
	t.Helper()
	_, port, err := splitAdminAddr(strings.TrimPrefix(rawURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func runtimeIsUnix() bool { return os.PathSeparator == '/' }
