// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/websession"
)

const synthHostToken = "synthetic-outpost-bearer-0123456789abcdef"

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

// newHostUpstream stands in for outpost's admin listener and records the path
// and Authorization of every request that reaches it.
func newHostUpstream(t *testing.T, token string, ok bool) *hostUpstream {
	t.Helper()
	u := &hostUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.seen = append(u.seen, seenReq{r.URL.Path, r.Header.Get("Authorization")})
		u.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(u.Close)
	t.Setenv("OUTPOST_ADMIN_ADDR", strings.TrimPrefix(u.URL, "http://"))
	withOutpostAdminResolver(t, outpostConfig{}, nil, "127.0.0.1:17777")
	prev := hostAdminToken
	hostAdminToken = func() (string, bool) { return token, ok }
	t.Cleanup(func() { hostAdminToken = prev })
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

func TestHostAdminBearerIsAttachedForTheLoopbackOperator(t *testing.T) {
	u := newHostUpstream(t, synthHostToken, true)
	h := newTestHandler(t, Options{})
	hdr := http.Header{"Authorization": {"Bearer browser-supplied"}}
	w := do(h, "GET", "/host/api/status", "127.0.0.1:5555", hdr)
	if w.Code != 200 {
		t.Fatalf("host api = %d: %s", w.Code, w.Body.String())
	}
	if got, _ := authOf(t, u, "/api/status"); got != "Bearer "+synthHostToken {
		t.Fatalf("upstream Authorization = %q, want the host bearer", got)
	}
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

func TestHostAdminBearerStaysOnTheAdminAPI(t *testing.T) {
	u := newHostUpstream(t, synthHostToken, true)
	h := newTestHandler(t, Options{})
	hdr := http.Header{"Authorization": {"Bearer browser-supplied"}}
	for _, p := range []string{"/host/", "/host/mcp/", "/host/_periscope/x", "/host/somapp/api/status", "/host/api"} {
		do(h, "GET", p, "127.0.0.1:5555", hdr)
	}
	// Dot segments and doubled slashes must not pivot onto another surface.
	do(h, "GET", "/host/api/%2e%2e/mcp/", "127.0.0.1:5555", hdr)
	do(h, "GET", "/host/api//..//mcp/", "127.0.0.1:5555", hdr)
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

func TestHostAdminBearerAbsentOrUnreadableTokenSendsNothing(t *testing.T) {
	u := newHostUpstream(t, "", false)
	h := newTestHandler(t, Options{})
	hdr := http.Header{"Authorization": {"Bearer browser-supplied"}}
	if w := do(h, "GET", "/host/api/status", "127.0.0.1:5555", hdr); w.Code != 200 {
		t.Fatalf("host api = %d", w.Code)
	}
	if got, seen := authOf(t, u, "/api/status"); !seen || got != "" {
		t.Fatalf("Authorization = %q (seen=%v), want none — not even the browser's", got, seen)
	}
}

func TestHostAdminBearerNeverGoesToAnExplicitRemoteEndpoint(t *testing.T) {
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
	// The address configured through outpost itself is a source too.
	t.Setenv("OUTPOST_ADMIN_ADDR", "")
	withOutpostAdminResolver(t, outpostConfig{AdminAddr: "10.0.0.5:17777"}, nil, "127.0.0.1:17777")
	if p := hostBuiltinPanel(); p.hostAdmin {
		t.Error("remote admin_addr from outpost config is eligible for the credential")
	}
}

func TestHostAdminBearerRequiresTheStockHostPanel(t *testing.T) {
	u := newHostUpstream(t, synthHostToken, true)
	port := mustPort(t, u.URL)
	// An --app / registered panel on the very same loopback port is not Host.
	h := newTestHandler(t, Options{Panels: []Panel{
		{Name: "other", Label: "Other", Path: "/other/", Mode: "proxy", Port: port, Available: true, Auth: AuthSystem, Source: "app"},
	}})
	if w := do(h, "GET", "/other/api/status", "127.0.0.1:5555", nil); w.Code != 200 {
		t.Fatalf("other = %d", w.Code)
	}
	if got, seen := authOf(t, u, "/api/status"); !seen || got != "" {
		t.Fatalf("third-party panel received Authorization %q", got)
	}
}

func TestHostAdminBearerDeniedWhereTheLauncherGateDenies(t *testing.T) {
	u := newHostUpstream(t, synthHostToken, true)

	// Login-required console, no session.
	locked := newTestHandler(t, Options{RequireLogin: true,
		Sessions: websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt"))})
	for _, peer := range []string{"127.0.0.1:5555", "192.168.1.44:5555"} {
		if w := do(locked, "GET", "/host/api/status", peer, http.Header{"Accept": {"application/json"}}); w.Code != http.StatusForbidden {
			t.Errorf("login-required, no session, peer %s = %d, want 403", peer, w.Code)
		}
	}
	// Ungated console reached from a non-loopback peer.
	open := newTestHandler(t, Options{})
	if w := do(open, "GET", "/host/api/status", "192.168.1.44:5555", nil); w.Code != http.StatusForbidden {
		t.Errorf("non-loopback peer = %d, want 403", w.Code)
	}
	// A forged session cookie is not a session.
	if w := do(locked, "GET", "/host/api/status", "127.0.0.1:5555", http.Header{"Cookie": {sessionCookie + "=forged"}}); w.Code != http.StatusForbidden {
		t.Errorf("forged cookie = %d, want 403", w.Code)
	}
	if n := len(u.requests()); n != 0 {
		t.Fatalf("%d request(s) reached outpost despite the gate denying", n)
	}
}

func TestHostAdminBearerOnlyForDirectLoopbackPeerWithSession(t *testing.T) {
	u := newHostUpstream(t, synthHostToken, true)
	sessions := websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt"))
	h := newTestHandler(t, Options{RequireLogin: true, Sessions: sessions})
	cookie, err := sessions.Mint("operator")
	if err != nil {
		t.Fatal(err)
	}
	hdr := http.Header{"Cookie": {sessionCookie + "=" + cookie}}

	if w := do(h, "GET", "/host/api/status", "127.0.0.1:5555", hdr); w.Code != 200 {
		t.Fatalf("operator session on loopback = %d", w.Code)
	}
	if got, _ := authOf(t, u, "/api/status"); got != "Bearer "+synthHostToken {
		t.Fatalf("loopback operator Authorization = %q", got)
	}

	// Same valid session from the LAN: the gate admits, the credential is not
	// presented — the request did not originate on this machine.
	u.mu.Lock()
	u.seen = nil
	u.mu.Unlock()
	if w := do(h, "GET", "/host/api/status", "192.168.1.44:5555", hdr); w.Code != 200 {
		t.Fatalf("operator session from LAN = %d", w.Code)
	}
	if got, seen := authOf(t, u, "/api/status"); !seen || got != "" {
		t.Fatalf("LAN peer got Authorization %q", got)
	}
}

func TestHostAdminBearerNotPresentedViaCloudOrForSpoofedTrustHeaders(t *testing.T) {
	u := newHostUpstream(t, synthHostToken, true)
	h := newTestHandler(t, Options{})
	// A tunnel-arrived request is vouched by outpost, not by the local-operator
	// rows; the credential is not part of that path.
	hdr := http.Header{"X-Forwarded-Prefix": {"/matrix/h/x/app/console"}, "X-Forwarded-For": {"127.0.0.1"}}
	do(h, "GET", "/host/api/status", "127.0.0.1:5555", hdr)
	for _, r := range u.requests() {
		if r.Auth != "" {
			t.Fatalf("credential attached to a cloud-arrived request: %q", r.Auth)
		}
	}
}

func TestHostAdminBearerDeviceSessions(t *testing.T) {
	u := newHostUpstream(t, synthHostToken, true)
	h, store := pairEnv(t)
	lan := "192.168.1.44:5555"

	// Default scope does not include Host: refused by the gate, never forwarded.
	cookie := redeemScan(t, h, store, nil, time.Hour)
	if w := do(h, "GET", "/host/api/status", lan, withCookie(cookie)); w.Code != http.StatusForbidden {
		t.Fatalf("out-of-scope device = %d, want 403", w.Code)
	}
	// A device paired for Host is admitted — but it is not the operator, so the
	// local credential is never presented for it, even from a loopback peer.
	hostDev := redeemScan(t, h, store, []string{"host"}, time.Hour)
	if w := do(h, "GET", "/host/api/status", "127.0.0.1:5555", withCookie(hostDev)); w.Code != 200 {
		t.Fatalf("in-scope device = %d: %s", w.Code, w.Body.String())
	}
	if got, seen := authOf(t, u, "/api/status"); !seen || got != "" {
		t.Fatalf("paired device received Authorization %q", got)
	}
	u.mu.Lock()
	u.seen = nil
	u.mu.Unlock()
	// Revoked / expired devices are refused.
	if _, err := store.revokeAll(); err != nil {
		t.Fatal(err)
	}
	if w := do(h, "GET", "/host/api/status", lan, withCookie(cookie)); w.Code != http.StatusForbidden {
		t.Fatalf("revoked device = %d, want 403", w.Code)
	}
	expired := redeemScan(t, h, store, nil, 50*time.Millisecond)
	time.Sleep(80 * time.Millisecond)
	if w := do(h, "GET", "/host/api/status", lan, withCookie(expired)); w.Code != http.StatusForbidden {
		t.Fatalf("expired device = %d, want 403", w.Code)
	}
	if n := len(u.requests()); n != 0 {
		t.Fatalf("%d request(s) reached outpost for denied devices", n)
	}
}

func TestHostAdminTokenFileSafety(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "agent.json")
	write := func(p, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(p, mode)
	}
	write(good, `{"mcp_bearer_token":"`+synthHostToken+`"}`, 0o600)
	if tok, ok := readMCPTokenFile(good); !ok || tok != synthHostToken {
		t.Fatalf("0600 file: ok=%v", ok)
	}
	// Rotation is picked up (mtime/size change).
	write(good, `{"mcp_bearer_token":"rotated-`+synthHostToken+`"}`, 0o600)
	if tok, _ := readMCPTokenFile(good); tok != "rotated-"+synthHostToken {
		t.Fatalf("rotated token not re-read: %q", tok)
	}
	for name, body := range map[string]string{
		"short":   `{"mcp_bearer_token":"abc"}`,
		"crlf":    `{"mcp_bearer_token":"aaaaaaaaaaaaaaaaaaaa\r\nX-Evil: 1"}`,
		"missing": `{}`,
		"junk":    `not json`,
	} {
		p := filepath.Join(dir, name+".json")
		write(p, body, 0o600)
		if _, ok := readMCPTokenFile(p); ok {
			t.Errorf("%s: unusable token accepted", name)
		}
	}
	if _, ok := readMCPTokenFile(filepath.Join(dir, "nope.json")); ok {
		t.Error("missing file accepted")
	}
	if runtimeIsUnix() {
		open := filepath.Join(dir, "open.json")
		write(open, `{"mcp_bearer_token":"`+synthHostToken+`"}`, 0o644)
		if _, ok := readMCPTokenFile(open); ok {
			t.Error("group/world-readable config accepted")
		}
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
