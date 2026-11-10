package broker

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPeerMeterTokensDayAndRollover(t *testing.T) {
	m := newPeerMeter()
	now := time.Date(2026, 11, 10, 23, 59, 0, 0, time.UTC)
	lim := PeerLimits{TokensDay: 10}
	if r := m.admit("peer:a", lim, now); r != nil {
		t.Fatalf("fresh peer refused: %+v", r)
	}
	m.observe("peer:a", 9, 0, now)
	if r := m.admit("peer:a", lim, now); r != nil {
		t.Fatalf("under cap refused: %+v", r)
	}
	m.observe("peer:a", 9, 0, now) // the accepted in-flight overshoot
	r := m.admit("peer:a", lim, now)
	if r == nil || r.Limit != LimitTokensDay || r.Used != 18 || r.Cap != 10 || !r.Reset.Equal(time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("over cap = %+v", r)
	}
	if r := m.admit("peer:b", lim, now); r != nil {
		t.Fatalf("other peer refused: %+v", r)
	}
	if r := m.admit("peer:a", lim, now.Add(2*time.Minute)); r != nil {
		t.Fatalf("new UTC day still refused: %+v", r)
	}
}

func TestPeerMeterUSDMonth(t *testing.T) {
	m := newPeerMeter()
	now := time.Date(2026, 11, 30, 12, 0, 0, 0, time.UTC)
	lim := PeerLimits{USDMonth: 1.5}
	m.observe("peer:a", 0, 1.5, now)
	r := m.admit("peer:a", lim, now)
	if r == nil || r.Limit != LimitUSDMonth || !r.Reset.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("over usd cap = %+v", r)
	}
	if r := m.admit("peer:a", lim, time.Date(2026, 12, 1, 0, 0, 1, 0, time.UTC)); r != nil {
		t.Fatalf("new month still refused: %+v", r)
	}
}

func TestPeerMeterRPMSlidingWindow(t *testing.T) {
	m := newPeerMeter()
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	lim := PeerLimits{RPM: 2}
	for i := 0; i < 2; i++ {
		if r := m.admit("peer:a", lim, t0.Add(time.Duration(i)*10*time.Second)); r != nil {
			t.Fatalf("request %d refused: %+v", i, r)
		}
	}
	r := m.admit("peer:a", lim, t0.Add(30*time.Second))
	if r == nil || r.Limit != LimitRPM || !r.Reset.Equal(t0.Add(time.Minute)) {
		t.Fatalf("third request in a minute = %+v", r)
	}
	// The first admission leaves the window at t0+1m; one slot frees.
	if r := m.admit("peer:a", lim, t0.Add(61*time.Second)); r != nil {
		t.Fatalf("slot not freed: %+v", r)
	}
	if r := m.admit("peer:a", lim, t0.Add(62*time.Second)); r == nil {
		t.Fatal("window over-admitted")
	}
}

func TestPeerMeterSeedFromAudit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	lines := []string{
		`{"time":"2026-10-31T23:00:00Z","principal":"peer:a","prompt_tokens":1000,"completion_tokens":1000,"cost_usd":9}`,
		`{"time":"2026-11-09T10:00:00Z","principal":"peer:a","prompt_tokens":100,"completion_tokens":10,"cost_usd":0.5}`,
		`{"time":"2026-11-10T01:00:00Z","principal":"peer:a","prompt_tokens":7,"completion_tokens":3,"cached_tokens":4,"cost_usd":0.25}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newPeerMeter()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if err := m.seed(path, now); err != nil {
		t.Fatal(err)
	}
	if m.tokens["peer:a"] != 10 || m.usd["peer:a"] != 0.75 {
		t.Fatalf("seeded tokens=%d usd=%v, want today's 10 and the month's 0.75", m.tokens["peer:a"], m.usd["peer:a"])
	}
	if err := newPeerMeter().seed(filepath.Join(dir, "missing.jsonl"), now); err != nil {
		t.Fatalf("missing audit log: %v", err)
	}
}

func TestBrokerStartsWithMalformedAudit(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "audit.jsonl"), []byte("{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) { o.StateDir = state })
	if h.b.meter == nil || len(h.b.meter.tokens) != 0 {
		t.Fatal("malformed audit did not leave empty counters")
	}
	resp, _ := h.do("GET", "/health", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("door not serving: %d", resp.StatusCode)
	}
}

func TestSetPeerLimitsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.json")
	now := time.Now().UTC()
	if err := SetPeerLimits(path, "ghost", PeerLimits{RPM: 1}, now); err == nil {
		t.Fatal("limit on an unknown peer accepted")
	}
	if _, err := MintPeerCredential(path, "a", ShareAllow, now); err != nil {
		t.Fatal(err)
	}
	if err := SetPeerLimits(path, "a", PeerLimits{USDMonth: -1}, now); err == nil {
		t.Fatal("negative limit accepted")
	}
	want := PeerLimits{TokensDay: 5000, USDMonth: 2.5, RPM: 30}
	if err := SetPeerLimits(path, "a", want, now); err != nil {
		t.Fatal(err)
	}
	ps, _ := LoadPeerShares(path)
	if got := ps.Peers["a"].Limits; got == nil || *got != want || ps.Peers["a"].Share != ShareAllow {
		t.Fatalf("stored = %+v", ps.Peers["a"])
	}
	if err := SetPeerLimits(path, "a", PeerLimits{}, now); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); bytes.Contains(data, []byte(`"limits"`)) {
		t.Fatalf("cleared limits still stored:\n%s", data)
	}
}

func TestPeerLimitCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.json")
	old := peersPath
	peersPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { peersPath = old })
	if _, err := MintPeerCredential(path, "a", ShareAllow, time.Now()); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) error {
		cmd := newPeerCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(append([]string{"limit", "a"}, args...))
		return cmd.Execute()
	}
	if err := run(); err == nil {
		t.Fatal("limit with no flag accepted")
	}
	if err := run("--tokens-day", "100", "--rpm", "5"); err != nil {
		t.Fatal(err)
	}
	if err := run("--usd-month", "3"); err != nil { // only the given flag changes
		t.Fatal(err)
	}
	ps, _ := LoadPeerShares(path)
	if got := ps.Peers["a"].Limits; got == nil || *got != (PeerLimits{TokensDay: 100, USDMonth: 3, RPM: 5}) {
		t.Fatalf("limits = %+v", got)
	}
	if err := run("--clear"); err != nil {
		t.Fatal(err)
	}
	ps, _ = LoadPeerShares(path)
	if ps.Peers["a"].Limits != nil {
		t.Fatalf("--clear left %+v", ps.Peers["a"].Limits)
	}
}

// TestPeerDailyCapTwoPeers is the sprint integration: two peers on one door,
// a fake backend reporting fixed usage (2 prompt + 1 completion), peer one
// capped at 5 tokens/day. Peer one's third request is refused 429 with the
// reason and reset; peer two keeps working; llm usage --json matches.
func TestPeerDailyCapTwoPeers(t *testing.T) {
	h, _, peers := peerHarness(t)
	now := time.Now().UTC()
	cred1, err := MintPeerCredential(peers, "one", ShareAllow, now)
	if err != nil {
		t.Fatal(err)
	}
	cred2, err := MintPeerCredential(peers, "two", ShareAllow, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetPeerLimits(peers, "one", PeerLimits{TokensDay: 5}, now); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ { // 3 then 6 tokens: the second is the accepted overshoot
		if code, body := peerPost(t, h, "/v1/chat/completions", cred1, nil); code != http.StatusOK {
			t.Fatalf("peer one request %d = %d %s", i, code, body)
		}
	}
	req, _ := http.NewRequest("POST", h.server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"q"}]}`))
	req.Header.Set("Authorization", "Bearer "+cred1)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" || resp.Header.Get("X-Bashy-Refusal") != "peer-limit" {
		t.Fatalf("over cap = %d %v %s", resp.StatusCode, resp.Header, raw)
	}
	var refusal struct {
		Type, Principal, Limit, Reset string
		Used, Cap                     float64
	}
	if err := json.Unmarshal(raw, &refusal); err != nil {
		t.Fatal(err)
	}
	wantReset := dayStart(time.Now()).AddDate(0, 0, 1).Format(time.RFC3339)
	if refusal.Type != "peer_limit" || refusal.Principal != "peer:one" || refusal.Limit != LimitTokensDay || refusal.Used != 6 || refusal.Cap != 5 || refusal.Reset != wantReset {
		t.Fatalf("refusal = %+v (want reset %s)", refusal, wantReset)
	}
	if code, body := peerPost(t, h, "/v1/chat/completions", cred2, nil); code != http.StatusOK {
		t.Fatalf("peer two = %d %s", code, body)
	}

	old := usageAuditPath
	usageAuditPath = func() (string, error) { return filepath.Join(h.b.opts.StateDir, "audit.jsonl"), nil }
	t.Cleanup(func() { usageAuditPath = old })
	var out bytes.Buffer
	cmd := newUsageCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var rep usageReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	got := map[string]PrincipalUsage{}
	for _, row := range rep.Principals {
		got[row.Principal] = row
	}
	if r := got["peer:one"]; r.Requests != 3 || r.PromptTokens != 4 || r.CompletionTokens != 2 {
		t.Errorf("peer:one usage = %+v, want 3 requests (one refused), 4 prompt, 2 completion", r)
	}
	if r := got["peer:two"]; r.Requests != 1 || r.PromptTokens != 2 || r.CompletionTokens != 1 {
		t.Errorf("peer:two usage = %+v", r)
	}
}

// usd/month is metered on the door's A2 cost_usd, not on a price of its own:
// a peer asking for a model the llmprice table prices (claude-opus-4-8, 2 in
// + 1 out ≈ $0.000105) trips a $0.0001 monthly cap on its second request.
func TestPeerUSDMonthTripsOnPricedModel(t *testing.T) {
	h, _, peers := peerHarness(t)
	now := time.Now().UTC()
	cred, err := MintPeerCredential(peers, "one", ShareAllow, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetPeerLimits(peers, "one", PeerLimits{USDMonth: 0.0001}, now); err != nil {
		t.Fatal(err)
	}
	post := func() (int, []byte) {
		req, _ := http.NewRequest("POST", h.server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"q"}]}`))
		req.Header.Set("Authorization", "Bearer "+cred)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}
	if code, raw := post(); code != http.StatusOK {
		t.Fatalf("first request = %d %s", code, raw)
	}
	if rec := h.lastRecord(1); rec.Unpriced || rec.CostUSD <= 0 {
		t.Fatalf("peer record not priced by llmprice: %+v", rec)
	}
	code, raw := post()
	if code != http.StatusTooManyRequests {
		t.Fatalf("over usd cap = %d %s", code, raw)
	}
	var refusal struct {
		Limit, Reset string
		Used, Cap    float64
	}
	if err := json.Unmarshal(raw, &refusal); err != nil {
		t.Fatal(err)
	}
	want := (2*15.0 + 1*75.0) / 1e6
	if refusal.Limit != LimitUSDMonth || refusal.Cap != 0.0001 || refusal.Used < want-1e-12 || refusal.Used > want+1e-12 {
		t.Fatalf("refusal = %+v, want usd_month used %.9f cap 0.0001", refusal, want)
	}
	if wantReset := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339); refusal.Reset != wantReset {
		t.Errorf("reset = %s, want %s", refusal.Reset, wantReset)
	}
}
