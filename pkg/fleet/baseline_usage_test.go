package fleet

import (
	"strings"
	"testing"
	"time"
)

// TestBaselineUsageSeeds pins the compiled-in usage probes (story ad63edf1):
// every seed parses through the real baseline loader — validateUsage has
// already checked probe ids, the metric/window vocabulary and that every
// metric expression is compilable jq — and each carries the declared shape:
// read-only plan probes over local capture files, consented net probes over
// documented balance endpoints with a model:credential reference.
func TestBaselineUsageSeeds(t *testing.T) {
	c := New(WithRoot(t.TempDir()), WithoutCloudOverlay())

	plan, ok := c.Plan("claude-max")
	if !ok || len(plan.Usage) != 1 {
		t.Fatalf("claude-max usage: %d probes (found=%v)", len(plan.Usage), ok)
	}
	claude := plan.Usage[0]
	if claude.ID != "statusline" || len(claude.Run.Effects) != 1 || claude.Run.Effects[0] != "read" {
		t.Fatalf("claude-max probe: %+v", claude)
	}
	if len(claude.Metrics) != 2 ||
		claude.Metrics[0].Name != "used_percent" || claude.Metrics[0].Window != "5h" ||
		claude.Metrics[1].Name != "used_percent" || claude.Metrics[1].Window != "week" {
		t.Fatalf("claude-max metrics: %+v", claude.Metrics)
	}

	chatgpt, ok := c.Plan("chatgpt-pro")
	if !ok || len(chatgpt.Usage) != 1 {
		t.Fatalf("chatgpt-pro usage: %d probes (found=%v)", len(chatgpt.Usage), ok)
	}
	if p := chatgpt.Usage[0]; p.ID != "sessions" || len(p.Run.Effects) != 1 || p.Run.Effects[0] != "read" || p.Run.Timeout != 10*time.Second || len(p.Metrics) != 0 {
		t.Fatalf("chatgpt-pro probe: %+v", p)
	}

	// The balance seeds are identical across each vendor's seats and resolve
	// the key through the model record, never a literal.
	balanceSeed := func(t *testing.T, name, url string) {
		t.Helper()
		m, ok := c.Model(name)
		if !ok {
			t.Fatalf("model %s missing", name)
		}
		if len(m.Usage) != 1 {
			t.Fatalf("%s usage: %d probes", name, len(m.Usage))
		}
		p := m.Usage[0]
		if p.ID != "balance" || len(p.Run.Effects) != 2 || p.Run.Effects[0] != "read" || p.Run.Effects[1] != "net" {
			t.Fatalf("%s probe: %+v", name, p)
		}
		if p.Run.Env["KEY"] != "model:credential" {
			t.Fatalf("%s env: %+v", name, p.Run.Env)
		}
		if len(p.Metrics) != 1 || p.Metrics[0].Name != "balance" || p.Metrics[0].Window != "" || p.Metrics[0].Reset != "" {
			t.Fatalf("%s metrics: %+v", name, p.Metrics)
		}
		if !strings.Contains(p.Run.Body, url) {
			t.Fatalf("%s body does not hit %s: %q", name, url, p.Run.Body)
		}
	}
	for _, name := range []string{"deepseek-v4-pro", "deepseek-v4-flash", "deepseek-v4.1-flash"} {
		balanceSeed(t, name, "https://api.deepseek.com/user/balance")
	}
	for _, name := range []string{"kimi-k2.6", "kimi-k2.7-code", "kimi-k2.7-code-highspeed", "kimi-k3"} {
		balanceSeed(t, name, "https://api.moonshot.ai/v1/users/me/balance")
	}

	// z.ai publishes no quota/usage endpoint (checked docs.z.ai's full API
	// reference 2026-10-15), and muse/google have none either: those plans
	// carry no probe rather than a guess.
	for _, name := range []string{"zai-glm-coding-pro", "meta-muse-pro", "google-ai-ultra"} {
		if plan, ok := c.Plan(name); ok && len(plan.Usage) != 0 {
			t.Fatalf("%s unexpectedly carries usage probes: %+v", name, plan.Usage)
		}
	}
}
