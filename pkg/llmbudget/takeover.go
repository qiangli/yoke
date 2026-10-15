package llmbudget

import "fmt"

// TakeoverPolicy controls recovery of unexpectedly stopped supervised workers.
// Pointers distinguish omitted values from explicit zero/false settings.
type TakeoverPolicy struct {
	Mode                      string `json:"mode,omitempty"`
	MaxAutoTakeoversPerStory  *int   `json:"max_auto_takeovers_per_story,omitempty"`
	PreferOtherVendorOnOutage *bool  `json:"prefer_other_vendor_on_outage,omitempty"`
}

func (p TakeoverPolicy) Effective() (TakeoverPolicy, error) {
	if p.Mode == "" {
		p.Mode = "handoff-only"
	}
	if p.Mode != "off" && p.Mode != "handoff-only" && p.Mode != "continue" {
		return p, fmt.Errorf("llmbudget: invalid takeover mode %q", p.Mode)
	}
	if p.MaxAutoTakeoversPerStory == nil {
		n := 1
		p.MaxAutoTakeoversPerStory = &n
	}
	if *p.MaxAutoTakeoversPerStory < 0 {
		return p, fmt.Errorf("llmbudget: negative takeover limit")
	}
	if p.PreferOtherVendorOnOutage == nil {
		b := true
		p.PreferOtherVendorOnOutage = &b
	}
	return p, nil
}

// TakeoverSettings reads the same validated policy used for admission; no probes.
func (g *Gate) TakeoverSettings() (TakeoverPolicy, error) {
	p, err := g.policy()
	if err != nil {
		return TakeoverPolicy{}, err
	}
	return p.Takeover.Effective()
}

// RecoveryBinding resolves billing identity without probing or reading credentials.
func (g *Gate) RecoveryBinding(model, agent string) (Binding, error) {
	p, err := g.policy()
	if err != nil {
		return Binding{}, err
	}
	if b, ok := bindingFor(p, model, agent); ok {
		return b, nil
	}
	r, err := g.prepare(p, Request{Model: model, Agent: agent}, false)
	return requestBinding(r), err
}
