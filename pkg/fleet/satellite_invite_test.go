package fleet

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSatelliteServer struct {
	mu             sync.Mutex
	ownerToken     string
	exchangedToken string
	inviteCode     string
	exchangeStatus int
	mintStatus     int
	lastScopes     []string
	lastTTL        string
	lastName       string
}

func (f *fakeSatelliteServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	auth := r.Header.Get("Authorization")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tokens/exchange":
		if auth != "Bearer "+f.ownerToken {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		var req struct {
			Scopes []string `json:"scopes"`
			TTL    string   `json:"ttl"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.lastScopes = req.Scopes
		f.lastTTL = req.TTL

		if f.exchangeStatus != 0 && f.exchangeStatus != http.StatusCreated {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.exchangeStatus)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "scope escalation refused"})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      f.exchangedToken,
			"token_id":   "tok-exchanged-1",
			"scopes":     req.Scopes,
			"expires_at": time.Now().Add(10 * time.Minute).UTC(),
		})

	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/hosts/invites":
		if auth != "Bearer "+f.exchangedToken {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "forbidden"})
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.lastName = req.Name

		if f.mintStatus != 0 && f.mintStatus != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.mintStatus)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "mint failed"})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    f.inviteCode,
			"expires": time.Now().Add(30 * time.Minute).UTC(),
		})

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func setupFakeSatellite(t *testing.T) (*fakeSatelliteServer, *httptest.Server) {
	t.Helper()
	fake := &fakeSatelliteServer{
		ownerToken:     "test-owner-token-secret-999",
		exchangedToken: "test-exchanged-token-secret-888",
		inviteCode:     "invite-code-single-use-777",
	}
	srv := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(srv.Close)
	return fake, srv
}

func runSatelliteInvite(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewSatelliteInviteCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	// stderr carries the owner-transfer note on every mint; keep the
	// returned string the stdout lane only, so code assertions see the
	// bare code. The note has its own test.
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestSatelliteInviteToFile(t *testing.T) {
	fake, srv := setupFakeSatellite(t)
	outFile := filepath.Join(t.TempDir(), "sub", "test.invite")

	out, err := runSatelliteInvite(t,
		"--satellite", srv.URL,
		"--name", "puppy",
		"--to-file", outFile,
		"--token", fake.ownerToken,
	)
	if err != nil {
		t.Fatalf("invite failed: %v (%s)", err, out)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}
	if strings.TrimSpace(string(data)) != fake.inviteCode {
		t.Fatalf("file content = %q, want %q", string(data), fake.inviteCode)
	}

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(outFile)
		if err != nil {
			t.Fatalf("stat failed: %v", err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("file mode = %v, want 0600", perm)
		}
	}

	// Verify stdout confirms file write without printing code or tokens
	if !strings.Contains(out, "wrote invite for puppy to "+outFile) {
		t.Fatalf("output missing confirmation: %q", out)
	}
	if strings.Contains(out, fake.inviteCode) {
		t.Fatalf("code printed to stdout without --stdout: %q", out)
	}
	if strings.Contains(out, fake.ownerToken) || strings.Contains(out, fake.exchangedToken) {
		t.Fatalf("tokens leaked in stdout: %q", out)
	}

	// Verify request payload on fake
	if len(fake.lastScopes) != 1 || fake.lastScopes[0] != "host:invite-mint" {
		t.Fatalf("scopes = %v, want [host:invite-mint]", fake.lastScopes)
	}
	if fake.lastTTL != "10m" {
		t.Fatalf("ttl = %q, want 10m", fake.lastTTL)
	}
	if fake.lastName != "puppy" {
		t.Fatalf("mint name = %q, want puppy", fake.lastName)
	}
}

func TestSatelliteInviteStdout(t *testing.T) {
	fake, srv := setupFakeSatellite(t)

	out, err := runSatelliteInvite(t,
		"--satellite", srv.URL,
		"--name", "zoo",
		"--stdout",
		"--token", fake.ownerToken,
	)
	if err != nil {
		t.Fatalf("invite failed: %v (%s)", err, out)
	}

	trimmed := strings.TrimSpace(out)
	if trimmed != fake.inviteCode {
		t.Fatalf("stdout = %q, want %q", trimmed, fake.inviteCode)
	}
	if strings.Contains(out, fake.ownerToken) || strings.Contains(out, fake.exchangedToken) {
		t.Fatalf("tokens leaked in stdout: %q", out)
	}
}

func TestSatelliteInviteDefaultFile(t *testing.T) {
	fake, srv := setupFakeSatellite(t)

	// Change working directory to a temp dir so default <name>.invite lands there
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origDir) }()

	out, err := runSatelliteInvite(t,
		"--satellite", srv.URL,
		"--name", "myhost",
		"--token", fake.ownerToken,
	)
	if err != nil {
		t.Fatalf("invite failed: %v (%s)", err, out)
	}

	expectedFile := filepath.Join(tmpDir, "myhost.invite")
	data, err := os.ReadFile(expectedFile)
	if err != nil {
		t.Fatalf("default invite file not created: %v", err)
	}
	if strings.TrimSpace(string(data)) != fake.inviteCode {
		t.Fatalf("file content = %q, want %q", string(data), fake.inviteCode)
	}

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(expectedFile)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("file mode = %v, want 0600", perm)
		}
	}

	if !strings.Contains(out, "wrote invite for myhost to myhost.invite") {
		t.Fatalf("output missing confirmation: %q", out)
	}
	if strings.Contains(out, fake.inviteCode) {
		t.Fatalf("code printed to stdout without --stdout: %q", out)
	}
	if strings.Contains(out, fake.ownerToken) || strings.Contains(out, fake.exchangedToken) {
		t.Fatalf("tokens leaked in stdout: %q", out)
	}
}

func TestSatelliteInviteJSON(t *testing.T) {
	fake, srv := setupFakeSatellite(t)

	out, err := runSatelliteInvite(t,
		"--satellite", srv.URL,
		"--name", "ivy",
		"--stdout",
		"--json",
		"--token", fake.ownerToken,
	)
	if err != nil {
		t.Fatalf("invite failed: %v (%s)", err, out)
	}

	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("failed to parse json output %q: %v", out, err)
	}

	if res["code"] != fake.inviteCode {
		t.Fatalf("json code = %v, want %s", res["code"], fake.inviteCode)
	}
	if res["name"] != "ivy" {
		t.Fatalf("json name = %v, want ivy", res["name"])
	}
	if res["satellite"] != srv.URL {
		t.Fatalf("json satellite = %v, want %s", res["satellite"], srv.URL)
	}
	if exp, ok := res["expires_at"].(string); !ok || exp == "" {
		t.Fatalf("json expires_at missing or empty: %v", res)
	}

	if strings.Contains(out, fake.ownerToken) || strings.Contains(out, fake.exchangedToken) {
		t.Fatalf("tokens leaked in json output: %q", out)
	}
}

func TestSatelliteInviteCustomTTL(t *testing.T) {
	fake, srv := setupFakeSatellite(t)

	_, err := runSatelliteInvite(t,
		"--satellite", srv.URL,
		"--name", "pup",
		"--ttl", "5m",
		"--stdout",
		"--token", fake.ownerToken,
	)
	if err != nil {
		t.Fatalf("invite failed: %v", err)
	}
	if fake.lastTTL != "5m" {
		t.Fatalf("ttl = %q, want 5m", fake.lastTTL)
	}
}

func TestSatelliteInviteEscalation403(t *testing.T) {
	fake, srv := setupFakeSatellite(t)
	fake.exchangeStatus = http.StatusForbidden

	out, err := runSatelliteInvite(t,
		"--satellite", srv.URL,
		"--name", "pup",
		"--stdout",
		"--token", fake.ownerToken,
	)
	if err == nil {
		t.Fatal("wanted 403 escalation error, got nil")
	}

	wantErr := "owner token lacks host:invite-mint; see docs"
	if !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("err = %q, want it to contain %q", err.Error(), wantErr)
	}
	if strings.Contains(err.Error(), fake.ownerToken) || strings.Contains(out, fake.ownerToken) {
		t.Fatalf("token leaked in error or output: err=%q out=%q", err.Error(), out)
	}
}

func TestSatelliteInviteValidationErrors(t *testing.T) {
	_, srv := setupFakeSatellite(t)

	// Missing --satellite
	_, err := runSatelliteInvite(t, "--name", "pup", "--token", "tok")
	if err == nil || !strings.Contains(err.Error(), "satellite invite needs --satellite") {
		t.Fatalf("missing --satellite err = %v", err)
	}

	// Missing --name
	_, err = runSatelliteInvite(t, "--satellite", srv.URL, "--token", "tok")
	if err == nil || !strings.Contains(err.Error(), "satellite invite needs --name") {
		t.Fatalf("missing --name err = %v", err)
	}

	// Missing token (clear env)
	t.Setenv("BASHY_FLEET_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")
	_, err = runSatelliteInvite(t, "--satellite", srv.URL, "--name", "pup")
	if err == nil || !strings.Contains(err.Error(), "no cloudbox token") {
		t.Fatalf("missing token err = %v", err)
	}
}

func TestSatelliteInviteMintFailure(t *testing.T) {
	fake, srv := setupFakeSatellite(t)
	fake.mintStatus = http.StatusInternalServerError

	_, err := runSatelliteInvite(t,
		"--satellite", srv.URL,
		"--name", "pup",
		"--stdout",
		"--token", fake.ownerToken,
	)
	if err == nil {
		t.Fatal("wanted mint error, got nil")
	}
	if !strings.Contains(err.Error(), "POST /api/v1/hosts/invites") {
		t.Fatalf("err = %q, want it to mention POST /api/v1/hosts/invites", err.Error())
	}
}

func TestSatelliteInviteViaParentCmd(t *testing.T) {
	fake, srv := setupFakeSatellite(t)

	parent := NewSatelliteCmd()
	var out, errOut strings.Builder
	parent.SetOut(&out)
	parent.SetErr(&errOut)
	parent.SetArgs([]string{
		"invite",
		"--satellite", srv.URL,
		"--name", "treehost",
		"--stdout",
		"--token", fake.ownerToken,
	})

	if err := parent.Execute(); err != nil {
		t.Fatalf("execute parent cmd failed: %v", err)
	}

	if strings.TrimSpace(out.String()) != fake.inviteCode {
		t.Fatalf("stdout = %q, want %q", out.String(), fake.inviteCode)
	}
	if !strings.Contains(errOut.String(), satelliteInviteOwnerNote) {
		t.Fatalf("parent path missing owner note: %q", errOut.String())
	}
}

// The code makes the SATELLITE OWNER the owner of the host that redeems it.
// Help, every output mode and --json must say so — an owner-scoped code
// handed to a member silently transfers that member's host to the satellite
// owner, so the warning rides every mint (S606 story 748f23cc0790).
func TestSatelliteInviteSaysOwnerTransfer(t *testing.T) {
	fake, srv := setupFakeSatellite(t)

	// Help says it.
	cmd := NewSatelliteInviteCmd()
	for _, want := range []string{"SATELLITE OWNER", "owner node", "provisioning", "Never hand this"} {
		if !strings.Contains(cmd.Long, want) {
			t.Fatalf("help missing %q:\n%s", want, cmd.Long)
		}
	}

	run := func(args ...string) (string, string, error) {
		t.Helper()
		c := NewSatelliteInviteCmd()
		var out, errOut strings.Builder
		c.SetOut(&out)
		c.SetErr(&errOut)
		c.SetArgs(args)
		err := c.Execute()
		return out.String(), errOut.String(), err
	}

	// --stdout: bare code on stdout, the note on stderr, no tokens anywhere.
	out, errOut, err := run("--satellite", srv.URL, "--name", "pup", "--stdout", "--token", fake.ownerToken)
	if err != nil {
		t.Fatalf("invite: %v (%s)", err, out)
	}
	if strings.TrimSpace(out) != fake.inviteCode {
		t.Fatalf("stdout = %q, want the bare code %q", out, fake.inviteCode)
	}
	if !strings.Contains(errOut, satelliteInviteOwnerNote) {
		t.Fatalf("stderr note missing: %q", errOut)
	}
	if strings.Contains(out+errOut, fake.ownerToken) || strings.Contains(out+errOut, fake.exchangedToken) {
		t.Fatalf("tokens leaked: out=%q err=%q", out, errOut)
	}

	// default file mode notes it too.
	outFile := filepath.Join(t.TempDir(), "node.invite")
	out, errOut, err = run("--satellite", srv.URL, "--name", "node", "--to-file", outFile, "--token", fake.ownerToken)
	if err != nil {
		t.Fatalf("invite to file: %v (%s)", err, out)
	}
	if !strings.Contains(out, "wrote invite for node to "+outFile) {
		t.Fatalf("file confirmation missing: %q", out)
	}
	if !strings.Contains(errOut, satelliteInviteOwnerNote) {
		t.Fatalf("file mode note missing: %q", errOut)
	}

	// --json carries it as fields.
	out, _, err = run("--satellite", srv.URL, "--name", "node", "--stdout", "--json", "--token", fake.ownerToken)
	if err != nil {
		t.Fatalf("invite --json: %v (%s)", err, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invite --json is not JSON: %v (%s)", err, out)
	}
	if res["owner"] != "satellite-owner" {
		t.Fatalf("json owner = %v, want satellite-owner", res["owner"])
	}
	if note, _ := res["note"].(string); !strings.Contains(note, "SATELLITE OWNER") || !strings.Contains(note, "never for members") {
		t.Fatalf("json note = %v", res["note"])
	}
}

func TestSatelliteInviteEnvTokenResolution(t *testing.T) {
	fake, srv := setupFakeSatellite(t)
	t.Setenv("BASHY_FLEET_TOKEN", fake.ownerToken)
	t.Setenv("BASHY_API_KEY", "")

	out, err := runSatelliteInvite(t,
		"--satellite", srv.URL,
		"--name", "envhost",
		"--stdout",
	)
	if err != nil {
		t.Fatalf("invite with BASHY_FLEET_TOKEN failed: %v (%s)", err, out)
	}
	if strings.TrimSpace(out) != fake.inviteCode {
		t.Fatalf("stdout = %q, want %q", out, fake.inviteCode)
	}
}
