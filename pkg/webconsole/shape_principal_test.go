// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/shape"
	"github.com/qiangli/yoke/pkg/websession"
)

// TestShapedAppPrincipalsStayIsolated is the focused S525.A4 proof: one
// launcher serves two named people without trusting a browser-supplied identity,
// and the state/data plane uses the same principal boundary.
func TestShapedAppPrincipalsStayIsolated(t *testing.T) {
	home := t.TempDir()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.Header.Get(PrincipalHeader)
		_, _ = w.Write([]byte(p))
	}))
	t.Cleanup(up.Close)
	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}

	pairPath := filepath.Join(home, "pairing.json")
	sessions := websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt"))
	h := newTestHandler(t, Options{
		RequireLogin: true, Sessions: sessions, Pairing: true, PairStorePath: pairPath,
		Panels: []Panel{{Name: "notes", Label: "Notes", Path: "/notes/", Mode: atlas.WebProxy,
			Port: port, Auth: AuthSystem, Source: "app", Available: true}},
	})
	store := newPairStore(pairPath)

	paired := func(name string) *http.Cookie {
		t.Helper()
		_, secret, err := store.issueTicketForPrincipal([]string{"notes"}, time.Hour, time.Minute, name)
		if err != nil {
			t.Fatal(err)
		}
		w := do(h, http.MethodGet, pairRedeemPath+"?v="+pairQRVersion+"&t="+url.QueryEscape(secret), "192.0.2.10:5555", nil)
		for _, c := range w.Result().Cookies() {
			if c.Name == sessionCookie && c.Value != "" {
				return c
			}
		}
		t.Fatalf("pair %q = %d, no session cookie", name, w.Code)
		return nil
	}
	alice, bob := paired("alice"), paired("bob")

	request := func(cookie *http.Cookie, forged string) string {
		t.Helper()
		head := withCookie(cookie)
		if forged != "" {
			head.Set(PrincipalHeader, forged)
		}
		w := do(h, http.MethodGet, "/notes/", "192.0.2.10:5555", head)
		if w.Code != http.StatusOK {
			t.Fatalf("notes = %d: %s", w.Code, w.Body.String())
		}
		return strings.TrimSpace(w.Body.String())
	}
	if got := request(alice, "device:mallory"); got != "device:alice" {
		t.Fatalf("alice principal = %q", got)
	}
	if got := request(bob, "device:alice"); got != "device:bob" {
		t.Fatalf("bob principal = %q", got)
	}
	operatorValue, err := sessions.Mint(currentOSUser())
	if err != nil {
		t.Fatal(err)
	}
	if got := request(&http.Cookie{Name: sessionCookie, Value: operatorValue}, "cloud:forged"); got != "os:"+currentOSUser() {
		t.Fatalf("operator principal = %q", got)
	}
	cloud := http.Header{
		"X-Forwarded-Prefix": {"/cloud/apps"},
		"Remote-User":        {"alice@example.test"},
		"Remote-Email":       {"alice@example.test"},
		PrincipalHeader:      {"cloud:mallory@example.test"},
	}
	if got := strings.TrimSpace(do(h, http.MethodGet, "/notes/", "127.0.0.1:5555", cloud).Body.String()); got != "cloud:alice@example.test" {
		t.Fatalf("cloud principal = %q", got)
	}
	cloud.Set("Remote-Email", "bob@example.test")
	if got := strings.TrimSpace(do(h, http.MethodGet, "/notes/", "127.0.0.1:5555", cloud).Body.String()); got != "" {
		t.Fatalf("ambiguous cloud identity exposed principal %q", got)
	}

	for principal, rev := range map[string]string{"device:alice": "alice-rev", "device:bob": "bob-rev"} {
		s, err := shape.OpenAt(home, "notes", principal)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Write(shape.State{ActiveRevision: rev, LastKnownGood: "base"}); err != nil {
			t.Fatal(err)
		}
		if err := s.WriteData("note.txt", []byte(principal)); err != nil {
			t.Fatal(err)
		}
	}
	for principal, want := range map[string]string{"device:alice": "alice-rev", "device:bob": "bob-rev"} {
		s, _ := shape.OpenAt(home, "notes", principal)
		got, err := s.Read()
		if err != nil || got.ActiveRevision != want {
			t.Fatalf("%s state = %+v, %v", principal, got, err)
		}
		data, err := s.ReadData("note.txt")
		if err != nil || string(data) != principal {
			t.Fatalf("%s data = %q, %v", principal, data, err)
		}
	}

	// A legacy pairing has no chosen principal. It is still scoped/authenticated,
	// but shaping fails closed and the app receives no identity header.
	_, secret, err := store.issueTicket([]string{"notes"}, time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	w := do(h, http.MethodGet, pairRedeemPath+"?t="+url.QueryEscape(secret), "192.0.2.11:5555", nil)
	var unnamed *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			unnamed = c
		}
	}
	if got := request(unnamed, "device:mallory"); got != "" {
		t.Fatalf("unnamed pairing exposed principal %q", got)
	}

	metadataPath := filepath.Join(home, "shape.yaml")
	if err := os.WriteFile(metadataPath, []byte("schema_version: bashy-shape-feature-v1\nid: compact\norigin: device:alice\nbase_app: notes\nrevision: a1\nconflicts: [wide]\nscope: [user]\nschema_range: {min: 1, max: 2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	compact, err := shape.ReadFeature(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	features := []shape.Feature{
		compact,
		{SchemaVersion: shape.FeatureSchemaV1, ID: "wide", Origin: "device:bob", BaseApp: "notes", Revision: "b1", Schema: shape.SchemaRange{Min: 2, Max: 3}},
	}
	if err := shape.CheckCompatible(features); err == nil {
		t.Fatal("conflicting features reported compatible")
	}
}
