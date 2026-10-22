package fleet

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// `infra credential` (S489.6d, story 9ea7c26e068f): thin client over the
// master /api/v1/provisioning/credentials endpoints. The master is an
// httptest fake speaking the real envelopes (201 {"credential": meta},
// {"credentials": [...]}, {"status":"revoked"} refusals with {code,
// error}); no cloudbox is needed and the value only ever travels toward
// the fake, never back.

type fakeCredentialMaster struct {
	mu       sync.Mutex
	addBody  map[string]any
	rotateID string
	rotBody  map[string]any
	revokeID string
}

func (f *fakeCredentialMaster) row() map[string]any {
	return map[string]any{
		"id": 7, "owner": "owner@example.com",
		"provider": "digitalocean", "role": "purge",
		"account_email": "ops@dhnt-orbit.example", "account_uuid": "acct-orbit",
		"validated_scopes":  "reads:account,droplets,tags; refused:droplet:create",
		"created_at":        "2026-10-22T00:00:00Z",
		"expires_at":        "2027-01-20T00:00:00Z",
		"last_validated_at": "2026-10-22T00:00:00Z",
		"expiry_status":     "valid",
	}
}

func (f *fakeCredentialMaster) serve(w http.ResponseWriter, r *http.Request) {
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
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/provisioning/credentials":
		write(200, map[string]any{"credentials": []any{f.row()}})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/provisioning/credentials":
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		f.mu.Lock()
		f.addBody = m
		f.mu.Unlock()
		if m["provider"] == nil || m["role"] == nil || m["value"] == nil {
			write(400, map[string]any{"code": "invalid_request", "error": "provider, role and value are required"})
			return
		}
		if m["provider"] == "aws" {
			write(400, map[string]any{"code": "unsupported_provider", "error": "credentials are not supported for provider aws"})
			return
		}
		write(201, map[string]any{"credential": f.row()})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/provisioning/credentials/"):
		f.mu.Lock()
		f.revokeID = strings.TrimPrefix(r.URL.Path, "/api/v1/provisioning/credentials/")
		f.mu.Unlock()
		if f.revokeID != "7" {
			write(404, map[string]any{"code": "not_found", "error": "no such credential"})
			return
		}
		write(200, map[string]any{"status": "revoked", "id": 7})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rotate"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/provisioning/credentials/"), "/rotate")
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		f.mu.Lock()
		f.rotateID, f.rotBody = id, m
		f.mu.Unlock()
		if id != "7" {
			write(404, map[string]any{"code": "not_found", "error": "no such credential"})
			return
		}
		if m["value"] == nil {
			write(400, map[string]any{"code": "invalid_request", "error": "value is required"})
			return
		}
		write(200, map[string]any{"credential": f.row()})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func setupFakeCredentialMaster(t *testing.T) *fakeCredentialMaster {
	t.Helper()
	f := &fakeCredentialMaster{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	t.Setenv("BASHY_CLOUDBOX_URL", srv.URL)
	t.Setenv("BASHY_FLEET_TOKEN", "owner-session")
	t.Setenv("BASHY_API_KEY", "")
	return f
}

func runCredentialWithReader(t *testing.T, in io.Reader, args ...string) (string, error) {
	t.Helper()
	cmd := NewInfraCmd(WithoutCloudOverlay())
	cmd.SetArgs(append([]string{"credential"}, args...))
	if in != nil {
		cmd.SetIn(in)
	} else {
		cmd.SetIn(strings.NewReader(""))
	}
	var out strings.Builder
	cmd.SetOut(&out)
	err := cmd.Execute()
	return out.String(), err
}

func runCredential(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	return runCredentialWithReader(t, strings.NewReader(stdin), args...)
}

func TestInfraCredentialAddReadsValueFromStdin(t *testing.T) {
	isolatedFleetRoot(t)
	f := setupFakeCredentialMaster(t)

	out, err := runCredential(t, "do-purge-op-token\n", "add", "--provider", "do", "--role", "purge")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	for _, want := range []string{"digitalocean/purge", "id 7", "ops@dhnt-orbit.example", "refused:droplet:create"} {
		if !strings.Contains(out, want) {
			t.Fatalf("add output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "do-purge-op-token") {
		t.Fatalf("add echoed the value:\n%s", out)
	}
	f.mu.Lock()
	sent := f.addBody["value"]
	f.mu.Unlock()
	if sent != "do-purge-op-token" {
		t.Fatalf("master got value %q (newline must be trimmed)", sent)
	}

	jout, err := runCredential(t, "do-purge-op-token", "add", "--provider", "do", "--role", "purge", "--json")
	if err != nil {
		t.Fatalf("add --json: %v", err)
	}
	var rec CredentialRecord
	if err := json.Unmarshal([]byte(jout), &rec); err != nil {
		t.Fatalf("add --json is not a record: %v:\n%s", err, jout)
	}
	if rec.ID != 7 || rec.Provider != "digitalocean" || rec.Role != "purge" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if strings.Contains(jout, "do-purge-op-token") {
		t.Fatalf("add --json leaked the value:\n%s", jout)
	}
}

func TestInfraCredentialAddRefusals(t *testing.T) {
	isolatedFleetRoot(t)
	setupFakeCredentialMaster(t)

	if _, err := runCredential(t, "v", "add", "--provider", "do"); err == nil ||
		!strings.Contains(err.Error(), "--provider and --role are required") {
		t.Fatalf("add without --role: err = %v", err)
	}
	if _, err := runCredential(t, "", "add", "--provider", "do", "--role", "purge"); err == nil ||
		!strings.Contains(err.Error(), "no credential on stdin") {
		t.Fatalf("add with empty stdin: err = %v", err)
	}
	if _, err := runCredential(t, "v", "add", "--provider", "do", "--role", "purge", "--value", "x"); err == nil ||
		!strings.Contains(err.Error(), "unknown flag: --value") {
		t.Fatalf("add --value must not exist: err = %v", err)
	}
	if _, err := runCredential(t, "v", "add", "--provider", "aws", "--role", "purge"); err == nil ||
		!strings.Contains(err.Error(), "unsupported_provider") {
		t.Fatalf("add unknown provider: err = %v", err)
	}
	if _, err := runCredential(t, "v", "add", "--provider", "do", "--role", "purge", "--expires-at", "yesterday"); err == nil ||
		!strings.Contains(err.Error(), "invalid --expires-at") {
		t.Fatalf("add bad expiry: err = %v", err)
	}
}

func TestInfraCredentialLsRmRotate(t *testing.T) {
	isolatedFleetRoot(t)
	f := setupFakeCredentialMaster(t)

	out, err := runCredential(t, "", "ls")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	for _, want := range []string{"7", "digitalocean", "purge", "ops@dhnt-orbit.example", "expires:2027-01-20"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ls output missing %q:\n%s", want, out)
		}
	}

	jout, err := runCredential(t, "", "ls", "--json")
	if err != nil {
		t.Fatalf("ls --json: %v", err)
	}
	var env struct {
		Credentials []CredentialRecord `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(jout), &env); err != nil || len(env.Credentials) != 1 {
		t.Fatalf("ls --json: %v:\n%s", err, jout)
	}

	rout, err := runCredential(t, "do-purge-op-token-v2\n", "rotate", "--provider", "do", "--role", "purge")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if !strings.Contains(rout, "rotated digitalocean/purge (id 7") {
		t.Fatalf("rotate output: %s", rout)
	}
	if strings.Contains(rout, "do-purge-op-token-v2") {
		t.Fatalf("rotate echoed the value:\n%s", rout)
	}
	f.mu.Lock()
	rotVal, rotID := f.rotBody["value"], f.rotateID
	f.mu.Unlock()
	if rotVal != "do-purge-op-token-v2" || rotID != "7" {
		t.Fatalf("rotate sent value=%q id=%q", rotVal, rotID)
	}

	// Rotate by --id.
	routID, err := runCredential(t, "do-purge-op-token-v3\n", "rotate", "--id", "7")
	if err != nil {
		t.Fatalf("rotate --id 7: %v", err)
	}
	if !strings.Contains(routID, "rotated digitalocean/purge (id 7") {
		t.Fatalf("rotate --id 7 output: %s", routID)
	}

	// rm --id 7 --json
	jrm, err := runCredential(t, "", "rm", "--id", "7", "--json")
	if err != nil {
		t.Fatalf("rm --id 7 --json: %v", err)
	}
	var rmResp struct {
		Status string `json:"status"`
		ID     uint64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(jrm), &rmResp); err != nil || rmResp.ID != 7 || rmResp.Status != "revoked" {
		t.Fatalf("rm --id 7 --json unexpected output: %v (%s)", err, jrm)
	}

	// rm --id 7
	mout, err := runCredential(t, "", "rm", "--id", "7")
	if err != nil {
		t.Fatalf("rm --id 7: %v", err)
	}
	if !strings.Contains(mout, "revoked digitalocean/purge (id 7)") {
		t.Fatalf("rm output: %s", mout)
	}
	f.mu.Lock()
	revoked := f.revokeID
	f.mu.Unlock()
	if revoked != "7" {
		t.Fatalf("rm revoked id %q", revoked)
	}

	// rm by provider/role
	pout, err := runCredential(t, "", "rm", "--provider", "do", "--role", "purge")
	if err != nil {
		t.Fatalf("rm --provider do --role purge: %v", err)
	}
	if !strings.Contains(pout, "revoked digitalocean/purge (id 7)") {
		t.Fatalf("rm output: %s", pout)
	}

	if _, err := runCredential(t, "", "rm", "--id", "999"); err == nil ||
		!strings.Contains(err.Error(), "no such credential") {
		t.Fatalf("rm nonexistent id: err = %v", err)
	}
	if _, err := runCredential(t, "", "rm", "--provider", "do", "--role", "provision"); err == nil ||
		!strings.Contains(err.Error(), "no do provision credential") {
		t.Fatalf("rm unknown pair: err = %v", err)
	}
	if _, err := runCredential(t, "", "rm", "--provider", "do"); err == nil {
		t.Fatal("rm without --role succeeded")
	}
	if _, err := runCredential(t, "", "rm"); err == nil ||
		!strings.Contains(err.Error(), "--id ID (or --provider and --role) is required") {
		t.Fatalf("rm without id or provider/role: err = %v", err)
	}
}

func TestInfraCredentialDevNull(t *testing.T) {
	isolatedFleetRoot(t)
	setupFakeCredentialMaster(t)

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	defer devNull.Close()

	_, err = runCredentialWithReader(t, devNull, "add", "--provider", "do", "--role", "purge")
	if err == nil || !strings.Contains(err.Error(), "no credential on stdin") {
		t.Fatalf("devnull should be treated as empty stdin, got: %v", err)
	}
}

func TestInfraCredentialNeedsMasterToken(t *testing.T) {
	isolatedFleetRoot(t)
	t.Setenv("BASHY_CLOUDBOX_URL", "http://127.0.0.1:1")
	t.Setenv("BASHY_FLEET_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")
	if _, err := runCredential(t, "", "ls"); err == nil ||
		!strings.Contains(err.Error(), "no cloudbox token") {
		t.Fatalf("ls without token: err = %v", err)
	}
}
