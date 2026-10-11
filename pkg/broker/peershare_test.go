package broker

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
