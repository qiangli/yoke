package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet/cloudboxfake"
)

func TestSatelliteWireClient(t *testing.T) {
	ctx := context.Background()
	f := cloudboxfake.New(t)
	owner := f.AddOwner("owner@example.com")
	id := f.AddSatellite(t, "owner@example.com", "garden")
	other := f.AddSatellite(t, "owner@example.com", "other")
	c := OwnerClient{BaseURL: f.URL(), Token: owner}
	var invite struct{ Code string }
	if err := c.request(ctx, "POST", "/api/v1/hosts/invites", nil, &invite); err != nil {
		t.Fatal(err)
	}
	var pair struct{ Token string }
	if err := c.request(ctx, "POST", "/api/v1/hosts/pair", map[string]string{"code": invite.Code, "host": "studio"}, &pair); err != nil {
		t.Fatal(err)
	}
	host := OwnerClient{BaseURL: f.URL(), Token: pair.Token}
	var attach struct {
		AttachToken string `json:"attach_token"`
	}
	if err := host.request(ctx, "POST", "/api/v1/satellites/"+id+"/attach", nil, &attach); err != nil {
		t.Fatal(err)
	}
	rows, err := c.SatelliteAssignments(ctx, id)
	if err != nil || len(rows) != 1 || rows[0].Host != "studio" || rows[0].State != "pending" || rows[0].AttachedAt != nil {
		t.Fatalf("pending assignments: %+v, %v", rows, err)
	}
	if rows, err := c.SatelliteAssignments(ctx, other); err != nil || len(rows) != 0 {
		t.Fatalf("other assignments: %+v, %v", rows, err)
	}
	detail, err := c.Satellite(ctx, id)
	if err != nil || detail.ID != id || detail.OwnerEmail != "owner@example.com" {
		t.Fatalf("detail: %+v, %v", detail, err)
	}
	sat := OwnerClient{BaseURL: detail.Endpoint, Token: attach.AttachToken}
	var redeemed struct{ Token string }
	if err := sat.request(ctx, "POST", "/api/v1/hosts/attach", map[string]string{"host": "studio"}, &redeemed); err != nil {
		t.Fatal(err)
	}
	sat.Token = redeemed.Token
	if detail, err := host.Satellite(ctx, id); err != nil || detail.Attached == nil || !*detail.Attached || len(detail.Hosts) != 0 {
		t.Fatalf("host detail: %+v, %v", detail, err)
	}

	// The public fake owns the new wire endpoints. Supply only the legacy
	// hosts/peers catalogs here, which are outside that fake's surface.
	target, _ := url.Parse(f.URL())
	proxy := httputil.NewSingleHostReverseProxy(target)
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		switch r.URL.Path {
		case "/api/v1/hosts":
			_, _ = w.Write([]byte(`{"hosts":[{"host":"studio"},{"host":"unassigned"}]}`))
		case "/api/v1/peers":
			_, _ = w.Write([]byte(`{"peers":[{"host":"studio","os":"linux"},{"host":"unassigned"}]}`))
		default:
			proxy.ServeHTTP(w, r)
		}
	}))
	defer server.Close()
	out, err := runSatellite(t, "show", id, "--master", server.URL, "--token", owner, "--json")
	if err != nil || !strings.Contains(out, `"studio"`) || len(paths) != 1 || paths[0] != "GET /api/v1/satellites/"+id {
		t.Fatalf("direct show: %s, %v, paths %v", out, err, paths)
	}
	for _, tc := range []struct {
		id    string
		count int
	}{{id, 1}, {other, 0}} {
		cmd := NewPairedHostCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"ls", "--satellite", tc.id, "--master", server.URL, "--token", owner, "--json"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		var got struct {
			Hosts []HostEntry
			Peers []PeerEntry
		}
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Hosts) != tc.count || len(got.Peers) != tc.count || strings.Contains(out.String(), "unassigned") {
			t.Fatalf("filtered catalogs: %s", out.String())
		}
	}
	if err := c.DeleteSelf(ctx); err == nil {
		t.Fatal("owner self-delete must fail")
	}
	if err := host.DeleteSelf(ctx); err != nil {
		t.Fatal(err)
	}
	if err := host.DeleteSelf(ctx); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if rows, err := c.SatelliteAssignments(ctx, id); err != nil || len(rows) != 0 {
		t.Fatalf("stale assignments after delete: %+v, %v", rows, err)
	}
	if detail, err := c.Satellite(ctx, id); err != nil || len(detail.Hosts) != 0 {
		t.Fatalf("stale detail hosts: %+v, %v", detail, err)
	}
	if err := sat.DeleteSelf(ctx); err != nil {
		t.Fatalf("revoked satellite token: %v", err)
	}
}

func TestSatelliteWireErrors(t *testing.T) {
	for _, status := range []int{401, 403, 404, 500, 200} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var paths []string
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.RequestURI())
				w.WriteHeader(status)
				_, _ = w.Write([]byte("{invalid"))
			}))
			defer s.Close()
			c := OwnerClient{BaseURL: s.URL, Token: "fixture"}
			ctx := context.Background()
			if _, err := c.Satellite(ctx, "sat/id"); err == nil {
				t.Fatal("expected detail error")
			}
			if _, err := c.SatelliteAssignments(ctx, "sat & other"); err == nil {
				t.Fatal("expected assignment error")
			}
			if paths[0] != "/api/v1/satellites/sat%2Fid" || paths[1] != "/api/v1/satellites/assignment?satellite=sat+%26+other" {
				t.Fatalf("escaping: %v", paths)
			}
			if err := c.DeleteSelf(ctx); status != 401 && status != 200 && err == nil {
				t.Fatal("expected delete error")
			}
		})
	}
}
