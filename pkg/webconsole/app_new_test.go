// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/shape/contain"
	"github.com/qiangli/yoke/pkg/websession"
)

// TestAppNewBlank: `bashy app new` goes through the console's own name check
// (a reserved mount is refused), and the new app's blank page is served to a
// signed-in principal like any registered shaped app.
func TestAppNewBlank(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("HOME", filepath.Join(home, "empty-user-home"))
	registry := filepath.Join(home, "registry")

	appNew := func(args ...string) error {
		t.Helper()
		for _, cmd := range fleet.NewAppCmds(fleet.WithRoot(registry), fleet.WithAppValidate(ValidateRegisteredApp)) {
			if cmd.Name() != "new" {
				continue
			}
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(args)
			return cmd.Execute()
		}
		t.Fatal("app new command is not registered")
		return nil
	}
	if err := appNew(consoleBaseMount); err == nil {
		t.Fatalf("app new %q (a reserved mount) was accepted", consoleBaseMount)
	}
	if err := appNew("signup", "--label", "Sign-up sheet"); err != nil {
		t.Fatalf("app new: %v", err)
	}

	app, ok := fleet.New(fleet.WithRoot(registry), fleet.WithoutCloudOverlay()).App("signup")
	if !ok || !app.IsShaped() {
		t.Fatalf("registered = %+v, %v", app, ok)
	}
	panel := Panel{Name: app.Name, Label: app.Label, Path: "/signup/", ShapedSource: app.Source,
		Auth: AuthSystem, Source: "registered", Available: true}
	pairPath := filepath.Join(home, "pairing.json")
	sessions := websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt"))
	h := newTestHandler(t, Options{
		RequireLogin: true, Sessions: sessions, Pairing: true, PairStorePath: pairPath,
		Panels: []Panel{panel}, Shape: ShapeOptions{Home: home, Runner: []string{"/bin/sh"}, Level: contain.LevelProcess},
	})
	_, secret, err := newPairStore(pairPath).issueTicketForPrincipal([]string{"signup"}, time.Hour, time.Minute, "alice")
	if err != nil {
		t.Fatal(err)
	}
	const peer = "192.0.2.10:5555"
	redeemed := do(h, http.MethodGet, pairRedeemPath+"?v="+pairQRVersion+"&t="+url.QueryEscape(secret), peer, nil)
	var device http.Header
	for _, cookie := range redeemed.Result().Cookies() {
		if cookie.Name == sessionCookie && cookie.Value != "" {
			device = withCookie(cookie)
		}
	}
	if device == nil {
		t.Fatalf("pair redemption = %d without a session cookie", redeemed.Code)
	}
	w := do(h, http.MethodGet, "/signup/", peer, device)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Sign-up sheet") || !strings.Contains(w.Body.String(), "data-bashy-blank") {
		t.Fatalf("blank app page = %d %q", w.Code, w.Body.String())
	}
}

// TestAppNewAPIIsLive: the launcher's "New app" posts here. The blank app is
// listed and served at once (registered apps are read live), and a taken,
// reserved or malformed request is refused.
func TestAppNewAPIIsLive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_APPS_DIR", t.TempDir())
	h := newTestHandler(t, Options{})
	_ = do(h, "GET", "/api/apps", "127.0.0.1:1", nil) // prime the TTL cache

	body := `{"name":"signup","label":"Sign-up sheet"}`
	if w := registryRequest(h, "POST", "/api/apps/new", body, "127.0.0.1:1", nil); w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"path":"/signup/"`) {
		t.Fatalf("new = %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/api/apps", "127.0.0.1:1", nil); !strings.Contains(w.Body.String(), `"name":"signup"`) {
		t.Fatalf("new app not listed: %s", w.Body.String())
	}
	if w := do(h, "GET", "/signup/", "127.0.0.1:1", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "data-bashy-blank") {
		t.Fatalf("new app page = %d %s", w.Code, w.Body.String())
	}
	for _, bad := range []string{body, `{"name":"files"}`, `{"name":"../x"}`, `{"name":"ok","port":8080}`, `{}`} {
		if w := registryRequest(h, "POST", "/api/apps/new", bad, "127.0.0.1:1", nil); w.Code != http.StatusBadRequest {
			t.Errorf("accepted %s: %d %s", bad, w.Code, w.Body.String())
		}
	}
}
