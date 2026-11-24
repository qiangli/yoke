package fleet

import (
	"context"
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

	account := f.AddOwner("owner@example.com")
	sat := f.AddSatellite(t, "owner@example.com", "test")
	source := SatelliteTokenSource{
		Client: SatelliteClient{BaseURL: f.SatelliteURL(sat), Token: account},
		Now:    func() time.Time { return now },
	}
	first, err := source.Token(context.Background())
	if err != nil || first == "" || first == account {
		t.Fatal("expected an exchanged token", err)
	}
	now = now.Add(5 * time.Minute)
	if tok, err := source.Token(context.Background()); err != nil || tok != first {
		t.Fatal("expected cached token", err)
	}
	now = now.Add(4*time.Minute + 40*time.Second)
	if tok, err := source.Token(context.Background()); err != nil || tok == first {
		t.Fatal("expected refresh before expiry", err)
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
