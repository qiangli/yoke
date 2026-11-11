package broker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/boundary"
	"github.com/qiangli/yoke/pkg/cligw"
	"github.com/qiangli/yoke/pkg/toolcmd"
)

// capCLI is a fake cligw that answers a capability request the way the real
// slash path does: a chat completion plus the typed envelope and selection.
type capCLI struct {
	*fakeCLI
	mu       sync.Mutex
	hits     int
	lastCap  string
	lastCall string
	lastAuth string
}

func (c *capCLI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	capText := r.Header.Get(cligw.CapabilityHeader)
	if capText == "" {
		c.fakeCLI.ServeHTTP(w, r)
		return
	}
	c.mu.Lock()
	c.hits++
	c.lastCap, c.lastCall, c.lastAuth = capText, r.Header.Get(cligw.CallerHeader), r.Header.Get("Authorization")
	c.mu.Unlock()
	var req struct {
		Messages []struct{ Content string } `json:"messages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	q := ""
	if len(req.Messages) > 0 {
		q = req.Messages[len(req.Messages)-1].Content
	}
	res := toolcmd.Result{Tool: "claude", Command: "deep-research", Mode: "print", Outcome: toolcmd.OutcomeSuccess,
		Text:      "REPORT for " + q + " [RFC 4193]",
		Selection: &toolcmd.Selection{Capability: capText, Vendor: "claude", Binding: "claude:opus5.5", Command: "deep-research", RemainingQuota: 0.5, MeasuredSamples: 1}}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices":              []map[string]any{{"message": map[string]string{"role": "assistant", "content": res.Text}}},
		"x_bashy_tool_command": res,
	})
}

func peerHarness(t *testing.T) (*harness, *capCLI, string) {
	t.Helper()
	peers := filepath.Join(t.TempDir(), "cligw", "peers.json")
	cli := &capCLI{fakeCLI: &fakeCLI{}}
	h := newHarness(t, func(o *Options) { o.CLI = cli; o.PeersFile = peers })
	return h, cli, peers
}

type memberFixture struct {
	mu   sync.Mutex
	rows []boundary.Member
}

func (f *memberFixture) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"members": f.rows})
}

func (f *memberFixture) set(rows ...boundary.Member) {
	f.mu.Lock()
	f.rows = append([]boundary.Member{}, rows...)
	f.mu.Unlock()
}

func memberPeerHarness(t *testing.T, members MemberAuthorizer) (*harness, string) {
	t.Helper()
	peers := filepath.Join(t.TempDir(), "cligw", "peers.json")
	h := newHarness(t, func(o *Options) {
		o.CLI = &capCLI{fakeCLI: &fakeCLI{}}
		o.PeersFile = peers
		o.Members = members
	})
	return h, peers
}

func TestMemberBoundPeerRemovalCircleOfflineAndLegacy(t *testing.T) {
	clock := time.Now().UTC()
	row := boundary.Member{Email: "bob@example.com", Circle: "family", Status: "active", Mutual: true}
	fixture := &memberFixture{}
	fixture.set(row)
	server := httptest.NewServer(fixture)
	client := &boundary.Client{BaseURL: server.URL, Token: "owner", HTTP: server.Client(), TTL: time.Second, Now: func() time.Time { return clock }}
	h, peers := memberPeerHarness(t, client)

	emailCred, err := MintPeerCredentialFor(peers, "bob", ShareAllow, row.Email, "", clock)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := peerPost(t, h, "/v1/chat/completions", emailCred, nil); code != http.StatusOK {
		t.Fatalf("member peer = %d %s", code, body)
	}
	fixture.set()
	clock = clock.Add(2 * time.Second)
	if code, body := peerPost(t, h, "/v1/chat/completions", emailCred, nil); code != http.StatusForbidden || !strings.Contains(body, "not an active mutual member") {
		t.Fatalf("removed member = %d %s", code, body)
	}

	fixture.set(row)
	clock = clock.Add(2 * time.Second)
	circleCred, err := MintPeerCredentialFor(peers, "family", ShareAllow, row.Email, "family", clock)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := peerPost(t, h, "/v1/chat/completions", circleCred, nil); code != http.StatusOK {
		t.Fatalf("circle peer = %d %s", code, body)
	}
	row.Circle = "team"
	fixture.set(row)
	clock = clock.Add(2 * time.Second)
	if code, body := peerPost(t, h, "/v1/chat/completions", circleCred, nil); code != http.StatusForbidden || !strings.Contains(body, "not an active mutual member") {
		t.Fatalf("changed circle = %d %s", code, body)
	}

	row.Circle = "family"
	fixture.set(row)
	clock = clock.Add(2 * time.Second)
	if code, body := peerPost(t, h, "/v1/chat/completions", circleCred, nil); code != http.StatusOK {
		t.Fatalf("warm member list = %d %s", code, body)
	}
	server.Close()
	clock = clock.Add(time.Hour)
	if code, body := peerPost(t, h, "/v1/chat/completions", circleCred, nil); code != http.StatusOK {
		t.Fatalf("1h offline = %d %s", code, body)
	}
	recs := h.b.audit.snapshot()
	last := recs[len(recs)-1]
	if !last.MemberListOffline || last.MemberListAgeMS == nil || *last.MemberListAgeMS != time.Hour.Milliseconds() {
		t.Fatalf("offline audit = %+v", last)
	}
	clock = clock.Add(24 * time.Hour)
	if code, body := peerPost(t, h, "/v1/chat/completions", circleCred, nil); code != http.StatusForbidden || !strings.Contains(body, "maximum 24h") {
		t.Fatalf("25h offline = %d %s", code, body)
	}

	legacy, err := MintPeerCredential(peers, "legacy", ShareAllow, clock)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := peerPost(t, h, "/v1/chat/completions", legacy, nil); code != http.StatusOK {
		t.Fatalf("legacy peer changed = %d %s", code, body)
	}
}

func TestMemberBoundPeerLimitsStillApply(t *testing.T) {
	row := boundary.Member{Email: "bob@example.com", Circle: "family", Status: "active", Mutual: true}
	fixture := &memberFixture{}
	fixture.set(row)
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	client := &boundary.Client{BaseURL: server.URL, Token: "owner", HTTP: server.Client()}
	h, peers := memberPeerHarness(t, client)
	cred, err := MintPeerCredentialFor(peers, "limited", ShareAllow, row.Email, "family", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := SetPeerLimits(peers, "limited", PeerLimits{RPM: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if code, body := peerPost(t, h, "/v1/chat/completions", cred, nil); code != http.StatusOK {
		t.Fatalf("first request = %d %s", code, body)
	}
	if code, body := peerPost(t, h, "/v1/chat/completions", cred, nil); code != http.StatusTooManyRequests || !strings.Contains(body, LimitRPM) {
		t.Fatalf("second request = %d %s", code, body)
	}
}

type fixedMemberAuthorizer struct{ decision boundary.Decision }

func (f fixedMemberAuthorizer) Decide(context.Context, string, []string) (boundary.Decision, error) {
	return f.decision, nil
}

func TestPeerAddRefusesNonMember(t *testing.T) {
	oldPath, oldClient := peersPath, peerMemberClient
	path := filepath.Join(t.TempDir(), "peers.json")
	peersPath = func() (string, error) { return path, nil }
	peerMemberClient = func() (MemberAuthorizer, error) { return fixedMemberAuthorizer{}, nil }
	t.Cleanup(func() { peersPath, peerMemberClient = oldPath, oldClient })

	cmd := newPeerAddCmd()
	cmd.SetArgs([]string{"bob", "--email", "bob@example.com", "--share", "allow"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not an active mutual member") {
		t.Fatalf("non-member add error = %v", err)
	}
	if ps, err := LoadPeerShares(path); err != nil || len(ps.Peers) != 0 {
		t.Fatalf("refused add wrote a peer: %+v err=%v", ps, err)
	}

	peerMemberClient = func() (MemberAuthorizer, error) {
		return fixedMemberAuthorizer{decision: boundary.Decision{Allowed: true}}, nil
	}
	cmd = newPeerAddCmd()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"bob", "--email", "bob@example.com", "--circle", "family", "--share", "allow"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	ps, err := LoadPeerShares(path)
	if err != nil || ps.Peers["bob"].Email != "bob@example.com" || ps.Peers["bob"].Circle != "family" {
		t.Fatalf("member peer = %+v err=%v", ps.Peers["bob"], err)
	}
}

// A circle is any owner-defined name: llm peer add --circle with a custom
// slug is accepted when the mutual member currently carries that circle.
func TestPeerAddAcceptsCustomCircle(t *testing.T) {
	row := boundary.Member{Email: "bob@example.com", Circle: "hiking-club", Status: "active", Mutual: true}
	fixture := &memberFixture{}
	fixture.set(row)
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)

	oldPath, oldClient := peersPath, peerMemberClient
	path := filepath.Join(t.TempDir(), "peers.json")
	peersPath = func() (string, error) { return path, nil }
	peerMemberClient = func() (MemberAuthorizer, error) {
		return &boundary.Client{BaseURL: server.URL, Token: "owner", HTTP: server.Client()}, nil
	}
	t.Cleanup(func() { peersPath, peerMemberClient = oldPath, oldClient })

	cmd := newPeerAddCmd()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"bob", "--email", "bob@example.com", "--circle", "hiking-club", "--share", "allow"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	ps, err := LoadPeerShares(path)
	if err != nil || ps.Peers["bob"].Circle != "hiking-club" {
		t.Fatalf("custom circle peer = %+v err=%v", ps.Peers["bob"], err)
	}

	fixture.set(boundary.Member{Email: row.Email, Circle: "team", Status: "active", Mutual: true})
	cmd = newPeerAddCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"carol", "--email", "bob@example.com", "--circle", "hiking-club", "--share", "allow"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not an active mutual member") {
		t.Fatalf("stale custom circle add err = %v", err)
	}
}

func peerPost(t *testing.T, h *harness, path, cred string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", h.server.URL+path, strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"q"}]}`))
	req.Header.Set("Authorization", "Bearer "+cred)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestPeerCredentialShareAllowDenyRevoke(t *testing.T) {
	h, cli, peers := peerHarness(t)
	now := time.Now()
	cred, err := MintPeerCredential(peers, "codexhost", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(peers); strings.Contains(string(data), cred) {
		t.Fatal("peers file stores the credential in clear")
	}
	if fi, _ := os.Stat(peers); fi.Mode().Perm() != 0o600 {
		t.Fatalf("peers file mode %v", fi.Mode().Perm())
	}
	capHdr := map[string]string{cligw.CapabilityHeader: "deep-research"}

	// Minted but not shared: deny by default, nothing reaches cligw.
	if code, body := peerPost(t, h, "/v1/chat/completions", cred, capHdr); code != http.StatusForbidden || strings.Contains(body, cred) {
		t.Fatalf("unshared peer: %d %s", code, body)
	}
	if err := SetPeerShare(peers, "codexhost", ShareAllow, now); err != nil {
		t.Fatal(err)
	}
	code, body := peerPost(t, h, "/v1/chat/completions", cred, capHdr)
	if code != http.StatusOK || !strings.Contains(body, "REPORT") {
		t.Fatalf("shared peer: %d %s", code, body)
	}
	cli.mu.Lock()
	if cli.lastAuth != "Bearer cli-token" {
		t.Fatalf("peer credential forwarded to cligw: %q", cli.lastAuth)
	}
	cli.mu.Unlock()
	recs := h.b.audit.snapshot()
	if got := recs[len(recs)-1].Principal; got != "peer:codexhost" {
		t.Fatalf("audit principal = %q, want peer:codexhost", got)
	}

	// A shared peer reaches inference only — never sticky, native or cligw management.
	for _, p := range []string{"/v1/sticky", "/api/pull", "/cligw/health"} {
		if code, _ := peerPost(t, h, p, cred, nil); code != http.StatusForbidden {
			t.Fatalf("peer reached %s: %d", p, code)
		}
	}
	// The Responses route (Codex's only wire since 0.162) is inference: the
	// gate must not 403 it; the broker routes it by model.
	for _, p := range []string{"/v1/responses", "/openai/v1/responses"} {
		if code, body := peerPost(t, h, p, cred, nil); code == http.StatusForbidden || code == http.StatusUnauthorized {
			t.Fatalf("shared peer refused on %s: %d %s", p, code, body)
		}
	}
	if code, _ := peerPost(t, h, "/v1/responses", "not-a-credential", nil); code != http.StatusUnauthorized {
		t.Fatalf("unknown bearer on /v1/responses: %d", code)
	}
	// The owner token is unchanged.
	if code, _ := peerPost(t, h, "/v1/chat/completions", testToken, capHdr); code != http.StatusOK {
		t.Fatalf("owner refused: %d", code)
	}

	if err := SetPeerShare(peers, "codexhost", ShareDeny, now); err != nil {
		t.Fatal(err)
	}
	if code, _ := peerPost(t, h, "/v1/chat/completions", cred, capHdr); code != http.StatusForbidden {
		t.Fatalf("denied peer: %d", code)
	}
	if err := RevokePeer(peers, "codexhost"); err != nil {
		t.Fatal(err)
	}
	if code, _ := peerPost(t, h, "/v1/chat/completions", cred, capHdr); code != http.StatusUnauthorized {
		t.Fatalf("revoked peer: %d", code)
	}
	cli.mu.Lock()
	defer cli.mu.Unlock()
	if cli.hits != 2 {
		t.Fatalf("cligw saw %d capability requests, want 2 (shared peer + owner)", cli.hits)
	}
}

func TestPeerCapabilityRefusedOnSticky(t *testing.T) {
	h, cli, peers := peerHarness(t)
	cred, _ := MintPeerCredential(peers, "p1", ShareAllow, time.Now())
	code, _ := peerPost(t, h, "/sticky/bench/v1/chat/completions", cred, map[string]string{cligw.CapabilityHeader: "deep-research"})
	if code != http.StatusBadRequest {
		t.Fatalf("capability on sticky: %d", code)
	}
	if cli.hits != 0 {
		t.Fatal("sticky capability reached cligw")
	}
}

// TestPeerCapabilityDispatchRoundTrip drives the caller half (toolcmd) at the
// real broker door: the report comes back attributed to the peer's selection.
func TestPeerCapabilityDispatchRoundTrip(t *testing.T) {
	h, cli, peers := peerHarness(t)
	cred, _ := MintPeerCredential(peers, "codexhost", ShareAllow, time.Now())
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	conf, _ := json.Marshal(map[string]any{"peers": map[string]any{"dragon": map[string]string{"url": h.server.URL, "credential": cred}}})
	if err := os.MkdirAll(filepath.Join(home, "toolcmd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "toolcmd", "peers.json"), conf, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := toolcmd.DispatchPeerCapability(t.Context(), "dragon", "deep-research", "codex", "ULA prefixes", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "REPORT for ULA prefixes") || res.Selection == nil || res.Selection.Peer != "dragon" || res.Selection.Vendor != "claude" {
		t.Fatalf("round trip result: %+v", res)
	}
	cli.mu.Lock()
	if cli.lastCall != "codex" || cli.lastCap != "deep-research" {
		t.Fatalf("door saw caller %q capability %q", cli.lastCall, cli.lastCap)
	}
	cli.mu.Unlock()

	if err := RevokePeer(peers, "codexhost"); err != nil {
		t.Fatal(err)
	}
	_, err = toolcmd.DispatchPeerCapability(t.Context(), "dragon", "deep-research", "codex", "again", 0)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), cred) {
		t.Fatalf("revoked dispatch err = %v", err)
	}
}
