package sdlc

import "testing"

func TestValidPromotionOrdering(t *testing.T) {
	cfg := Config{} // default order dev -> qa -> prod
	cases := []struct {
		from, to string
		ok       bool
	}{
		{"", "dev", true},      // first env, no --from
		{"dev", "dev", true},   // idempotent re-promote to first
		{"dev", "qa", true},    // legal single step
		{"qa", "prod", true},   // legal single step
		{"dev", "prod", false}, // skip qa -> rejected
		{"", "qa", false},      // must come from dev
		{"qa", "dev", false},   // backwards
		{"dev", "staging", false},
	}
	for _, c := range cases {
		got, why := ValidPromotion(cfg, c.from, c.to)
		if got != c.ok {
			t.Errorf("ValidPromotion(%q→%q) = %v (%q), want %v", c.from, c.to, got, why, c.ok)
		}
	}
}

func TestEnvOrderCustom(t *testing.T) {
	cfg := Config{Deploy: DeploymentConfig{Order: []string{"dev", "stage", "prod"}}}
	if ok, _ := ValidPromotion(cfg, "dev", "stage"); !ok {
		t.Error("custom order dev→stage should be valid")
	}
	if ok, _ := ValidPromotion(cfg, "dev", "qa"); ok {
		t.Error("qa is not in the custom order; must be rejected")
	}
}
