package fleet

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeTransferMaster struct {
	mu             sync.Mutex
	rows           []map[string]any
	token          string
	listStatus     int
	transferStatus int
	transferErr    string
	transferResult *TransferResult
	seenRequests   []string
	transferPosts  int
	lastPostTo     string
	lastPostID     string
}

func (f *fakeTransferMaster) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seenRequests = append(f.seenRequests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		return
	}

	if r.Method == http.MethodGet && r.URL.Path == "/api/v1/satellites" {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.listStatus != 0 && f.listStatus/100 != 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.listStatus)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": http.StatusText(f.listStatus)})
			return
		}
		rows := f.rows
		if rows == nil {
			rows = []map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"satellites": rows})
		return
	}

	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/satellites/") &&
		strings.HasSuffix(r.URL.Path, "/transfer") {
		f.mu.Lock()
		f.transferPosts++
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/satellites/")
		id = strings.TrimSuffix(id, "/transfer")
		f.lastPostID = id

		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.lastPostTo = body["to"]

		if f.transferStatus != 0 && f.transferStatus/100 != 2 {
			status := f.transferStatus
			errMsg := f.transferErr
			if errMsg == "" {
				errMsg = http.StatusText(status)
			}
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": errMsg})
			return
		}

		res := f.transferResult
		if res == nil {
			res = &TransferResult{
				SatelliteID:   id,
				From:          "owner@example.com",
				To:            body["to"],
				TransferredAt: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
				Note:          "transferred successfully",
			}
		}
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(res)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
}

func (f *fakeTransferMaster) transferHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.transferPosts
}

func (f *fakeTransferMaster) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seenRequests...)
}

func setupTransferMaster(t *testing.T, fake *fakeTransferMaster) {
	t.Helper()
	if fake.token == "" {
		fake.token = "owner-token"
	}
	srv := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(srv.Close)
	t.Setenv("BASHY_CLOUDBOX_URL", srv.URL)
	t.Setenv("BASHY_FLEET_TOKEN", fake.token)
	t.Setenv("BASHY_API_KEY", "")
}

type testInteractiveInput struct {
	io.Reader
}

func (t testInteractiveInput) Interactive() bool { return true }

func runSatelliteTransfer(t *testing.T, in io.Reader, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := NewSatelliteCmd()
	var outBuf, errBuf strings.Builder
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	if in != nil {
		cmd.SetIn(in)
	}
	cmd.SetArgs(append([]string{"transfer"}, args...))
	err = cmd.Execute()
	return outBuf.String(), errBuf.String(), err
}

func TestSatelliteTransferJSONSuccess(t *testing.T) {
	isolatedFleetRoot(t)
	fake := &fakeTransferMaster{
		rows: []map[string]any{
			{"id": "sat-1", "name": "zoo", "endpoint": "https://zoo.example.com", "state": "active"},
		},
	}
	setupTransferMaster(t, fake)

	stdout, stderr, err := runSatelliteTransfer(t, nil, "sat-1", "--to", "bob@example.com", "--yes", "--json")
	if err != nil {
		t.Fatalf("transfer --yes --json failed: %v\nstderr: %s", err, stderr)
	}

	var res TransferResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nstdout: %s", err, stdout)
	}
	if res.SatelliteID != "sat-1" || res.From != "owner@example.com" || res.To != "bob@example.com" {
		t.Fatalf("unexpected TransferResult fields: %+v", res)
	}
	if res.Note != "transferred successfully" {
		t.Fatalf("unexpected note: %q", res.Note)
	}
	if res.TransferredAt.IsZero() {
		t.Fatalf("transferred_at is zero")
	}

	if hits := fake.transferHits(); hits != 1 {
		t.Fatalf("expected 1 transfer POST, got %d", hits)
	}
}

func TestSatelliteTransferInteractiveAccept(t *testing.T) {
	isolatedFleetRoot(t)
	fake := &fakeTransferMaster{
		rows: []map[string]any{
			{"id": "sat-1", "name": "my-satellite", "endpoint": "https://sat.example.com", "state": "active"},
		},
	}
	setupTransferMaster(t, fake)

	in := testInteractiveInput{strings.NewReader("y\n")}
	stdout, stderr, err := runSatelliteTransfer(t, in, "sat-1", "--to", "alice@example.com")
	if err != nil {
		t.Fatalf("interactive accept failed: %v\nstderr: %s", err, stderr)
	}

	// Prompt must be on stderr outside stdout
	if !strings.Contains(stderr, "Transfer my-satellite and everything it owns to alice@example.com? [y/N]") {
		t.Fatalf("prompt missing from stderr: %q", stderr)
	}
	if strings.Contains(stdout, "Transfer my-satellite") {
		t.Fatalf("prompt leaked into stdout: %q", stdout)
	}
	if !strings.Contains(stdout, "satellite: sat-1") || !strings.Contains(stdout, "to: alice@example.com") {
		t.Fatalf("stdout missing transfer result: %q", stdout)
	}
	if hits := fake.transferHits(); hits != 1 {
		t.Fatalf("expected 1 transfer POST, got %d", hits)
	}
}

func TestSatelliteTransferInteractiveAcceptJSON(t *testing.T) {
	isolatedFleetRoot(t)
	fake := &fakeTransferMaster{
		rows: []map[string]any{
			{"id": "sat-1", "name": "my-satellite", "endpoint": "https://sat.example.com", "state": "active"},
		},
	}
	setupTransferMaster(t, fake)

	in := testInteractiveInput{strings.NewReader("yes\n")}
	stdout, stderr, err := runSatelliteTransfer(t, in, "sat-1", "--to", "alice@example.com", "--json")
	if err != nil {
		t.Fatalf("interactive accept with --json failed: %v\nstderr: %s", err, stderr)
	}

	// Prompt must be printed to stderr, stdout must be clean JSON
	if !strings.Contains(stderr, "Transfer my-satellite and everything it owns to alice@example.com? [y/N]") {
		t.Fatalf("prompt missing from stderr: %q", stderr)
	}
	var res TransferResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("stdout should be pure JSON without prompt, but unmarshal failed: %v\nstdout: %q", err, stdout)
	}
	if res.SatelliteID != "sat-1" || res.To != "alice@example.com" {
		t.Fatalf("unexpected res: %+v", res)
	}
	if hits := fake.transferHits(); hits != 1 {
		t.Fatalf("expected 1 transfer POST, got %d", hits)
	}
}

func TestSatelliteTransferInteractiveCancel(t *testing.T) {
	isolatedFleetRoot(t)
	fake := &fakeTransferMaster{
		rows: []map[string]any{
			{"id": "sat-1", "name": "zoo", "endpoint": "https://zoo.example.com", "state": "active"},
		},
	}
	setupTransferMaster(t, fake)

	// User declines with 'n'
	in := testInteractiveInput{strings.NewReader("n\n")}
	_, _, err := runSatelliteTransfer(t, in, "sat-1", "--to", "bob@example.com")
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("expected transfer canceled error, got: %v", err)
	}
	if hits := fake.transferHits(); hits != 0 {
		t.Fatalf("assert zero transfer POSTs on cancel, got %d", hits)
	}

	// User sends EOF
	inEOF := testInteractiveInput{strings.NewReader("")}
	_, _, err = runSatelliteTransfer(t, inEOF, "sat-1", "--to", "bob@example.com")
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("expected transfer canceled error on EOF, got: %v", err)
	}
	if hits := fake.transferHits(); hits != 0 {
		t.Fatalf("assert zero transfer POSTs on cancel, got %d", hits)
	}
}

func TestSatelliteTransferYes(t *testing.T) {
	isolatedFleetRoot(t)
	fake := &fakeTransferMaster{
		rows: []map[string]any{
			{"id": "sat-1", "name": "zoo", "endpoint": "https://zoo.example.com", "state": "active"},
		},
	}
	setupTransferMaster(t, fake)

	// With --yes, succeeds without interactive input
	stdout, stderr, err := runSatelliteTransfer(t, nil, "sat-1", "--to", "bob@example.com", "--yes")
	if err != nil {
		t.Fatalf("--yes failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "satellite: sat-1") {
		t.Fatalf("missing satellite ID in output: %s", stdout)
	}
	if hits := fake.transferHits(); hits != 1 {
		t.Fatalf("expected 1 transfer POST, got %d", hits)
	}
}

func TestSatelliteTransferNoInputWithoutYes(t *testing.T) {
	isolatedFleetRoot(t)
	fake := &fakeTransferMaster{
		rows: []map[string]any{
			{"id": "sat-1", "name": "zoo", "endpoint": "https://zoo.example.com", "state": "active"},
		},
	}
	setupTransferMaster(t, fake)

	// Even with interactive input provided, --no-input without --yes must fail before mutation
	in := testInteractiveInput{strings.NewReader("y\n")}
	_, _, err := runSatelliteTransfer(t, in, "sat-1", "--to", "bob@example.com", "--no-input")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("expected --yes required error for --no-input, got: %v", err)
	}
	if hits := fake.transferHits(); hits != 0 {
		t.Fatalf("assert zero transfer POSTs when --no-input lacks --yes, got %d", hits)
	}
}

func TestSatelliteTransferNoninteractiveStdin(t *testing.T) {
	isolatedFleetRoot(t)
	fake := &fakeTransferMaster{
		rows: []map[string]any{
			{"id": "sat-1", "name": "zoo", "endpoint": "https://zoo.example.com", "state": "active"},
		},
	}
	setupTransferMaster(t, fake)

	// Plain strings.Reader (noninteractive stdin) with "y\n" but absence of --yes
	in := strings.NewReader("y\n")
	_, _, err := runSatelliteTransfer(t, in, "sat-1", "--to", "bob@example.com")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("expected --yes required error for noninteractive stdin, got: %v", err)
	}
	if hits := fake.transferHits(); hits != 0 {
		t.Fatalf("assert zero transfer POSTs when noninteractive stdin lacks --yes, got %d", hits)
	}
}

func TestSatelliteTransferAmbiguousTarget(t *testing.T) {
	isolatedFleetRoot(t)
	fake := &fakeTransferMaster{
		rows: []map[string]any{
			{"id": "sat-1", "name": "dup", "endpoint": "https://a.example.com", "state": "active"},
			{"id": "sat-2", "name": "dup", "endpoint": "https://b.example.com", "state": "active"},
		},
	}
	setupTransferMaster(t, fake)

	_, _, err := runSatelliteTransfer(t, nil, "dup", "--to", "bob@example.com", "--yes")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous error, got: %v", err)
	}
	if hits := fake.transferHits(); hits != 0 {
		t.Fatalf("assert zero transfer POSTs on ambiguous target, got %d", hits)
	}
}

func TestSatelliteTransferServerRefusal(t *testing.T) {
	isolatedFleetRoot(t)
	fake := &fakeTransferMaster{
		rows: []map[string]any{
			{"id": "sat-1", "name": "zoo", "endpoint": "https://zoo.example.com", "state": "active"},
		},
		transferStatus: http.StatusForbidden,
		transferErr:    "not authorized to transfer this satellite",
	}
	setupTransferMaster(t, fake)

	_, _, err := runSatelliteTransfer(t, nil, "sat-1", "--to", "bob@example.com", "--yes")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected 403 server refusal, got: %v", err)
	}
	// Assert zero retries: exactly 1 POST made
	if hits := fake.transferHits(); hits != 1 {
		t.Fatalf("assert exactly 1 transfer POST (no automatic retry), got %d", hits)
	}
}

func TestSatelliteTransferExactIDWinsOverName(t *testing.T) {
	isolatedFleetRoot(t)
	fake := &fakeTransferMaster{
		rows: []map[string]any{
			{"id": "sat-1", "name": "sat-2", "endpoint": "https://a.example.com", "state": "active"},
			{"id": "sat-2", "name": "other", "endpoint": "https://b.example.com", "state": "active"},
		},
	}
	setupTransferMaster(t, fake)

	// "sat-2" matches sat-2 by exact ID, even though sat-1 has name "sat-2"
	_, _, err := runSatelliteTransfer(t, nil, "sat-2", "--to", "bob@example.com", "--yes")
	if err != nil {
		t.Fatalf("exact ID transfer failed: %v", err)
	}
	if fake.lastPostID != "sat-2" {
		t.Fatalf("exact ID sat-2 should have won, but transferred %q", fake.lastPostID)
	}
	if hits := fake.transferHits(); hits != 1 {
		t.Fatalf("expected 1 transfer POST, got %d", hits)
	}
}

func TestSatelliteTransferMissingTarget(t *testing.T) {
	isolatedFleetRoot(t)
	fake := &fakeTransferMaster{
		rows: []map[string]any{
			{"id": "sat-1", "name": "zoo", "endpoint": "https://zoo.example.com", "state": "active"},
		},
	}
	setupTransferMaster(t, fake)

	_, _, err := runSatelliteTransfer(t, nil, "nonexistent", "--to", "bob@example.com", "--yes")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got: %v", err)
	}
	if hits := fake.transferHits(); hits != 0 {
		t.Fatalf("assert zero transfer POSTs on missing target, got %d", hits)
	}
}

func TestSatelliteTransferInvalidEmail(t *testing.T) {
	isolatedFleetRoot(t)
	fake := &fakeTransferMaster{
		rows: []map[string]any{
			{"id": "sat-1", "name": "zoo", "endpoint": "https://zoo.example.com", "state": "active"},
		},
	}
	setupTransferMaster(t, fake)

	_, _, err := runSatelliteTransfer(t, nil, "sat-1", "--to", "invalid-email", "--yes")
	if err == nil || !strings.Contains(err.Error(), "invalid --to email") {
		t.Fatalf("expected invalid email error, got: %v", err)
	}
	if hits := fake.transferHits(); hits != 0 {
		t.Fatalf("assert zero transfer POSTs on invalid email, got %d", hits)
	}
}

func TestSatelliteTransferMissingTo(t *testing.T) {
	isolatedFleetRoot(t)
	fake := &fakeTransferMaster{
		rows: []map[string]any{
			{"id": "sat-1", "name": "zoo", "endpoint": "https://zoo.example.com", "state": "active"},
		},
	}
	setupTransferMaster(t, fake)

	_, _, err := runSatelliteTransfer(t, nil, "sat-1", "--yes")
	if err == nil || !strings.Contains(err.Error(), "needs --to") {
		t.Fatalf("expected needs --to error, got: %v", err)
	}
	if hits := fake.transferHits(); hits != 0 {
		t.Fatalf("assert zero transfer POSTs on missing --to, got %d", hits)
	}
}

func TestSatelliteTransferTargetResolvedByName(t *testing.T) {
	isolatedFleetRoot(t)
	fake := &fakeTransferMaster{
		rows: []map[string]any{
			{"id": "sat-99", "name": "unique-name", "endpoint": "https://u.example.com", "state": "active"},
		},
	}
	setupTransferMaster(t, fake)

	_, _, err := runSatelliteTransfer(t, nil, "unique-name", "--to", "bob@example.com", "--yes")
	if err != nil {
		t.Fatalf("unique name transfer failed: %v", err)
	}
	if fake.lastPostID != "sat-99" {
		t.Fatalf("expected sat-99, got %q", fake.lastPostID)
	}
	if hits := fake.transferHits(); hits != 1 {
		t.Fatalf("expected 1 transfer POST, got %d", hits)
	}
}
