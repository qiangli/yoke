package rods

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHostPruneDryRunAndApply(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	var gets, deletes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer owner" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
			w.WriteHeader(401)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/hosts":
			gets++
			if r.URL.Query().Get("stale") != "336h0m0s" {
				t.Errorf("stale = %q", r.URL.Query().Get("stale"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"hosts":[{"id":"1","name":"old","reason":"stale"},{"id":"2","name":"gone","reason":"deleted"}]}`))
		case r.Method == "DELETE" && r.URL.Path == "/api/hosts/old":
			deletes++
			if r.URL.Query().Get("confirm") != "old" || r.URL.Query().Get("purge") != "true" {
				t.Errorf("delete query = %v", r.URL.Query())
			}
			w.WriteHeader(204)
		case r.Method == "DELETE" && r.URL.Path == "/api/hosts/gone":
			deletes++
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	t.Setenv("BASHY_CLOUDBOX_URL", srv.URL)
	t.Setenv("BASHY_FLEET_TOKEN", "owner")
	c := NewHostPruneCommand()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetArgs(nil)
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if deletes != 0 || gets != 1 || !strings.Contains(out.String(), "would remove host old") || !strings.Contains(out.String(), "deleted") {
		t.Fatalf("dry: gets=%d deletes=%d out=%s", gets, deletes, out.String())
	}
	c = NewHostPruneCommand()
	out.Reset()
	c.SetOut(&out)
	c.SetArgs([]string{"--apply"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if deletes != 2 || gets != 4 || !strings.Contains(out.String(), "removed 2 item(s)") {
		t.Fatalf("apply: gets=%d deletes=%d out=%s", gets, deletes, out.String())
	}
}

func TestHostPruneRechecksBeforeDelete(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	var gets, deletes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			gets++
			if gets == 1 {
				_, _ = w.Write([]byte(`{"hosts":[{"name":"recovered","reason":"stale"}]}`))
			} else {
				_, _ = w.Write([]byte(`{"hosts":[]}`))
			}
		case "DELETE":
			deletes++
		default:
			w.WriteHeader(405)
		}
	}))
	defer srv.Close()
	t.Setenv("BASHY_CLOUDBOX_URL", srv.URL)
	t.Setenv("BASHY_FLEET_TOKEN", "owner")
	c := NewHostPruneCommand()
	c.SetArgs([]string{"--apply"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if gets != 2 || deletes != 0 {
		t.Fatalf("gets=%d deletes=%d; recovered host was deleted", gets, deletes)
	}
}

func TestSatellitePruneDryRunAndApply(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	var gets, posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer owner" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
			w.WriteHeader(401)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.String() == "/api/v1/satellites?prune=1":
			gets++
			_, _ = w.Write([]byte(`{"satellites":[{"id":"lost","reason":"lost"}],"provisioning":[{"id":"req","reason":"never_enrolled"}]}`))
		case r.Method == "POST" && r.URL.Path == "/api/satellites/lost/destroy-lost":
			posts++
			w.WriteHeader(200)
		case r.Method == "POST" && r.URL.Path == "/api/v1/provisioning/requests/req/destroy":
			posts++
			w.WriteHeader(202)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	t.Setenv("BASHY_CLOUDBOX_URL", srv.URL)
	t.Setenv("BASHY_FLEET_TOKEN", "owner")
	c := NewSatellitePruneCommand()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetArgs(nil)
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if posts != 0 || !strings.Contains(out.String(), "satellite:lost") || !strings.Contains(out.String(), "provisioning:req") {
		t.Fatalf("dry posts=%d out=%s", posts, out.String())
	}
	c = NewSatellitePruneCommand()
	out.Reset()
	c.SetOut(&out)
	c.SetArgs([]string{"--apply"})
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "owner approval is required") {
		t.Fatalf("apply should report queued destruction: %v", err)
	}
	if posts != 2 || gets != 4 {
		t.Fatalf("apply gets=%d posts=%d out=%s", gets, posts, out.String())
	}
}

func TestSatelliteTransfer(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	var posts int
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer owner" {
			w.WriteHeader(401)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/api/v1/satellites" {
			_, _ = w.Write([]byte(`{"satellites":[{"id":"sat-1","name":"sat-a"}]}`))
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/v1/satellites/sat-1/transfer" {
			posts++
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["to"] != "new@example.com" {
				t.Errorf("to = %q", body["to"])
			}
			w.WriteHeader(status)
			if status == 409 {
				_, _ = w.Write([]byte(`{"error":"mutual Members required"}`))
			} else {
				_, _ = w.Write([]byte(`{"satellite_id":"sat-1","from":"old@example.com","to":"new@example.com","transferred_at":"2026-10-24T12:00:00Z","note":"moved"}`))
			}
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(404)
	}))
	defer srv.Close()
	t.Setenv("BASHY_CLOUDBOX_URL", srv.URL)
	t.Setenv("BASHY_FLEET_TOKEN", "owner")
	c := NewSatelliteTransferCommand()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetIn(strings.NewReader("n\n"))
	c.SetArgs([]string{"sat-a", "--to", "new@example.com"})
	if err := c.Execute(); err == nil || posts != 0 {
		t.Fatalf("cancel err=%v posts=%d", err, posts)
	}
	c = NewSatelliteTransferCommand()
	out.Reset()
	c.SetOut(&out)
	c.SetArgs([]string{"sat-a", "--to", "new@example.com", "--yes"})
	if err := c.Execute(); err != nil || posts != 1 || !strings.Contains(out.String(), "note: moved") {
		t.Fatalf("transfer err=%v posts=%d out=%s", err, posts, out.String())
	}
	status = 409
	c = NewSatelliteTransferCommand()
	c.SetArgs([]string{"sat-1", "--to", "new@example.com", "--yes"})
	if err := c.Execute(); err == nil || err.Error() != "mutual Members required" {
		t.Fatalf("409 err=%v", err)
	}
	c = NewSatelliteTransferCommand()
	c.SetArgs([]string{"missing", "--to", "new@example.com", "--yes"})
	if err := c.Execute(); err == nil || err.Error() != "not found or not yours" {
		t.Fatalf("404 err=%v", err)
	}
}
