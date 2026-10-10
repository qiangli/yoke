package cligw

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/llmgw/gateway"
	"github.com/qiangli/yoke/pkg/llmgw/sched"
)

// TestServeHelpNamesCapAndBenchmarkLanes pins the door-docs half: serve
// help tells a benchmark to give each lane its own principal or raise the
// cap, and names every knob that raises it.
func TestServeHelpNamesCapAndBenchmarkLanes(t *testing.T) {
	cmd := NewCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"serve", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--max-in-flight", "max_in_flight", "CLIGW_MAX_IN_FLIGHT", "own principal"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("serve help missing %q:\n%s", want, out.String())
		}
	}
}

// TestPolicyAdmissionLimitDefaults pins the historical cap: a zero Policy
// keeps the gateway default of 4.
func TestPolicyAdmissionLimitDefaults(t *testing.T) {
	p := DefaultPolicy()
	if got := p.AdmissionLimit("owner"); got != gateway.DefaultMaxInFlight {
		t.Fatalf("zero policy cap = %d, want gateway default %d", got, gateway.DefaultMaxInFlight)
	}
	if gateway.DefaultMaxInFlight != 4 {
		t.Fatalf("gateway default = %d, want the historical 4", gateway.DefaultMaxInFlight)
	}
}

// TestPolicyAdmissionLimitOverrides: the door default applies to every
// seat, and a per-principal entry wins for its own principal only.
func TestPolicyAdmissionLimitOverrides(t *testing.T) {
	p := DefaultPolicy()
	p.MaxInFlight = 8
	p.MaxInFlightByPrincipal = map[string]int{"owner": 2}
	if got := p.AdmissionLimit("owner"); got != 2 {
		t.Fatalf("owner cap = %d, want the per-principal 2", got)
	}
	if got := p.AdmissionLimit("lane-2"); got != 8 {
		t.Fatalf("lane-2 cap = %d, want the door default 8", got)
	}
}

// TestPolicyFileMaxInFlight: the config-file spellings land in the Policy.
func TestPolicyFileMaxInFlight(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	body := "default: quota-first\nreserve_floor: 0.15\nmax_in_flight: 8\nmax_in_flight_by_principal:\n  owner: 2\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPolicyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.MaxInFlight != 8 {
		t.Fatalf("max_in_flight = %d, want 8", p.MaxInFlight)
	}
	if got := p.AdmissionLimit("owner"); got != 2 {
		t.Fatalf("owner cap = %d, want 2", got)
	}
	if got := p.AdmissionLimit("lane-2"); got != 8 {
		t.Fatalf("lane-2 cap = %d, want 8", got)
	}
}

// TestPolicyRejectsBadMaxInFlight: a negative default and a non-positive
// or nameless seat override fail loudly instead of silently widening (or
// closing) the door.
func TestPolicyRejectsBadMaxInFlight(t *testing.T) {
	for _, tweak := range []func(*Policy){
		func(p *Policy) { p.MaxInFlight = -1 },
		func(p *Policy) { p.MaxInFlightByPrincipal = map[string]int{"owner": 0} },
		func(p *Policy) { p.MaxInFlightByPrincipal = map[string]int{"": 2} },
	} {
		p := DefaultPolicy()
		tweak(&p)
		if err := p.Validate(); err == nil {
			t.Fatalf("Validate(%+v) = nil, want an error", p)
		}
	}
}

// TestServerOptionsMaxInFlightPrecedence: the serve flag/env override wins
// over the policy file, and per-seat overrides win per key.
func TestServerOptionsMaxInFlightPrecedence(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	catalog := installServerFleet(t)
	quota := &fakeQuota{headroom: map[string]float64{"sonnet-x": .8}, refused: map[string]string{}}
	policy := DefaultPolicy()
	policy.MaxInFlight = 7
	policy.MaxInFlightByPrincipal = map[string]int{"owner": 3}
	server, err := NewServer(ServerOptions{
		Catalog: catalog,
		Policy:  &policy,
		Quota:   quota,
		Breaker: sched.NewBreaker(),
		Pool:    PoolConfig{StartServers: 0, MinSpare: 0, MaxSpare: 1, MaxWorkers: 2, IdleTTL: time.Minute},
		// As --max-in-flight would: the default override and one seat.
		MaxInFlight:            9,
		MaxInFlightByPrincipal: map[string]int{"owner": 5},
		Logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if got := server.admissionLimit("owner"); got != 5 {
		t.Fatalf("owner cap = %d, want the ServerOptions seat override 5", got)
	}
	if got := server.admissionLimit("lane-2"); got != 9 {
		t.Fatalf("lane-2 cap = %d, want the ServerOptions default 9", got)
	}

	// Without overrides the policy file rules.
	plain, err := NewServer(ServerOptions{
		Catalog: catalog,
		Policy:  &policy,
		Quota:   quota,
		Breaker: sched.NewBreaker(),
		Pool:    PoolConfig{StartServers: 0, MinSpare: 0, MaxSpare: 1, MaxWorkers: 2, IdleTTL: time.Minute},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Close() })
	if got := plain.admissionLimit("owner"); got != 3 {
		t.Fatalf("owner cap = %d, want the policy seat override 3", got)
	}
	if got := plain.admissionLimit("lane-2"); got != 7 {
		t.Fatalf("lane-2 cap = %d, want the policy default 7", got)
	}
}

// TestServerQuota429LandsInUsageLog pins the story's visibility half: a
// quota 429 is written to usage.jsonl as a refusal event carrying the
// principal, model, reason, limit and in-flight count.
func TestServerQuota429LandsInUsageLog(t *testing.T) {
	ts := newTestServer(t, map[string]float64{"sonnet-x": .05, "gpt-x": .05})

	resp := ts.do(t, http.MethodPost, "/v1/chat/completions", ts.Token(), fmt.Sprintf(chatBody, "L4"))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("exhausted band = %s, want 429: %s", resp.Status, body)
	}

	var found *RefusalRecord
	for _, line := range readUsageLog(t) {
		var rec RefusalRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec.SchemaVersion == RefusalSchemaVersion {
			found = &rec
		}
	}
	if found == nil {
		t.Fatal("usage.jsonl carries no refusal event for the quota 429")
	}
	if found.Principal != Principal || found.Model != "L4" || found.Status != http.StatusTooManyRequests {
		t.Fatalf("refusal = %+v, want owner/L4/429", found)
	}
	if !strings.Contains(found.Reason, "reserve floor") {
		t.Fatalf("refusal reason = %q, want the quota cause", found.Reason)
	}
	if found.Limit != gateway.DefaultMaxInFlight {
		t.Fatalf("refusal limit = %d, want the default cap %d", found.Limit, gateway.DefaultMaxInFlight)
	}
}

// TestMaxInFlightFromEnv: empty keeps the policy default, a positive
// integer parses, anything else fails loudly.
func TestMaxInFlightFromEnv(t *testing.T) {
	t.Setenv("CLIGW_MAX_IN_FLIGHT", "")
	if n, err := maxInFlightFromEnv(); err != nil || n != 0 {
		t.Fatalf("empty env = %d, %v; want 0, nil", n, err)
	}
	t.Setenv("CLIGW_MAX_IN_FLIGHT", "8")
	if n, err := maxInFlightFromEnv(); err != nil || n != 8 {
		t.Fatalf("env 8 = %d, %v; want 8, nil", n, err)
	}
	for _, bad := range []string{"0", "-3", "many"} {
		t.Setenv("CLIGW_MAX_IN_FLIGHT", bad)
		if _, err := maxInFlightFromEnv(); err == nil {
			t.Fatalf("env %q parsed without error", bad)
		}
	}
}
