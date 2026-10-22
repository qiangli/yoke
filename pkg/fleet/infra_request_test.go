package fleet

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// `infra request` (S490 story a0e6f1ae): thin client over the master
// /api/v1/provisioning/requests endpoints. The master is an httptest
// fake speaking the real envelopes ({"requests": [...]},
// {"request": {...}}, {code, error} refusals); no cloudbox is needed.

type fakeMaster struct {
	mu         sync.Mutex
	resumeBody string
}

func (f *fakeMaster) row() map[string]any {
	return map[string]any{
		"id": "req-1", "owner": "owner@example.com",
		"idempotency_key": "infra-provision-zoo",
		"provider":        "digitalocean",
		"region":          "sfo3", "size": "s-4vcpu-8gb",
		"image": "ubuntu-24-04-x64",
		"name":  "zoo", "satellite_id": "zoo", "profile": "standard",
		"state": "vm_creating", "needs_token": false, "resource_id": "123",
	}
}

func (f *fakeMaster) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer owner-session" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	write := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/provisioning/requests":
		write(200, map[string]any{"requests": []any{f.row()}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/provisioning/requests/req-1":
		write(200, map[string]any{"request": f.row()})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/provisioning/requests/"):
		write(404, map[string]any{"code": "not_found", "error": "provisioning request not found"})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/provisioning/requests/req-1/resume":
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.resumeBody = string(body)
		f.mu.Unlock()
		row := f.row()
		row["state"] = "vm_creating"
		write(200, map[string]any{"request": row})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/provisioning/requests/req-1/stop":
		row := f.row()
		row["state"] = "stopped"
		write(200, map[string]any{"request": row})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func setupFakeMaster(t *testing.T) *fakeMaster {
	t.Helper()
	f := &fakeMaster{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	t.Setenv("BASHY_CLOUDBOX_URL", srv.URL)
	t.Setenv("BASHY_FLEET_TOKEN", "owner-session")
	t.Setenv("BASHY_API_KEY", "")
	return f
}

func runRequest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewInfraCmd(WithoutCloudOverlay())
	cmd.SetArgs(append([]string{"request"}, args...))
	var out strings.Builder
	cmd.SetOut(&out)
	err := cmd.Execute()
	return out.String(), err
}

func TestInfraRequestLsAndStatus(t *testing.T) {
	isolatedFleetRoot(t)
	setupFakeMaster(t)

	out, err := runRequest(t, "ls")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	for _, want := range []string{"req-1", "zoo", "digitalocean", "sfo3", "vm_creating"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ls output missing %q:\n%s", want, out)
		}
	}

	jout, err := runRequest(t, "ls", "--json")
	if err != nil {
		t.Fatalf("ls --json: %v", err)
	}
	var docs struct {
		Requests []ProvisionRequest `json:"requests"`
	}
	if err := json.Unmarshal([]byte(jout), &docs); err != nil {
		t.Fatalf("ls --json is not JSON: %v:\n%s", err, jout)
	}
	if len(docs.Requests) != 1 || docs.Requests[0].ID != "req-1" {
		t.Fatalf("unexpected list: %+v", docs)
	}

	sout, err := runRequest(t, "status", "--id", "req-1")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, want := range []string{"id: req-1", "satellite: zoo", "state: vm_creating", "resource: 123"} {
		if !strings.Contains(sout, want) {
			t.Fatalf("status output missing %q:\n%s", want, sout)
		}
	}

	if _, err := runRequest(t, "status", "--id", "nope"); err == nil ||
		!strings.Contains(err.Error(), "not_found") {
		t.Fatalf("unknown id: err = %v", err)
	}
	if _, err := runRequest(t, "status"); err == nil {
		t.Fatal("status without --id succeeded")
	}
}

func TestInfraRequestResumeSendsTokenOnlyWithCredentialRef(t *testing.T) {
	isolatedFleetRoot(t)
	f := setupFakeMaster(t)

	out, err := runRequest(t, "resume", "--id", "req-1")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !strings.Contains(out, "resumed req-1") {
		t.Fatalf("unexpected resume output:\n%s", out)
	}
	f.mu.Lock()
	body := f.resumeBody
	f.mu.Unlock()
	if body != "" {
		t.Fatalf("resume without --credential-ref sent a body: %q", body)
	}

	const provToken = "fresh-provider-token-AbC123"
	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", provToken)
	t.Setenv("DIGITALOCEAN_TOKEN", "")
	out, err = runRequest(t, "resume", "--id", "req-1", "--credential-ref", "digitalocean")
	if err != nil {
		t.Fatalf("resume --credential-ref: %v", err)
	}
	var sent struct {
		Token string `json:"token"`
	}
	f.mu.Lock()
	body = f.resumeBody
	f.mu.Unlock()
	if err := json.Unmarshal([]byte(body), &sent); err != nil || sent.Token != provToken {
		t.Fatalf("master saw body %q, want {\"token\": <ref>}", body)
	}
	if strings.Contains(out, provToken) {
		t.Fatal("provider token printed in resume output")
	}
}

func TestInfraRequestStop(t *testing.T) {
	isolatedFleetRoot(t)
	setupFakeMaster(t)

	out, err := runRequest(t, "stop", "--id", "req-1")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !strings.Contains(out, "stopped req-1") {
		t.Fatalf("unexpected stop output:\n%s", out)
	}
	jout, err := runRequest(t, "stop", "--id", "req-1", "--json")
	if err != nil {
		t.Fatalf("stop --json: %v", err)
	}
	var r ProvisionRequest
	if err := json.Unmarshal([]byte(jout), &r); err != nil {
		t.Fatalf("stop --json is not JSON: %v:\n%s", err, jout)
	}
	if r.ID != "req-1" || r.State != "stopped" {
		t.Fatalf("unexpected stop record: %+v", r)
	}
}

func TestInfraRequestNeedsMasterToken(t *testing.T) {
	isolatedFleetRoot(t)
	isolateConfig(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("BASHY_CLOUDBOX_URL", "http://127.0.0.1:1")
	t.Setenv("BASHY_FLEET_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")
	if _, err := runRequest(t, "ls"); err == nil ||
		!strings.Contains(err.Error(), "no cloudbox token") {
		t.Fatalf("tokenless ls: err = %v", err)
	}
}
