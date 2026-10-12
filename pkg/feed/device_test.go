package feed

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSigningDeviceCommands(t *testing.T) {
	te := newTestEnv(t, &fakeExcerpter{})
	pub := base64.StdEncoding.EncodeToString(make([]byte, 32))
	revoked := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("missing paired token")
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/feed/devices":
			_ = json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{"pubkey": pub, "label": "CLI"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/feed/devices/revoke":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["pubkey"] != pub {
				t.Errorf("revoke body: %+v", body)
			}
			revoked = true
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	te.env.Cloud = func() (string, string) { return server.URL, "test-token" }
	if got := te.mustRun("key", "ls"); !strings.Contains(got, pub+"\tactive\tCLI") {
		t.Fatalf("key ls: %s", got)
	}
	te.mustRun("key", "revoke", pub)
	if !revoked {
		t.Fatal("revoke endpoint not called")
	}
}
