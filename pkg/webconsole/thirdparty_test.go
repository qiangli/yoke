// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/atlas"
)

// upstream stands in for a third-party app. It records what the proxy sent and
// answers a root-relative redirect, which is the shape that escapes a mount.
type upstream struct {
	srv  *httptest.Server
	port int
	last http.Header
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.last = r.Header.Clone()
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/home", http.StatusFound)
		case "/absolute":
			http.Redirect(w, r, "https://idp.example/login", http.StatusFound)
		default:
			_, _ = w.Write([]byte("upstream ok"))
		}
	}))
	t.Cleanup(u.srv.Close)
	pu, err := url.Parse(u.srv.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	u.port, err = strconv.Atoi(pu.Port())
	if err != nil {
		t.Fatalf("upstream port: %v", err)
	}
	return u
}

func appPanel(name string, port int, auth string) Panel {
	return Panel{
		Name: name, Label: strings.ToUpper(name[:1]) + name[1:],
		Path: "/" + name + "/", Mode: atlas.WebProxy, Port: port,
		Auth: auth, Source: "app", Available: true,
		Icon: "M4 8a2 2 0 0 1 2-2z", Tip: name + " tooltip",
	}
}

// A public panel opens ITS MOUNT and nothing else. This is the whole security
// property of per-panel tiers: a public tile must never become a hole into the
// Terminal or the console's API.
func TestPublicPanelOpensOnlyItself(t *testing.T) {
	up := newUpstream(t)
	h := newTestHandler(t, Options{
		RequireLogin: true,
		Panels: []Panel{
			appPanel("pub", up.port, AuthPublic),
			appPanel("priv", up.port, AuthSystem),
		},
	})
	const lan = "10.1.2.3:5555"

	if got := do(h, "GET", "/pub/", lan, nil).Code; got != http.StatusOK {
		t.Errorf("public mount from LAN = %d, want 200", got)
	}
	if got := do(h, "GET", "/pub/deep/path", lan, nil).Code; got != http.StatusOK {
		t.Errorf("public subtree from LAN = %d, want 200", got)
	}
	for _, path := range []string{"/priv/", "/api/apps", "/api/session", "/", "/term/"} {
		w := do(h, "GET", path, lan, nil)
		if w.Code == http.StatusOK {
			t.Errorf("%s from LAN = 200; a public panel must not open it", path)
		}
	}
}

// custom = the app runs its own login, so the console must not intercept — and
// a root-relative redirect must be rewritten INTO the mount, or the browser
// lands on the console launcher instead of the app's sign-in page.
func TestCustomPanelRedirectStaysInsideTheMount(t *testing.T) {
	up := newUpstream(t)
	h := newTestHandler(t, Options{
		Panels: []Panel{appPanel("custom", up.port, AuthCustom)},
	})

	w := do(h, "GET", "/custom/redirect", "10.1.2.3:5555", nil)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/custom/home" {
		t.Errorf("Location = %q, want /custom/home", got)
	}

	// Behind the tunnel the prefix composes: console mount + panel mount.
	hdr := http.Header{"X-Forwarded-Prefix": []string{"/matrix/h/x/app/console"}}
	w = do(h, "GET", "/custom/redirect", "127.0.0.1:5555", hdr)
	if got := w.Header().Get("Location"); got != "/matrix/h/x/app/console/custom/home" {
		t.Errorf("tunnelled Location = %q, want /matrix/h/x/app/console/custom/home", got)
	}

	// An absolute URL is the app's deliberate choice (an external IdP); leave it.
	w = do(h, "GET", "/custom/absolute", "127.0.0.1:5555", hdr)
	if got := w.Header().Get("Location"); got != "https://idp.example/login" {
		t.Errorf("absolute Location = %q, want it untouched", got)
	}
}

// The app must be able to build correct absolute URLs from behind two hops.
func TestProxyStampsForwardedHeaders(t *testing.T) {
	up := newUpstream(t)
	h := newTestHandler(t, Options{Panels: []Panel{appPanel("fwd", up.port, AuthPublic)}})

	do(h, "GET", "/fwd/", "127.0.0.1:5555", http.Header{
		"X-Forwarded-Prefix": []string{"/matrix/h/x/app/console"},
		"X-Forwarded-Host":   []string{"ai.dhnt.io"},
		"X-Forwarded-Proto":  []string{"https"},
	})
	if up.last == nil {
		t.Fatal("upstream never saw a request")
	}
	if got := up.last.Get("X-Forwarded-Prefix"); got != "/matrix/h/x/app/console/fwd" {
		t.Errorf("X-Forwarded-Prefix = %q, want the composed mount", got)
	}
	if got := up.last.Get("X-Forwarded-Host"); got != "ai.dhnt.io" {
		t.Errorf("X-Forwarded-Host = %q", got)
	}
	if got := up.last.Get("X-Forwarded-Proto"); got != "https" {
		t.Errorf("X-Forwarded-Proto = %q", got)
	}
}

func decodeMeta(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return m
}

// /meta exists to be polled by something that cannot present an identity.
func TestMetaIsReachableWithoutIdentity(t *testing.T) {
	h := newTestHandler(t, Options{
		RequireLogin: true,
		Panels:       []Panel{appPanel("shown", 9, AuthSystem)},
	})
	const lan = "10.1.2.3:5555"
	if got := do(h, "GET", "/meta", lan, nil).Code; got != http.StatusOK {
		t.Errorf("/meta with requireLogin from LAN = %d, want 200", got)
	}
	if got := do(h, "GET", "/meta/shown", lan, nil).Code; got != http.StatusOK {
		t.Errorf("/meta/shown = %d, want 200", got)
	}
	if got := do(h, "GET", "/meta/shown/deeper", lan, nil).Code; got != http.StatusForbidden {
		t.Errorf("unmatched deep /meta path = %d, want gated 403", got)
	}
	for _, path := range []string{"/meta/.", "/meta/..", "/meta/{shown}", "/meta/%7Bshown%7D"} {
		if got := do(h, "GET", path, lan, nil).Code; got != http.StatusForbidden {
			t.Errorf("%s = %d, want gated 403", path, got)
		}
	}
}

// The projection must not leak host internals, and must stay cacheable.
func TestMetaProjectionOmitsInternalsAndCaches(t *testing.T) {
	h := newTestHandler(t, Options{Panels: []Panel{appPanel("proj", 8123, AuthCustom)}})

	w := do(h, "GET", "/meta/proj", "127.0.0.1:5555", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	m := decodeMeta(t, w.Body.String())
	for _, leaked := range []string{"port", "start", "status", "start_hint", "available"} {
		if _, ok := m[leaked]; ok {
			t.Errorf("/meta leaked %q; the projection is display metadata only", leaked)
		}
	}
	for _, want := range []string{"name", "label", "mount", "path", "auth", "source", "schema_version"} {
		if _, ok := m[want]; !ok {
			t.Errorf("/meta missing %q", want)
		}
	}
	if m["auth"] != AuthCustom {
		t.Errorf("auth = %v, want custom", m["auth"])
	}

	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag; the endpoint exists to be cached")
	}
	w2 := do(h, "GET", "/meta/proj", "127.0.0.1:5555", http.Header{"If-None-Match": []string{etag}})
	if w2.Code != http.StatusNotModified {
		t.Errorf("If-None-Match = %d, want 304", w2.Code)
	}
}

// --disable removes a panel ENTIRELY. Reporting it on /meta would leak the
// existence of a surface that --disable exists to remove.
func TestDisabledPanelIsAbsentFromMeta(t *testing.T) {
	h := newTestHandler(t, Options{
		Panels:  []Panel{appPanel("kept", 9, AuthSystem), appPanel("gone", 9, AuthSystem)},
		Disable: []string{"gone"},
	})
	if got := do(h, "GET", "/meta/gone", "127.0.0.1:5555", nil).Code; got != http.StatusNotFound {
		t.Errorf("/meta/gone = %d, want 404", got)
	}
	body := do(h, "GET", "/meta", "127.0.0.1:5555", nil).Body.String()
	if strings.Contains(body, `"gone"`) {
		t.Errorf("/meta lists a disabled panel: %s", body)
	}
	if !strings.Contains(body, `"kept"`) {
		t.Errorf("/meta dropped the enabled panel: %s", body)
	}
}

// A disabled panel must not leave an auth tier behind that would admit traffic.
func TestDisabledPublicPanelIsNotRoutable(t *testing.T) {
	h := newTestHandler(t, Options{
		RequireLogin: true,
		Panels:       []Panel{appPanel("ghost", 9, AuthPublic)},
		Disable:      []string{"ghost"},
	})
	if got := do(h, "GET", "/ghost/", "10.1.2.3:5555", nil).Code; got == http.StatusOK {
		t.Error("a disabled public panel still admits traffic")
	}
}

// discoverApps must report a bad spec, never drop it silently, and must take
// the fallback rung when the binary does not speak the contract.
func TestDiscoverAppsFallbackAndErrors(t *testing.T) {
	notAnApp := func(context.Context, string) (AppMeta, error) { return AppMeta{}, ErrNotAnApp }
	speaks := func(context.Context, string) (AppMeta, error) {
		return AppMeta{SchemaVersion: MetaSchema, Label: "Declared", Port: 7777, Tip: "hi"}, nil
	}

	// No meta, but a port: still a tile, named for the binary.
	got, errs := discoverApps(context.Background(), []string{"/bin/classgo@8080"}, notAnApp, map[string]bool{}, nil)
	if len(errs) != 0 || len(got) != 1 {
		t.Fatalf("fallback rung: panels=%d errs=%v", len(got), errs)
	}
	if got[0].Name != "classgo" || got[0].Port != 8080 || got[0].Icon != "" {
		t.Errorf("fallback panel = %+v", got[0])
	}

	// No meta and no port: nothing to proxy, so report it.
	got, errs = discoverApps(context.Background(), []string{"/bin/classgo"}, notAnApp, map[string]bool{}, nil)
	if len(got) != 0 || len(errs) != 1 {
		t.Fatalf("portless spec: panels=%d errs=%v", len(got), errs)
	}

	// A declared app keeps its own label and tip.
	got, _ = discoverApps(context.Background(), []string{"/bin/classgo"}, speaks, map[string]bool{}, nil)
	if len(got) != 1 || got[0].Label != "Declared" || got[0].Tip != "hi" || got[0].Port != 7777 {
		t.Fatalf("declared panel = %+v", got)
	}

	// A mount already claimed by a builtin is refused, with a reason.
	_, errs = discoverApps(context.Background(), []string{"/bin/relay@1"}, notAnApp, map[string]bool{"relay": true}, nil)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "already claimed") {
		t.Fatalf("duplicate mount: errs=%v", errs)
	}
}

func TestDiscoverAppsFallsBackOnlyForANonContractBinary(t *testing.T) {
	operational := errors.New("permission denied")
	probe := func(context.Context, string) (AppMeta, error) { return AppMeta{}, operational }
	got, errs := discoverApps(context.Background(), []string{"/bin/classgo@8080"}, probe, map[string]bool{}, nil)
	if len(got) != 0 || len(errs) != 1 || !errors.Is(errs[0], operational) {
		t.Fatalf("operational failure published a panel: got=%#v errs=%v", got, errs)
	}
}

func TestThirdPartyCannotSelfDowngradeAuth(t *testing.T) {
	probe := func(context.Context, string) (AppMeta, error) {
		return AppMeta{SchemaVersion: MetaSchema, Mount: "fixture", Port: 8080, Auth: AuthPublic}, nil
	}
	got, errs := discoverApps(context.Background(), []string{"fixture"}, probe, map[string]bool{}, nil)
	if len(errs) != 0 || len(got) != 1 || got[0].Auth != AuthSystem {
		t.Fatalf("metadata self-downgrade = %#v errs=%v", got, errs)
	}

	got, errs = discoverApps(context.Background(), []string{"fixture"}, probe, map[string]bool{}, map[string]string{"fixture": AuthPublic})
	if len(errs) != 0 || len(got) != 1 || got[0].Auth != AuthPublic {
		t.Fatalf("operator public override = %#v errs=%v", got, errs)
	}
}

func TestThirdPartyStartHintUsesCompleteArgv(t *testing.T) {
	probe := func(context.Context, string) (AppMeta, error) {
		return AppMeta{SchemaVersion: MetaSchema, Mount: "fixture", Port: 8080, Start: []string{"/opt/My App/bin", "serve", "--label=x y"}}, nil
	}
	got, errs := discoverApps(context.Background(), []string{"fixture"}, probe, map[string]bool{}, nil)
	if len(errs) != 0 || len(got) != 1 {
		t.Fatalf("discover = %#v errs=%v", got, errs)
	}
	if hint := got[0].StartHint(); hint != `"/opt/My App/bin" serve "--label=x y"` {
		t.Fatalf("StartHint = %q", hint)
	}
}
