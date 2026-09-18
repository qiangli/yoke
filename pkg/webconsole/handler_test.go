package webconsole

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/coopauth"
)

func newTestHandler(t *testing.T, opts Options) http.Handler {
	t.Helper()
	opts.Ctx = context.Background()
	h, closer, err := Handler(opts)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	t.Cleanup(func() { _ = closer() })
	return h
}

// do issues a request with an explicit peer address, which is what the gate and
// the spoofed-header strip both key on.
func do(h http.Handler, method, path, peer string, hdr http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = peer
	for k, vs := range hdr {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestLoopbackIsUngated(t *testing.T) {
	h := newTestHandler(t, Options{})
	if got := do(h, "GET", "/", "127.0.0.1:5555", nil).Code; got != http.StatusOK {
		t.Fatalf("start page on loopback = %d, want 200", got)
	}
	if got := do(h, "GET", "/healthz", "127.0.0.1:5555", nil).Code; got != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", got)
	}
}

// The nested-prefix trap: mounting the room stamps X-Forwarded-Prefix so the
// SPA gets a correct <base href>, and ArrivedViaCloud is DEFINED as that header
// being present — so meet's own gate would 403 the machine owner on their own
// loopback. It must receive the pass-through gate instead.
func TestMountedRoomDoesNotLockOutTheOwner(t *testing.T) {
	h := newTestHandler(t, Options{})
	w := do(h, "GET", "/meet/api/rooms", "127.0.0.1:5555", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("mounted room on loopback = %d, want 200 (body %q)\n"+
			"the mount's X-Forwarded-Prefix is being read as 'arrived via cloud'",
			w.Code, strings.TrimSpace(w.Body.String()))
	}
}

// A mounted panel must see the CONCATENATED prefix, never a replaced one, or a
// tunnelled console renders every panel with broken asset URLs.
func TestMountComposesPrefixes(t *testing.T) {
	var got string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = coopauth.BaseHref(r) + "|" + r.URL.Path
	})
	h := coopauth.Mount("/meet", inner)

	r := httptest.NewRequest("GET", "/meet/api/rooms", nil)
	r.Header.Set(coopauth.HdrForwardedPrefix, "/matrix/h/laptop/app/console")
	h.ServeHTTP(httptest.NewRecorder(), r)

	want := "/matrix/h/laptop/app/console/meet/|/api/rooms"
	if got != want {
		t.Fatalf("nested mount = %q, want %q", got, want)
	}
}

// The header-spoofing fix: off-loopback, a caller must not be able to vouch for
// themselves. Without the strip, coopauth admits on these headers alone whenever
// no SSO secret is configured — which is the default.
func TestSpoofedVouchFromOffHostIsRefused(t *testing.T) {
	h := newTestHandler(t, Options{})
	w := do(h, "GET", "/api/apps", "203.0.113.9:41234", http.Header{
		coopauth.HdrForwardedPrefix: {"/x"},
		coopauth.HdrRemoteUser:      {"root@example.com"},
		coopauth.HdrRemoteGroups:    {"admin"},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("spoofed vouch from off-host = %d, want 403", w.Code)
	}
}

func TestOffHostWithoutVouchIsRefused(t *testing.T) {
	h := newTestHandler(t, Options{})
	if got := do(h, "GET", "/api/apps", "203.0.113.9:41234", nil).Code; got != http.StatusForbidden {
		t.Fatalf("off-host = %d, want 403", got)
	}
}

func TestAppsListsTheBuiltinSurfaces(t *testing.T) {
	h := newTestHandler(t, Options{})
	w := do(h, "GET", "/api/apps", "127.0.0.1:5555", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("apps = %d", w.Code)
	}
	var got struct {
		Schema string `json:"schema_version"`
		Apps   []struct {
			Name      string `json:"name"`
			Label     string `json:"label"`
			Status    string `json:"status"`
			StartHint string `json:"start_hint"`
			Mode      string `json:"mode"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, w.Body.String())
	}
	if got.Schema != appsSchemaVersion {
		t.Errorf("schema = %q, want %q", got.Schema, appsSchemaVersion)
	}
	seen := map[string]string{}
	labels := map[string]string{}
	for _, a := range got.Apps {
		seen[a.Name] = a.Status
		labels[a.Name] = a.Label
		// A stopped proxy tile must always be able to tell the reader how to
		// start it; a tile that only says "stopped" sends them hunting.
		if a.Mode == "proxy" && a.StartHint == "" {
			t.Errorf("proxy surface %q has no start hint", a.Name)
		}
	}
	for _, want := range []string{"terminal", "meet"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("surface %q missing from /api/apps (have %v)", want, seen)
		}
	}
	if labels["meet"] != "Meet" {
		t.Errorf("meet mount label = %q, want Meet", labels["meet"])
	}
}

func TestCanonicalDeepLinkRedirectsSurvive(t *testing.T) {
	h := newTestHandler(t, Options{})
	for path, want := range map[string]string{
		"/shell": "/term/", "/meet": "/meet/", "/sprint": "/sprint/"} {
		w := do(h, "GET", path, "127.0.0.1:5555", nil)
		if w.Code != http.StatusFound || w.Header().Get("Location") != want {
			t.Errorf("%s -> %d %q, want 302 %q", path, w.Code, w.Header().Get("Location"), want)
		}
	}
}

func TestRetiredWebAliasesAreNotMounted(t *testing.T) {
	h := newTestHandler(t, Options{})
	for _, path := range []string{"/board", "/board/", "/relay", "/relay/"} {
		w := do(h, "GET", path, "127.0.0.1:5555", nil)
		if location := w.Header().Get("Location"); location != "" {
			t.Errorf("retired path %s redirects to %q", path, location)
		}
		if !strings.Contains(w.Body.String(), `id="grid-host"`) {
			t.Errorf("retired path %s was claimed by an app instead of falling through to the launcher", path)
		}
	}
}

// A stopped proxied panel must explain itself rather than leak a dial error.
func TestStoppedProxyPanelExplainsItself(t *testing.T) {
	h := newTestHandler(t, Options{Panels: []Panel{{
		Name: "loom", Label: "Loom", Path: "/loom/", Mode: "proxy",
		Port: 1, Start: []string{"loom", "start"}, Source: "atlas", Available: true,
	}}})
	w := do(h, "GET", "/loom/", "127.0.0.1:5555", nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("stopped proxy = %d, want 502", w.Code)
	}
	if !strings.Contains(w.Body.String(), "bashy loom start") {
		t.Errorf("stopped proxy body lacks the start hint: %q", w.Body.String())
	}
}

// The base href is per-request, not per-build: the same binary is reached at /
// on loopback and under a tunnel prefix.
func TestIndexBaseHrefFollowsTheMount(t *testing.T) {
	h := newTestHandler(t, Options{})
	plain := do(h, "GET", "/", "127.0.0.1:5555", nil).Body.String()
	if !strings.Contains(plain, `<base href="/">`) {
		t.Errorf("loopback index missing <base href=\"/\">")
	}
	// A tunnelled request arrives FROM outpost over loopback and carries a
	// vouched identity; without one the gate refuses it, which is the point of
	// TestSpoofedVouchFromOffHostIsRefused.
	tunnelled := do(h, "GET", "/", "127.0.0.1:5555", http.Header{
		coopauth.HdrForwardedPrefix: {"/matrix/h/laptop/app/console"},
		coopauth.HdrRemoteUser:      {"owner@example.com"},
	})
	if !strings.Contains(tunnelled.Body.String(), `<base href="/matrix/h/laptop/app/console/">`) {
		t.Errorf("tunnelled index has the wrong <base href>")
	}
}

// A tile's Path is what the browser follows; mounting from the panel NAME
// instead served a 404 at the exact link the start page renders.
func TestEveryAvailablePanelIsMountedAtItsAdvertisedPath(t *testing.T) {
	panels := Discover()
	h := newTestHandler(t, Options{Panels: panels})
	for _, p := range panels {
		if !p.Available {
			continue
		}
		w := do(h, "GET", p.Path, "127.0.0.1:5555", nil)
		// An unmounted path does not 404 — it falls through to the launcher's own
		// SPA route and returns the START PAGE, which looks fine in a browser and
		// is exactly how the terminal's /term/ mismatch hid. So assert on what
		// came back, not on the status.
		//
		// The marker is the start page's grid host, not the shared
		// data-bashy-console build stamp: every page the launcher serves carries
		// that stamp, the standalone terminal included.
		if strings.Contains(w.Body.String(), `id="grid-host"`) {
			t.Errorf("panel %q advertises %s but that path fell through to the console start page",
				p.Name, p.Path)
		}
	}
}

// The room is Meet everywhere a PERSON meets it: the tile, the page title, and
// the address bar.
//
// Its command, tile, and mount all use the same public noun.
func TestTheRoomIsMountedUnderItsPublicName(t *testing.T) {
	h := newTestHandler(t, Options{})

	w := do(h, "GET", "/meet/api/rooms", "127.0.0.1:5555", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("/meet/api/rooms = %d, want the room served at its public mount", w.Code)
	}
	for _, p := range findPanels(t, h) {
		if p.Name == "meet" && p.Path != "/meet/" {
			t.Errorf("the room's tile points at %q, not /meet/", p.Path)
		}
	}
}

// --disable must accept the name a person would type.
//
// The internal name and the public mount differ for exactly the renamed apps,
// and matching the name alone made `--disable meet` a SILENT NO-OP: the panel
// an operator asked to withhold stayed listed, routed and reachable. A refusal
// would be recoverable; serving it anyway is not.
func TestDisableAcceptsThePublicMountName(t *testing.T) {
	for _, name := range []string{"meet"} {
		t.Run(name, func(t *testing.T) {
			h := newTestHandler(t, Options{Disable: []string{name}})
			// An unmounted path falls through to the launcher's SPA route, so
			// the test is "this is no longer the ROOM", not "this is a 404" —
			// the same distinction TestEveryAvailablePanelIsMountedAtItsAdvertisedPath
			// draws from the other side.
			if body := do(h, "GET", "/meet/api/rooms", "127.0.0.1:5555", nil).Body.String(); !strings.Contains(body, "bashy app") {
				t.Errorf("--disable %s left the room answering its own API: %.120q", name, body)
			}
			for _, p := range findPanels(t, h) {
				if p.Name == "meet" {
					t.Errorf("--disable %s left the room listed as a tile", name)
				}
			}
		})
	}
}

// findPanels reads the tile list the console actually serves.
func findPanels(t *testing.T, h http.Handler) []Panel {
	t.Helper()
	w := do(h, "GET", "/api/apps", "127.0.0.1:5555", nil)
	var got struct {
		Apps []Panel `json:"apps"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode /api/apps: %v", err)
	}
	return got.Apps
}
