package fleet

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/assetring"
)

var planDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Story 5fec08b11a5f: every seeded plan is dated and sourced, every seeded
// model that names a plan resolves it, and every recorded limit carries its
// own evidence, date and source — nothing is recorded without one.
func TestEmbeddedPlansAreDatedSourcedAndResolved(t *testing.T) {
	c := New(WithRoot(t.TempDir()), WithoutCloudOverlay())
	plans, errs := c.Plans()
	if len(errs) != 0 {
		t.Fatalf("seed plans parse: %v", errs)
	}
	if len(plans) == 0 {
		t.Fatal("no seeded plans")
	}
	byName := map[string]Plan{}
	for _, p := range plans {
		byName[p.Name] = p
		if p.Ring != assetring.RingEmbedded {
			t.Errorf("plan %q ring = %v, want embedded", p.Name, p.Ring)
		}
		if p.Rank() == 0 {
			t.Errorf("plan %q has no normalized tier", p.Name)
		}
		if !planDate.MatchString(p.AsOf) {
			t.Errorf("plan %q as_of = %q, want YYYY-MM-DD", p.Name, p.AsOf)
		}
		if len(p.Sources) == 0 {
			t.Errorf("plan %q has no source", p.Name)
		}
		for _, s := range p.Sources {
			if !strings.HasPrefix(s, "https://") {
				t.Errorf("plan %q source %q is not an https vendor page", p.Name, s)
			}
		}
		for i, l := range p.Limits {
			if l.Evidence == "" || !planDate.MatchString(l.AsOf) || l.Source == "" {
				t.Errorf("plan %q limit %d lacks evidence/date/source: %+v", p.Name, i, l)
			}
		}
	}

	models, _ := c.Models()
	withPlan := 0
	for _, m := range models {
		if m.Plan == "" {
			continue
		}
		withPlan++
		if _, ok := c.ModelPlan(m); !ok {
			t.Errorf("model %q names plan %q, which does not resolve", m.Name, m.Plan)
		}
		if m.BillingMode() == BillingMetered {
			t.Errorf("model %q is metered but names a subscription plan", m.Name)
		}
	}
	if withPlan == 0 {
		t.Fatal("no seeded model names a plan")
	}

	// The operator's seats as recorded by the story: the Claude and Codex seats
	// are their vendors' highest plans; the Agy, Muse and GLM seats are Pro.
	for model, tier := range map[string]string{
		"opus5.5": PlanTierMax, "gpt6-sol": PlanTierMax,
		"gemini3.8-flash": PlanTierPro, "muse-spark1.3": PlanTierPro, "glm-5.3": PlanTierPro,
		// Served by agy, so billed through the Google seat, not Anthropic's.
		"opus4.6": PlanTierPro,
	} {
		m, ok := c.Model(model)
		if !ok {
			t.Errorf("seed model %q missing", model)
			continue
		}
		p, ok := c.ModelPlan(m)
		if !ok || p.Tier != tier {
			t.Errorf("model %q plan %q tier = %q, want %q", model, m.Plan, p.Tier, tier)
		}
	}
	// Metered API seats have no plan: unknown, not guessed.
	if m, ok := c.Model("kimi-k3"); ok && m.Plan != "" {
		t.Errorf("metered kimi-k3 names plan %q", m.Plan)
	}
}

func TestPlanTierRankOrdersAndUnknownIsZero(t *testing.T) {
	prev := 0
	for _, tier := range PlanTiers() {
		r := PlanTierRank(tier)
		if r <= prev {
			t.Fatalf("tier %q rank %d not above %d", tier, r, prev)
		}
		prev = r
	}
	for _, tier := range []string{"", "Pro", "ultra"} {
		if r := PlanTierRank(tier); r != 0 {
			t.Errorf("PlanTierRank(%q) = %d, want 0 (unknown)", tier, r)
		}
	}
}

func TestPlanLimitPrefersModelScopeAndUnknownIsAbsent(t *testing.T) {
	p := Plan{Name: "seat", Limits: []PlanLimit{
		{Window: PlanWindowWeek, Unit: PlanUnitTokens, Value: 100, Source: "x"},
		{Window: PlanWindowWeek, Unit: PlanUnitTokens, Value: 40, Model: "small", Source: "x"},
	}}
	if v, ok := p.Limit(PlanWindowWeek, PlanUnitTokens, "small"); !ok || v != 40 {
		t.Fatalf("model-scoped limit = %d %v", v, ok)
	}
	if v, ok := p.Limit(PlanWindowWeek, PlanUnitTokens, "other"); !ok || v != 100 {
		t.Fatalf("plan-wide limit = %d %v", v, ok)
	}
	if _, ok := p.Limit(PlanWindowDay, PlanUnitTokens, "small"); ok {
		t.Fatal("an unrecorded limit reported as known")
	}
}

func TestParsePlanRejectsUnsourcedOrUnknownVocabulary(t *testing.T) {
	for name, body := range map[string]string{
		"tier":     "tier: ultra\n",
		"window":   "limits:\n  - {window: hour, unit: tokens, value: 1, source: x}\n",
		"unit":     "limits:\n  - {window: week, unit: dollars, value: 1, source: x}\n",
		"source":   "limits:\n  - {window: week, unit: tokens, value: 1}\n",
		"value":    "limits:\n  - {window: week, unit: tokens, value: 0, source: x}\n",
		"evidence": "limits:\n  - {window: week, unit: tokens, value: 1, source: x, evidence: guessed}\n",
	} {
		if _, err := ParsePlan("p", []byte(body), nil); err == nil {
			t.Errorf("%s: ParsePlan accepted %q", name, body)
		}
	}
	if _, err := ParsePlan("p", []byte("tier: max\n"), nil); err != nil {
		t.Fatalf("minimal plan rejected: %v", err)
	}
}

// A seat change is data: a same-named plan in the local store replaces the seed.
func TestLocalPlanReplacesSeed(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, dirPlans)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude-max.yaml"), []byte("name: claude-max\ntier: pro\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(WithRoot(root), WithoutCloudOverlay())
	p, ok := c.Plan("claude-max")
	if !ok || p.Tier != PlanTierPro || p.Ring != assetring.RingLocal {
		t.Fatalf("local plan = %+v %v", p, ok)
	}
}
