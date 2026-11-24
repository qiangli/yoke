package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet/cloudboxfake"
)

func TestParseSatelliteAppURL(t *testing.T) {
	ep, err := ParseSatelliteAppURL("https://sat.example.com/matrix/h/studio/app/site/mcp/")
	if err != nil {
		t.Fatalf("ParseSatelliteAppURL: %v", err)
	}
	if ep.URL != "https://sat.example.com/matrix/h/studio/app/site/mcp" ||
		ep.Satellite != "https://sat.example.com" || ep.Host != "studio" || ep.App != "site" {
		t.Fatalf("parsed = %+v", ep)
	}

	loop, err := ParseSatelliteAppURL("http://127.0.0.1:8080/matrix/h/box/app/web/mcp")
	if err != nil || loop.Satellite != "http://127.0.0.1:8080" || loop.Host != "box" || loop.App != "web" {
		t.Fatalf("loopback = %+v, %v", loop, err)
	}

	for _, bad := range []string{
		"",
		"http://sat.example.com/matrix/h/studio/app/site/mcp",
		"https://user@sat.example.com/matrix/h/studio/app/site/mcp",
		"https://sat.example.com/matrix/h/studio/app/site/mcp?x=1",
		"https://sat.example.com/matrix/h/studio/app/site/mcp#frag",
		"https://sat.example.com/matrix/h/studio/other/site/mcp",
		"https://sat.example.com/matrix/h/../app/site/mcp",
		"sat.example.com/matrix/h/studio/app/site/mcp",
	} {
		if _, err := ParseSatelliteAppURL(bad); err == nil {
			t.Fatalf("accepted invalid URL %q", bad)
		}
	}
}

func TestSatelliteExchangeAgainstCloudboxFakeAndRefresh(t *testing.T) {
	f := cloudboxfake.New(t)
	now := time.Date(2026, 11, 24, 23, 0, 0, 0, time.UTC)
	f.SetNow(func() time.Time { return now })

	ownerTok := f.AddOwner("owner@example.com")
	memberTok := f.AddOwner("member@example.com")
	outsiderTok := f.AddOwner("outsider@example.com")
	narrowTok := f.AddToken("member@example.com", cloudboxfake.ScopeMembersRead)

	satID := f.AddSatellite(t, "owner@example.com", "zoo")
	satURL := f.SatelliteURL(satID)

	// Pair owner's host and make member@example.com an active mutual member.
	postJSON := func(base, path, tok string, body any) (int, map[string]any) {
		t.Helper()
		var rdr *strings.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = strings.NewReader(string(b))
		} else {
			rdr = strings.NewReader("")
		}
		req, err := http.NewRequest(http.MethodPost, base+path, rdr)
		if err != nil {
			t.Fatal(err)
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	_, pairCode := postJSON(f.URL(), "/api/v1/hosts/invites", ownerTok, nil)
	postJSON(f.URL(), "/api/v1/hosts/pair", "", map[string]string{"code": pairCode["code"].(string), "host": "studio"})
	_, addRes := postJSON(f.URL(), "/api/v1/members", ownerTok, map[string]string{"email": "member@example.com", "circle": "team"})
	invID := addRes["invitation"].(map[string]any)["id"].(string)
	postJSON(f.URL(), "/api/v1/member/invitations/"+invID+"/accept", memberTok, map[string]string{"circle": "team"})

	// Outsider and narrow token are refused on exchange; generic 403 error, never leaking token.
	for _, tc := range []struct {
		name, tok string
	}{
		{"outsider", outsiderTok},
		{"narrow-scope", narrowTok},
	} {
		_, err := (SatelliteClient{BaseURL: satURL, Token: tc.tok}).Exchange(context.Background(), []string{"host:proxy"}, "10m")
		if err == nil {
			t.Fatalf("%s exchange unexpectedly succeeded", tc.name)
		}
		var pe *ProvisionError
		if !errors.As(err, &pe) || pe.Status != http.StatusForbidden {
			t.Fatalf("%s err = %v, want 403 ProvisionError", tc.name, err)
		}
		if strings.Contains(err.Error(), "host:invite-mint") {
			t.Fatalf("generic exchange error still mentions host:invite-mint: %v", err)
		}
		if strings.Contains(err.Error(), tc.tok) {
			t.Fatalf("%s token leaked in error: %v", tc.name, err)
		}
	}

	// Member cannot escalate to owner-only host:invite-mint.
	if _, err := (SatelliteClient{BaseURL: satURL, Token: memberTok}).Exchange(context.Background(), []string{"host:invite-mint"}, "10m"); err == nil {
		t.Fatal("member exchanged host:invite-mint")
	}

	// Member exchanges host:proxy and refreshes before/past TTL.
	src := &SatelliteTokenSource{
		Client: SatelliteClient{BaseURL: satURL, Token: memberTok},
		Scopes: []string{"host:proxy"},
		TTL:    "10m",
		Now:    func() time.Time { return now },
	}
	ex1, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("first exchange: %v", err)
	}
	if ex1.Token == "" || ex1.Token == memberTok || !ex1.ExpiresAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("unexpected exchange result: %+v", ex1)
	}

	// Within lifetime (5m in), cached token is reused.
	now = now.Add(5 * time.Minute)
	exCached, err := src.Token(context.Background())
	if err != nil || exCached.Token != ex1.Token {
		t.Fatalf("cached token = %+v, %v; want %s", exCached, err, ex1.Token)
	}

	// Within pre-expiry refresh window (9m40s in, 20s before 10m expiry), refreshed proactively.
	now = now.Add(4*time.Minute + 40*time.Second)
	ex2, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("proactive refresh: %v", err)
	}
	if ex2.Token == ex1.Token {
		t.Fatalf("token was not refreshed before expiry: still %s", ex2.Token)
	}

	// Past original 10m TTL, first token is expired on satellite while refreshed token works once shared.
	now = now.Add(1 * time.Minute)
	var seenUsers []string
	f.MountApp(satID, "studio", "site", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUsers = append(seenUsers, r.Header.Get("X-Periscope-User"))
		w.WriteHeader(http.StatusOK)
	}))

	callApp := func(bearer string) int {
		req, _ := http.NewRequest(http.MethodGet, satURL+"/matrix/h/studio/app/site/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Before share: 403 not_shared.
	if got := callApp(ex2.Token); got != http.StatusForbidden {
		t.Fatalf("unshared app status = %d, want 403", got)
	}
	// Share app with team circle.
	postJSON(f.URL(), "/api/v1/hosts/studio/shares", ownerTok, map[string]any{"app": "site", "circles": []string{"team"}})
	// Expired ex1 is 401; refreshed ex2 is 200 and forwards member identity; raw account token is 403/401.
	if got := callApp(ex1.Token); got != http.StatusUnauthorized {
		t.Fatalf("expired token status = %d, want 401", got)
	}
	if got := callApp(memberTok); got == http.StatusOK {
		t.Fatalf("raw master token must not pass satellite app route without exchange")
	}
	if got := callApp(ex2.Token); got != http.StatusOK {
		t.Fatalf("refreshed token status = %d, want 200", got)
	}
	if len(seenUsers) != 1 || seenUsers[0] != "member@example.com" {
		t.Fatalf("forwarded users = %v, want [member@example.com]", seenUsers)
	}
}

func TestSatelliteExchangeNeverEchoesPlaintextBodyOrToken(t *testing.T) {
	const secret = "account-secret-sentinel-777"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "echo "+secret, http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	_, err := (SatelliteClient{BaseURL: srv.URL, Token: secret}).Exchange(context.Background(), []string{"host:proxy"}, "10m")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("secret leaked in error: %v", err)
	}
}
