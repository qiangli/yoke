package fleet

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeRmMaster struct {
	mu      sync.Mutex
	rows    []map[string]any
	status  int
	errMsg  string
	deletes []string
}

func (f *fakeRmMaster) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer owner-token" {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/satellites":
		_ = json.NewEncoder(w).Encode(map[string]any{"satellites": f.rows})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/satellites/"):
		f.mu.Lock()
		f.deletes = append(f.deletes, r.URL.Path+"?"+r.URL.RawQuery)
		f.mu.Unlock()
		if f.status != 0 && f.status/100 != 2 {
			w.WriteHeader(f.status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": f.errMsg})
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/satellites/")
		_ = json.NewEncoder(w).Encode(map[string]any{"satellite_id": id, "name": "zoo", "state": "lost", "forced": r.URL.Query().Get("force") == "1", "deleted_at": "2026-11-13T12:00:00Z"})
	default:
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
	}
}

func runSatelliteRm(t *testing.T, fake *fakeRmMaster, args ...string) (string, error) {
	t.Helper()
	isolatedFleetRoot(t)
	srv := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(srv.Close)
	t.Setenv("BASHY_CLOUDBOX_URL", srv.URL)
	t.Setenv("BASHY_FLEET_TOKEN", "owner-token")
	t.Setenv("BASHY_API_KEY", "")
	cmd := NewSatelliteCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&strings.Builder{})
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(append([]string{"rm"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestSatelliteRmDryRunCallsNothing(t *testing.T) {
	fake := &fakeRmMaster{rows: []map[string]any{{"id": "sat-1", "name": "zoo", "state": "lost"}}}
	out, err := runSatelliteRm(t, fake, "zoo", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "dry-run: would remove zoo (sat-1, state lost") {
		t.Fatalf("stdout: %q", out)
	}
	if len(fake.deletes) != 0 {
		t.Fatalf("dry-run issued %v", fake.deletes)
	}
}

func TestSatelliteRmNeedsYesWhenNotInteractive(t *testing.T) {
	fake := &fakeRmMaster{rows: []map[string]any{{"id": "sat-1", "name": "zoo", "state": "lost"}}}
	if _, err := runSatelliteRm(t, fake, "sat-1"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("expected --yes refusal, got %v", err)
	}
	if len(fake.deletes) != 0 {
		t.Fatalf("refusal issued %v", fake.deletes)
	}
}

func TestSatelliteRmForceAndJSON(t *testing.T) {
	fake := &fakeRmMaster{rows: []map[string]any{{"id": "sat-1", "name": "zoo", "state": "enabled"}}}
	out, err := runSatelliteRm(t, fake, "sat-1", "--yes", "--force", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.deletes) != 1 || fake.deletes[0] != "/api/v1/satellites/sat-1?force=1" {
		t.Fatalf("deletes: %v", fake.deletes)
	}
	var res SatelliteDeleted
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.SatelliteID != "sat-1" || !res.Forced {
		t.Fatalf("json: %v %q", err, out)
	}
}

func TestSatelliteRmSurfacesRefusal(t *testing.T) {
	fake := &fakeRmMaster{rows: []map[string]any{{"id": "sat-1", "name": "zoo", "state": "enabled"}}, status: http.StatusConflict, errMsg: "satellite delete refused: sat-1 is live"}
	_, err := runSatelliteRm(t, fake, "sat-1", "--yes")
	if err == nil || !strings.Contains(err.Error(), "is live") {
		t.Fatalf("expected refusal surfaced, got %v", err)
	}
}
