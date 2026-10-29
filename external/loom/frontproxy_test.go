package loom

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// S515.1 live round 5: `/app/loom/` on the satellite showed Gitea's anonymous
// Sign In page. The translating front proxy was never in the request path —
// the app routed straight at Gitea's own port, and the supervised
// `bashy loom serve` ran Gitea alone. These are the loom-side invariants of
// the fix; the unit-spec half is pinned in pkg/fleet.
//
// Why Gitea may never bind a routable address: app.ini turns
// ENABLE_REVERSE_PROXY_AUTHENTICATION on unconditionally, so on that listener
// an `X-WEBAUTH-USER: admin` header IS a full login. Only the front proxy —
// which strips client-supplied X-WEBAUTH-* before stamping a vouched identity
// — may be reachable, and REVERSE_PROXY_TRUSTED_PROXIES stays loopback-only so
// Gitea refuses the translated headers from anywhere else.

func TestGiteaBindAddrIsAlwaysLoopback(t *testing.T) {
	for in, want := range map[string]string{
		"":              "127.0.0.1",
		"127.0.0.1":     "127.0.0.1",
		"127.0.0.2":     "127.0.0.2",
		"::1":           "::1",
		"0.0.0.0":       "127.0.0.1",
		"::":            "127.0.0.1",
		"192.168.1.10":  "127.0.0.1",
		"10.0.0.7":      "127.0.0.1",
		"localhost":     "127.0.0.1",
		"lan":           "127.0.0.1",
		"sat-3a3cc3.ts": "127.0.0.1",
	} {
		if got := giteaBindAddr(in); got != want {
			t.Errorf("giteaBindAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnsureConfigPinsGiteaToLoopbackAndTrustsOnlyLoopbackProxies(t *testing.T) {
	dir := t.TempDir()
	// A unit (or an operator) asking for a routable bind gets a loopback Gitea:
	// the front proxy is what binds the routable address.
	cfg, err := ensureConfig(dir, "0.0.0.0", 31880, "https://ai.dhnt.io/matrix/h/sat/app/loom/", true, nil)
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	seeded, _ := os.ReadFile(cfg)
	if got := sectionValue(string(seeded), "server", "HTTP_ADDR"); got != "127.0.0.1" {
		t.Fatalf("seeded HTTP_ADDR = %q, want 127.0.0.1", got)
	}
	if got := sectionValue(string(seeded), "service", "REVERSE_PROXY_TRUSTED_PROXIES"); got != "127.0.0.0/8,::1/128" {
		t.Fatalf("seeded REVERSE_PROXY_TRUSTED_PROXIES = %q, want loopback only", got)
	}

	// An app.ini left behind by an older build (or hand-edited) is reconciled
	// back to loopback on the next start, not inherited.
	opened := strings.Replace(string(seeded), "HTTP_ADDR = 127.0.0.1", "HTTP_ADDR = 0.0.0.0", 1)
	if opened == string(seeded) {
		t.Fatal("test setup: HTTP_ADDR not found")
	}
	if err := os.WriteFile(filepath.Join(dir, "app.ini"), []byte(opened), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureConfig(dir, "192.168.1.10", 31880, "https://ai.dhnt.io/matrix/h/sat/app/loom/", true, nil); err != nil {
		t.Fatal(err)
	}
	reconciled, _ := os.ReadFile(cfg)
	if got := sectionValue(string(reconciled), "server", "HTTP_ADDR"); got != "127.0.0.1" {
		t.Fatalf("reconciled HTTP_ADDR = %q, want 127.0.0.1", got)
	}
	if got := sectionValue(string(reconciled), "service", "REVERSE_PROXY_TRUSTED_PROXIES"); got != "127.0.0.0/8,::1/128" {
		t.Fatalf("reconciled REVERSE_PROXY_TRUSTED_PROXIES = %q, want loopback only", got)
	}
}

func TestServeRunsTheFrontProxyItself(t *testing.T) {
	// A service unit supervises ONE foreground process, so the argv it runs
	// has to own the proxy: `loom start`'s detached proxy child is not in play.
	cmd := NewLoomCmd()
	var serve *cobra.Command
	for _, c := range cmd.Commands() {
		if c.Name() == "serve" {
			serve = c
		}
	}
	if serve == nil {
		t.Fatal("no serve command")
	}
	if serve.Flags().Lookup("proxy-port") == nil {
		t.Fatal("`loom serve` has no --proxy-port flag, so a service unit cannot put the front proxy in the path")
	}
}

func TestServeProxyTranslatesIdentityOnItsOwnListener(t *testing.T) {
	var gotUser, gotEmail, gotName string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.Header.Get("X-WEBAUTH-USER")
		gotEmail = r.Header.Get("X-WEBAUTH-EMAIL")
		gotName = r.Header.Get("X-WEBAUTH-FULLNAME")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveProxyListener(ctx, ln, upstream.URL, "", true) }()

	req, _ := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/", nil)
	req.Header.Set("Remote-User", "alice@example.com")
	req.Header.Set("Remote-Email", "alice@example.com")
	req.Header.Set("Remote-Name", "Alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("front proxy on its own listener: %v", err)
	}
	_ = resp.Body.Close()
	if gotEmail != "alice@example.com" || gotName != "Alice" || gotUser != "alice-example.com" {
		t.Fatalf("translated headers = (user=%q,email=%q,name=%q)", gotUser, gotEmail, gotName)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveProxyListener: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveProxyListener did not stop with its context")
	}
}

func TestProxyStripsSpoofedWebauthFromAnUnvouchedClient(t *testing.T) {
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	handler, err := loomProxyHandler(upstream.URL, "", true) // auto_provision ON: the widest surface
	if err != nil {
		t.Fatal(err)
	}
	// A non-loopback client with NO cloudbox vouch: not the on-host owner, so
	// it gets no identity, and its own X-WEBAUTH-* must not survive.
	req := httptest.NewRequest(http.MethodGet, "http://loom.invalid/", nil)
	req.RemoteAddr = "203.0.113.9:54321"
	req.Header.Set("X-WEBAUTH-USER", "admin")
	req.Header.Set("X-WEBAUTH-EMAIL", "admin@localhost")
	req.Header.Set("X-WEBAUTH-FULLNAME", "admin")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	for _, h := range []string{"X-WEBAUTH-USER", "X-WEBAUTH-EMAIL", "X-WEBAUTH-FULLNAME"} {
		if v := got.Get(h); v != "" {
			t.Fatalf("spoofed %s reached gitea as %q", h, v)
		}
	}
}
