package fleet

// `satellite list` / `satellite show` (S598 story 299f93c16dd2): the master
// is an httptest fake speaking the real GET /api/v1/satellites envelope
// ({"satellites": [...]}); no cloudbox is needed. The fake records every
// method it sees so the suite can prove reads issue no mutations.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeSatelliteMaster struct {
	mu       sync.Mutex
	rows     []map[string]any
	status   int
	raw      string
	seen     []string
	token    string
	noAuthOK bool
}

func (f *fakeSatelliteMaster) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	if !f.noAuthOK && r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != http.MethodGet || r.URL.Path != "/api/v1/satellites" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.raw != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(f.raw))
		return
	}
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	if status/100 != 2 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": http.StatusText(status)})
		return
	}
	rows := f.rows
	if rows == nil {
		rows = []map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"satellites": rows})
}

func (f *fakeSatelliteMaster) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func setupSatelliteMaster(t *testing.T, fake *fakeSatelliteMaster) {
	t.Helper()
	if fake.token == "" {
		fake.token = "owner-session"
	}
	srv := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(srv.Close)
	t.Setenv("BASHY_CLOUDBOX_URL", srv.URL)
	t.Setenv("BASHY_FLEET_TOKEN", fake.token)
	t.Setenv("BASHY_API_KEY", "")
}

func runSatellite(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewSatelliteCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func satRow(id, name, endpoint, state string) map[string]any {
	return map[string]any{"id": id, "name": name, "endpoint": endpoint, "state": state}
}

func TestSatelliteListPopulated(t *testing.T) {
	isolatedFleetRoot(t)
	fake := &fakeSatelliteMaster{rows: []map[string]any{
		satRow("sat-1", "zoo", "https://zoo.example.com", "active"),
		satRow("sat-2", "farm", "https://farm.example.com", "degraded"),
	}}
	setupSatelliteMaster(t, fake)

	out, err := runSatellite(t, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"sat-1", "zoo", "https://zoo.example.com", "active", "sat-2", "farm"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output missing %q:\n%s", want, out)
		}
	}

	lsOut, err := runSatellite(t, "ls")
	if err != nil {
		t.Fatalf("ls alias: %v", err)
	}
	if lsOut != out {
		t.Fatalf("ls alias output differs from list:\n%s\n---\n%s", lsOut, out)
	}

	jout, err := runSatellite(t, "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	var docs struct {
		Satellites []SatelliteEntry `json:"satellites"`
	}
	if err := json.Unmarshal([]byte(jout), &docs); err != nil {
		t.Fatalf("list --json is not JSON: %v:\n%s", err, jout)
	}
	if len(docs.Satellites) != 2 || docs.Satellites[0].ID != "sat-1" {
		t.Fatalf("unexpected list: %+v", docs)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(jout), &raw); err != nil {
		t.Fatalf("list --json envelope: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("list --json must emit only the satellites envelope, got keys: %v", raw)
	}
}

func TestSatelliteListEmpty(t *testing.T) {
	isolatedFleetRoot(t)
	setupSatelliteMaster(t, &fakeSatelliteMaster{})

	out, err := runSatellite(t, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "no satellites") {
		t.Fatalf("empty list should say so:\n%s", out)
	}

	jout, err := runSatellite(t, "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	if !strings.Contains(jout, `"satellites": []`) {
		t.Fatalf("empty list --json must carry an empty array:\n%s", jout)
	}
}

func TestSatelliteShowByIDAndName(t *testing.T) {
	isolatedFleetRoot(t)
	setupSatelliteMaster(t, &fakeSatelliteMaster{rows: []map[string]any{
		satRow("sat-1", "zoo", "https://zoo.example.com", "active"),
		satRow("sat-2", "farm", "https://farm.example.com", "degraded"),
	}})

	byID, err := runSatellite(t, "show", "sat-1")
	if err != nil {
		t.Fatalf("show by id: %v", err)
	}
	for _, want := range []string{"id: sat-1", "name: zoo", "endpoint: https://zoo.example.com", "state: active"} {
		if !strings.Contains(byID, want) {
			t.Fatalf("show by id missing %q:\n%s", want, byID)
		}
	}

	byName, err := runSatellite(t, "show", "farm")
	if err != nil {
		t.Fatalf("show by name: %v", err)
	}
	if !strings.Contains(byName, "id: sat-2") {
		t.Fatalf("show by name picked the wrong record:\n%s", byName)
	}

	jout, err := runSatellite(t, "show", "sat-1", "--json")
	if err != nil {
		t.Fatalf("show --json: %v", err)
	}
	var docs struct {
		Satellite map[string]any `json:"satellite"`
	}
	if err := json.Unmarshal([]byte(jout), &docs); err != nil {
		t.Fatalf("show --json is not JSON: %v:\n%s", err, jout)
	}
	if docs.Satellite["id"] != "sat-1" || docs.Satellite["name"] != "zoo" ||
		docs.Satellite["endpoint"] != "https://zoo.example.com" || docs.Satellite["state"] != "active" {
		t.Fatalf("unexpected show record: %+v", docs.Satellite)
	}
	if len(docs.Satellite) != 4 {
		t.Fatalf("show must expose only id/name/endpoint/state, got: %v", docs.Satellite)
	}
}

func TestSatelliteShowIDWinsOverName(t *testing.T) {
	isolatedFleetRoot(t)
	setupSatelliteMaster(t, &fakeSatelliteMaster{rows: []map[string]any{
		satRow("zoo", "first", "https://first.example.com", "active"),
		satRow("other", "zoo", "https://zoo.example.com", "active"),
	}})

	out, err := runSatellite(t, "show", "zoo")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(out, "id: zoo") || !strings.Contains(out, "name: first") {
		t.Fatalf("exact ID must win over an equal name:\n%s", out)
	}
}

func TestSatelliteShowDuplicateNames(t *testing.T) {
	isolatedFleetRoot(t)
	setupSatelliteMaster(t, &fakeSatelliteMaster{rows: []map[string]any{
		satRow("sat-1", "dup", "https://a.example.com", "active"),
		satRow("sat-2", "dup", "https://b.example.com", "active"),
	}})

	if _, err := runSatellite(t, "show", "dup"); err == nil ||
		!strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("duplicate names: err = %v, want an ambiguity refusal", err)
	}
}

func TestSatelliteShowMissing(t *testing.T) {
	isolatedFleetRoot(t)
	setupSatelliteMaster(t, &fakeSatelliteMaster{rows: []map[string]any{
		satRow("sat-1", "zoo", "https://zoo.example.com", "active"),
	}})

	if _, err := runSatellite(t, "show", "nope"); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing record: err = %v, want a not-found refusal", err)
	}

	if _, err := runSatellite(t, "show", ""); err == nil {
		t.Fatalf("empty key succeeded; want a refusal")
	}
}

func TestSatelliteListAuthFailure(t *testing.T) {
	isolatedFleetRoot(t)
	fake := &fakeSatelliteMaster{rows: []map[string]any{satRow("sat-1", "zoo", "https://zoo.example.com", "active")}}
	setupSatelliteMaster(t, fake)
	t.Setenv("BASHY_FLEET_TOKEN", "wrong-token")

	if _, err := runSatellite(t, "list"); err == nil ||
		!strings.Contains(err.Error(), "401") {
		t.Fatalf("unauthorized: err = %v, want the 401 refusal", err)
	}
	if _, err := runSatellite(t, "show", "sat-1"); err == nil ||
		!strings.Contains(err.Error(), "401") {
		t.Fatalf("unauthorized show: err = %v, want the 401 refusal", err)
	}
}

func TestSatelliteListMalformed(t *testing.T) {
	isolatedFleetRoot(t)
	setupSatelliteMaster(t, &fakeSatelliteMaster{raw: "{not json"})

	if _, err := runSatellite(t, "list"); err == nil ||
		!strings.Contains(err.Error(), "decode") {
		t.Fatalf("malformed body: err = %v, want a decode refusal", err)
	}
	if _, err := runSatellite(t, "show", "sat-1"); err == nil ||
		!strings.Contains(err.Error(), "decode") {
		t.Fatalf("malformed body show: err = %v, want a decode refusal", err)
	}
}

func TestSatelliteEndpointUnavailable(t *testing.T) {
	isolatedFleetRoot(t)
	setupSatelliteMaster(t, &fakeSatelliteMaster{
		status:   http.StatusNotFound,
		noAuthOK: true,
	})

	if _, err := runSatellite(t, "list"); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing endpoint: err = %v, want the honest refusal", err)
	}
}

func TestSatelliteEndpointUnreachable(t *testing.T) {
	isolatedFleetRoot(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	t.Setenv("BASHY_CLOUDBOX_URL", url)
	t.Setenv("BASHY_FLEET_TOKEN", "owner-session")
	t.Setenv("BASHY_API_KEY", "")

	if _, err := runSatellite(t, "list"); err == nil {
		t.Fatalf("unreachable master succeeded; want the connection refusal")
	}
}

func TestSatelliteReadsIssueNoMutations(t *testing.T) {
	isolatedFleetRoot(t)
	fake := &fakeSatelliteMaster{rows: []map[string]any{
		satRow("sat-1", "zoo", "https://zoo.example.com", "active"),
	}}
	setupSatelliteMaster(t, fake)

	for _, args := range [][]string{
		{"list"}, {"list", "--json"}, {"ls"},
		{"show", "sat-1"}, {"show", "zoo"}, {"show", "sat-1", "--json"},
	} {
		if _, err := runSatellite(t, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	for _, seen := range fake.methods() {
		if !strings.HasPrefix(seen, "GET ") {
			t.Fatalf("read verbs must issue no mutations, saw %q", seen)
		}
	}
}
