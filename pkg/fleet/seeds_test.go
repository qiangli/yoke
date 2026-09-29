package fleet

import (
	"testing"

	"github.com/qiangli/yoke/pkg/assetring"
)

// The embedded ring ships SEEDS: a default model + agent roster with band
// pegs, resolvable with nothing mounted above it. This is what a process that
// does not inherit the operator's shell exports — a headless worker, a daemon,
// a fresh host — routes on; before the seeds it saw an empty fleet and every
// band picker failed closed. TestMain strips the ring env, so only the
// compiled-in baseline is under test here.
func TestEmbeddedSeedsResolve(t *testing.T) {
	c := New(WithRoot(t.TempDir()), WithoutCloudOverlay())

	models, errs := c.Models()
	if len(errs) != 0 {
		t.Fatalf("seed models parse: %v", errs)
	}
	if len(models) < 45 {
		t.Fatalf("seed models = %d, want >= 45", len(models))
	}
	for _, m := range models {
		if m.Ring != assetring.RingEmbedded {
			t.Errorf("model %q ring = %v, want embedded", m.Name, m.Ring)
		}
		if m.Band < 1 || m.Band > MaxBand {
			t.Errorf("model %q band = %d, want 1..%d — an unpegged seed cannot be routed", m.Name, m.Band, MaxBand)
		}
	}

	agents, errs := c.Agents()
	if len(errs) != 0 {
		t.Fatalf("seed agents parse: %v", errs)
	}
	if len(agents) < 62 {
		t.Fatalf("seed agents = %d, want >= 62", len(agents))
	}
	for _, a := range agents {
		if _, _, _, err := c.Binding(a.Name); err != nil {
			t.Errorf("seed agent %q does not resolve against the embedded ring alone: %v", a.Name, err)
		}
	}

	// The family aliases are derived at load (highest version wins, a tie
	// fails closed); every family the seeds ship must resolve to one model.
	for _, alias := range []string{"opus", "sonnet", "haiku", "fable", "gpt", "gemini", "gemini-flash", "deepseek", "glm", "kimi", "kimi-code"} {
		if _, ok := c.Model(alias); !ok {
			t.Errorf("family alias %q does not resolve — a seed family is missing or its versions tie", alias)
		}
	}
}

// Sprint #328 story #1195: the two-season peg expiry runs on model.band_season,
// so every embedded peg must carry its season. A peg with no season is
// unclocked: it holds forever and never expires once an operator advances
// ladder.yaml.
func TestEmbeddedPegsCarryASeason(t *testing.T) {
	c := New(WithRoot(t.TempDir()), WithoutCloudOverlay())

	models, errs := c.Models()
	if len(errs) != 0 {
		t.Fatalf("seed models parse: %v", errs)
	}
	for _, m := range models {
		if m.Band > 0 && m.BandSeason <= 0 {
			t.Errorf("model %q band L%d has no band_season — an unclocked peg never expires", m.Name, m.Band)
		}
	}
}

// BASHY_FLEET_SEEDS=off drops the seeded roster and nothing else: the tool
// launch contracts are the mechanism and stay embedded. This is the switch
// fleettest.Ring relies on, and what an org that publishes its own catalog
// sets on its hosts.
func TestSeedsSwitchDropsRosterNotContracts(t *testing.T) {
	t.Setenv(SeedsEnv, "off")
	c := New(WithRoot(t.TempDir()), WithoutCloudOverlay())
	if models, _ := c.Models(); len(models) != 0 {
		t.Fatalf("seeds off: models = %d, want 0", len(models))
	}
	if agents, _ := c.Agents(); len(agents) != 0 {
		t.Fatalf("seeds off: agents = %d, want 0", len(agents))
	}
	tools, errs := c.Tools(true)
	if len(errs) != 0 || len(tools) == 0 {
		t.Fatalf("seeds off must keep the tool launch contracts: %d tools, errs %v", len(tools), errs)
	}
}

// Sprint #328 story #1220: the current roster ships as seeds, not as local
// recipes an operator must hand-copy onto every host. Each row pins the
// binding (tool:model) and the id the TOOL is handed — the opencode ids are
// opencode's own `provider/model` spelling (`opencode models zai-coding-plan`,
// opencode 1.18.30), because a bare `glm-5.3` is a dead binding there.
func TestEmbeddedSeedsCurrentRoster(t *testing.T) {
	c := New(WithRoot(t.TempDir()), WithoutCloudOverlay())

	// The id is a property of the tool, so it lives on the model, not on an
	// agent: any opencode binding of these models must be handed it.
	for name, want := range map[string]string{
		"glm-5.3":       "zai-coding-plan/glm-5.3",
		"glm-5.3-flash": "zai-coding-plan/glm-5.3-flash",
	} {
		if m, ok := c.Model(name); !ok {
			t.Errorf("model %q is not seeded", name)
		} else if got := m.TargetFor("opencode"); got != want {
			t.Errorf("model %q: opencode is handed %q, want %q", name, got, want)
		}
	}

	for _, tc := range []struct {
		agent, tool, model, target string
	}{
		// The current Claude/Codex/Agy/Muse agents were seeded earlier; they
		// are pinned here so the roster cannot silently lose one.
		{"claude-opus5.5", "claude", "opus5.5", ""},
		{"claude-sonnet5.5", "claude", "sonnet5.5", ""},
		{"claude-fable5.1", "claude", "fable5.1", ""},
		{"claude-haiku4.5", "claude", "haiku4.5", ""},
		{"codex-gpt6-astra", "codex", "gpt6-astra", ""},
		{"codex-gpt6-sol", "codex", "gpt6-sol", ""},
		{"codex-gpt6-luna", "codex", "gpt6-luna", ""},
		{"agy-gemini3.8-flash", "agy", "gemini3.8-flash", ""},
		{"muse-spark1.3", "muse", "muse-spark1.3", ""},

		{"opencode-glm-5.3", "opencode", "glm-5.3", "zai-coding-plan/glm-5.3"},
		{"opencode-glm-5.3-flash", "opencode", "glm-5.3-flash", "zai-coding-plan/glm-5.3-flash"},
		{"genie-glm-5.3", "genie", "glm-5.3", "glm-5.3"},
		{"genie-gpt-5.5", "genie", "door-codex-gpt-5.5", "door-codex-gpt-5.5"},
		{"genie-opus5", "genie", "door-claude-opus5", "door-claude-opus5"},
		{"genie-gemini3.8-flash", "genie", "door-agy-gemini3.8-flash", "door-agy-gemini3.8-flash"},
		{"genie-muse-spark1.3", "genie", "door-muse-spark1.3", "door-muse-spark1.3"},
	} {
		a, tool, m, err := c.Binding(tc.agent)
		if err != nil {
			t.Errorf("seed agent %q: %v", tc.agent, err)
			continue
		}
		if a.Ring != assetring.RingEmbedded {
			t.Errorf("agent %q ring = %v, want embedded", tc.agent, a.Ring)
		}
		if tool.Name != tc.tool || m.Name != tc.model {
			t.Errorf("agent %q binding = %s:%s, want %s:%s", tc.agent, tool.Name, m.Name, tc.tool, tc.model)
		}
		if tc.target != "" {
			if got := m.TargetFor(tool.Name); got != tc.target {
				t.Errorf("agent %q: %s is handed %q, want %q", tc.agent, tool.Name, got, tc.target)
			}
		}
	}
}

// The cligw model door is a stable loopback (127.0.0.1:24556); each door
// model is an openai-compat endpoint on a sticky per-agent session, keyed by
// the bashy-llm owner token. These were local recipes; seeding them must
// keep their semantics exactly — a door model that is not openai-compat on
// the loopback is a different binding wearing the same name.
func TestEmbeddedSeedsDoorModels(t *testing.T) {
	c := New(WithRoot(t.TempDir()), WithoutCloudOverlay())

	for _, tc := range []struct{ name, sticky, upstream string }{
		{"door-codex-gpt-5.5", "genie-gpt-5.5", "codex-gpt-5.5"},
		{"door-claude-opus5", "genie-opus5", "claude-opus5"},
		{"door-agy-gemini3.8-flash", "genie-gemini3.8-flash", "agy-gemini3.8-flash"},
		{"door-muse-spark1.3", "genie-muse-spark1.3", "muse-spark1.3"},
	} {
		m, ok := c.Model(tc.name)
		if !ok {
			t.Errorf("door model %q is not seeded", tc.name)
			continue
		}
		if m.Name != tc.name || m.Ring != assetring.RingEmbedded {
			t.Errorf("door model %q resolved to %q ring %v, want itself, embedded", tc.name, m.Name, m.Ring)
		}
		wantURL := "http://127.0.0.1:24556/sticky/" + tc.sticky + "/v1"
		if m.Provider != "openai-compat" || m.BaseURL != wantURL || m.APIKeyRef != "bashy-llm" {
			t.Errorf("door model %q = provider %q base_url %q api_key_ref %q, want openai-compat %q bashy-llm",
				tc.name, m.Provider, m.BaseURL, m.APIKeyRef, wantURL)
		}
		if m.Kind != "api" || m.Source != ModelSourceCloud || m.BillingMode() != "flat_then_metered" {
			t.Errorf("door model %q = kind %q source %q billing %q, want api cloud flat_then_metered",
				tc.name, m.Kind, m.Source, m.BillingMode())
		}
		if m.UpstreamID != tc.upstream || m.TargetFor("genie") != tc.name {
			t.Errorf("door model %q = upstream %q genie id %q, want %q / %q",
				tc.name, m.UpstreamID, m.TargetFor("genie"), tc.upstream, tc.name)
		}
		// A door model proxies a family member; it must not capture that
		// family's floating alias (`opus`, `gpt`, ...).
		if m.Family != "" {
			t.Errorf("door model %q family = %q, want none", tc.name, m.Family)
		}
	}
}
