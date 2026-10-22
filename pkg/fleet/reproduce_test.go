package fleet

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `host ls` + `reproduce --plan` (S490 story 7107fb14): thin read-only
// clients over the master Bearer [REDACTED] The master is an httptest fake
// speaking the real envelopes ({"hosts": [...]}, {"peers": [...]},
// {"requests": [...]}, bare-array tokens); no cloudbox is needed and no
// cloud resource is touched.

type reproFakeMaster struct {
	srv *httptest.Server
}

func (f *reproFakeMaster) provRow() map[string]any {
	return map[string]any{
		"id": "req-1", "owner": "owner@example.com",
		"idempotency_key": "infra-provision-zoo",
		"provider":        "digitalocean",
		"region":          "sfo3", "size": "s-4vcpu-8gb",
		"image": "ubuntu-24-04-x64",
		"name":  "zoo", "satellite_id": "zoo", "profile": "standard",
		"state": "installed", "needs_token": false, "resource_id": "123",
	}
}

func (f *reproFakeMaster) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+"owner-session" {
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
		row := f.provRow()
		other := f.provRow()
		other["id"] = "req-9"
		other["satellite_id"] = "ivy"
		other["name"] = "ivy"
		write(200, map[string]any{"requests": []any{row, other}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/hosts":
		write(200, map[string]any{"hosts": []any{
			map[string]any{
				"host": "dragon", "os_user": "qiangli", "title": "dragon",
				"hostname": "dragon", "online": true, "apps": []any{},
				"builtins": map[string]any{},
			},
		}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/peers":
		write(200, map[string]any{"peers": []any{
			map[string]any{
				"host": "dragon", "owned": true, "online": true,
				"location": "same_lan", "version": "v1", "os": "darwin", "arch": "arm64",
			},
		}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tokens":
		write(200, []any{
			map[string]any{
				"id": "tok-1", "name": "agent-a", "scopes": []any{"ssh:read"},
				"expires_at": "2027-01-01T00:00:00Z",
			},
		})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func setupReproFake(t *testing.T) {
	t.Helper()
	f := &reproFakeMaster{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	t.Setenv("BASHY_CLOUDBOX_URL", f.srv.URL)
	t.Setenv("BASHY_FLEET_TOKEN", "owner-session")
	t.Setenv("BASHY_API_KEY", "")
}

func runHostLs(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewPairedHostCmd()
	cmd.SetArgs(args)
	var out strings.Builder
	cmd.SetOut(&out)
	err := cmd.Execute()
	return out.String(), err
}

func runReproduce(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewReproduceCmd()
	cmd.SetArgs(args)
	var out strings.Builder
	cmd.SetOut(&out)
	err := cmd.Execute()
	return out.String(), err
}

func TestHostLsTableAndJSON(t *testing.T) {
	setupReproFake(t)

	out, err := runHostLs(t, "ls")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	for _, want := range []string{"dragon", "same_lan", "darwin/arm64"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ls output missing %q:\n%s", want, out)
		}
	}

	jout, err := runHostLs(t, "ls", "--json")
	if err != nil {
		t.Fatalf("ls --json: %v", err)
	}
	var env struct {
		Hosts []HostEntry `json:"hosts"`
		Peers []PeerEntry `json:"peers"`
	}
	if err := json.Unmarshal([]byte(jout), &env); err != nil {
		t.Fatalf("ls --json is not JSON: %v:\n%s", err, jout)
	}
	if len(env.Hosts) != 1 || env.Hosts[0].Host != "dragon" {
		t.Fatalf("unexpected hosts: %+v", env.Hosts)
	}
	if len(env.Peers) != 1 || env.Peers[0].Location != "same_lan" {
		t.Fatalf("unexpected peers: %+v", env.Peers)
	}
}

func TestHostLsSatelliteRefused(t *testing.T) {
	setupReproFake(t)

	_, err := runHostLs(t, "ls", "--satellite", "zoo")
	if err == nil {
		t.Fatalf("ls --satellite succeeded; want the missing-mapping refusal")
	}
	if !strings.Contains(err.Error(), "/api/v1/satellites/assignment") {
		t.Fatalf("refusal does not name the missing endpoint: %v", err)
	}
}

func TestJoinPairedKeepsHostWithoutPeer(t *testing.T) {
	rows := joinPaired(
		[]HostEntry{{Host: "lonely", Online: false}},
		[]PeerEntry{{Host: "other", Location: "remote"}},
	)
	if len(rows) != 1 || rows[0].Host != "lonely" {
		t.Fatalf("host without a peers row vanished: %+v", rows)
	}
	if rows[0].Location != "" {
		t.Fatalf("unexpected location fill: %+v", rows[0])
	}
}

func TestReproduceNeedsPlan(t *testing.T) {
	setupReproFake(t)

	if _, err := runReproduce(t); err == nil {
		t.Fatalf("bare reproduce succeeded; want the plan-only refusal")
	}
	if _, err := runReproduce(t, "--apply"); err == nil {
		t.Fatalf("reproduce --apply succeeded; there is no --apply in this run")
	} else if !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("want cobra's unknown-flag refusal, got: %v", err)
	}
	if _, err := runReproduce(t, "--plan", "--satellite", "zoo", "--all"); err == nil {
		t.Fatalf("--satellite with --all succeeded; want the exclusivity refusal")
	}
}

func TestReproducePlanJSON(t *testing.T) {
	setupReproFake(t)

	jout, err := runReproduce(t, "--plan", "--json")
	if err != nil {
		t.Fatalf("--plan --json: %v", err)
	}
	var plan ReproducePlan
	if err := json.Unmarshal([]byte(jout), &plan); err != nil {
		t.Fatalf("--plan --json is not JSON: %v:\n%s", err, jout)
	}
	if plan.Scope != "all" {
		t.Fatalf("scope = %q, want all", plan.Scope)
	}
	kinds := map[string]int{}
	for _, a := range plan.Actions {
		kinds[a.Kind]++
		if a.Via == "" || a.From.Endpoint == "" || a.From.Record == "" {
			t.Fatalf("action missing via/from: %+v", a)
		}
	}
	if kinds["provision"] != 2 || kinds["pairing"] != 1 || kinds["token"] != 1 {
		t.Fatalf("unexpected action mix: %v", kinds)
	}
	if len(plan.Gaps) == 0 {
		t.Fatalf("plan has no gaps; uncovered dimensions must be listed")
	}
	sawSatellite, sawRegistry := false, false
	for _, g := range plan.Gaps {
		if strings.Contains(g.MissingEndpoint, "/api/v1/satellites") {
			sawSatellite = true
		}
		if strings.Contains(g.MissingEndpoint, "registry") {
			sawRegistry = true
		}
	}
	if !sawSatellite || !sawRegistry {
		t.Fatalf("gaps do not name the missing satellite/registry endpoints: %+v", plan.Gaps)
	}
	if strings.Contains(jout, "owner-session") {
		t.Fatalf("plan leaks the caller credential")
	}
}

func TestReproducePlanSatelliteScope(t *testing.T) {
	setupReproFake(t)

	jout, err := runReproduce(t, "--plan", "--satellite", "zoo", "--json")
	if err != nil {
		t.Fatalf("--plan --satellite: %v", err)
	}
	var plan ReproducePlan
	if err := json.Unmarshal([]byte(jout), &plan); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if plan.Scope != "zoo" {
		t.Fatalf("scope = %q, want zoo", plan.Scope)
	}
	for _, a := range plan.Actions {
		if a.Kind == "provision" && !strings.Contains(a.Resource, "zoo") {
			t.Fatalf("unfiltered provision action under --satellite zoo: %+v", a)
		}
		if a.Kind != "provision" && !strings.Contains(a.Note, "unattributed") {
			t.Fatalf("non-provision action lacks the unattributed note: %+v", a)
		}
	}
}

func TestReproducePlanHuman(t *testing.T) {
	setupReproFake(t)

	out, err := runReproduce(t, "--plan", "--all")
	if err != nil {
		t.Fatalf("--plan --all: %v", err)
	}
	for _, want := range []string{"scope: all", "actions:", "gaps:", "satellite/zoo", "host/dragon", "token/agent-a"} {
		if !strings.Contains(out, want) {
			t.Fatalf("human plan missing %q:\n%s", want, out)
		}
	}
}
