package broker

import (
	"context"
	"sync"
	"testing"
)

// The identities the live door froze before effort existed (Sprint 290/322
// pairing: 113fd8e2 opus5, 6b2c0425 gpt-5.5). Their digests were computed by
// the code BEFORE Identity gained Effort and are pinned here: an undeclared
// effort must add nothing to the canonical JSON, or every running benchmark's
// binding would silently re-key.
func TestIdentityDigestWithoutEffortIsUnchanged(t *testing.T) {
	cases := []struct {
		id   Identity
		want string
	}{
		{Identity{Backend: BackendCLI, Location: "local", Model: "opus5", Agent: "claude-opus5", Tool: "claude",
			ToolVersion: "2.1.283 (Claude Code)", VendorModel: "claude-opus-5", Provider: "anthropic",
			Launch: "cligw-pure-completion/stdin-stream-json"},
			"sha256:113fd8e29968378eee1d8c859df3e55b66fbffaaaae067e78d7e64680da63f84"},
		{Identity{Backend: BackendCLI, Location: "local", Model: "gpt-5.5", Agent: "codex-gpt-5.5", Tool: "codex",
			ToolVersion: "codex-cli 0.157.1", VendorModel: "gpt-5.5", Provider: "openai",
			Launch: "cligw-pure-completion/cold"},
			"sha256:6b2c042522c5fd956ed4e0865e090b9785cc9dd641625dc6ba46fd2c4b5498f8"},
		{Identity{Backend: BackendLocal, Location: "local", Model: "gpt-oss:20b",
			ModelDigest: "17052f91a42e97930aa6e28a6c6c06a983e6a58dbb00434885a0cf5313e376f7",
			Options:     map[string]any{"num_ctx": float64(32768)}},
			"sha256:26300c5708bd8468950e222e11cbf68ef89443dd7e62851bffacb9f3ddeea843"},
	}
	for _, c := range cases {
		if got := c.id.Digest(); got != c.want {
			t.Errorf("%s: digest %s, want the pre-effort %s", c.id.Model, got, c.want)
		}
	}
}

func TestIdentityDigestDiffersByDeclaredEffort(t *testing.T) {
	base := Identity{Backend: BackendCLI, Location: "local", Model: "opus5", Agent: "claude-opus5", Tool: "claude",
		ToolVersion: "2.1.283 (Claude Code)", VendorModel: "claude-opus-5", Provider: "anthropic",
		Launch: "cligw-pure-completion/stdin-stream-json"}
	high, low := base, base
	high.Effort, low.Effort = "high", "low"
	seen := map[string]string{}
	for _, id := range []Identity{base, high, low} {
		d := id.Digest()
		if prev, dup := seen[d]; dup {
			t.Fatalf("effort %q and %q share digest %s", prev, id.Effort, d)
		}
		seen[d] = id.Effort
	}
}

// effortCLI is fakeCLI whose agent declares a (changeable) effort and which
// answers the live-binding lookup, as cligw's adapter does.
type effortCLI struct {
	*fakeCLI
	mu     sync.Mutex
	effort string
}

func (c *effortCLI) set(e string) { c.mu.Lock(); c.effort = e; c.mu.Unlock() }

func (c *effortCLI) info() AgentInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return AgentInfo{Name: "claude-opus5", Tool: "claude", Model: "opus5", VendorModel: "claude-opus-5",
		Provider: "anthropic", Warm: "stdin-stream-json", Effort: c.effort, Band: 5}
}

func (c *effortCLI) ResolveAgent(ctx context.Context, model, filter string) (AgentInfo, error) {
	if _, err := c.fakeCLI.ResolveAgent(ctx, model, filter); err != nil {
		return AgentInfo{}, err
	}
	return c.info(), nil
}

func (c *effortCLI) LookupAgent(name string) (AgentInfo, bool) {
	if name != "claude-opus5" {
		return AgentInfo{}, false
	}
	return c.info(), true
}

// A binding frozen before the agent's effort was declared (or changed) must be
// refused after the change — the same 409 a CLI upgrade gets — and a new key
// resolves to a different digest.
func TestStickyRefusedAfterEffortChange(t *testing.T) {
	var cli *effortCLI
	h := newHarness(t, func(o *Options) {
		cli = &effortCLI{fakeCLI: o.CLI.(*fakeCLI)}
		o.CLI = cli
	})
	resp, out := h.do("POST", "/v1/sticky", StickySpec{Key: "arm-a", Model: "L5"}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d %v", resp.StatusCode, out)
	}
	before, _ := out["digest"].(string)
	if resp, _ := h.do("POST", "/sticky/arm-a/v1/chat/completions", chat("x"), nil); resp.StatusCode != 200 {
		t.Fatalf("served before the change: %d", resp.StatusCode)
	}

	cli.set("high")
	resp, out = h.do("POST", "/sticky/arm-a/v1/chat/completions", chat("x"), nil)
	if resp.StatusCode != 409 {
		t.Fatalf("after effort change: %d %v, want 409", resp.StatusCode, out)
	}
	resp, out = h.do("POST", "/v1/sticky", StickySpec{Key: "arm-b", Model: "L5"}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("create with effort: %d %v", resp.StatusCode, out)
	}
	if after, _ := out["digest"].(string); after == before || after == "" {
		t.Fatalf("digest with effort %q = digest without %q", after, before)
	}
	if id, _ := out["identity"].(map[string]any); id["effort"] != "high" {
		t.Fatalf("identity effort = %v", id["effort"])
	}
	// Re-creating arm-a now resolves differently: a key conflict, not a reroute.
	if resp, _ := h.do("POST", "/v1/sticky", StickySpec{Key: "arm-a", Model: "L5"}, nil); resp.StatusCode != 409 {
		t.Fatalf("re-create arm-a: %d, want 409", resp.StatusCode)
	}
	// Reverting the effort serves the original binding exactly again.
	cli.set("")
	if resp, _ := h.do("POST", "/sticky/arm-a/v1/chat/completions", chat("x"), nil); resp.StatusCode != 200 {
		t.Fatalf("after revert: %d", resp.StatusCode)
	}
}
