package llmbudget

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

func (g *Gate) policy() (*Policy, error) {
	p := g.cfg.Policy
	if p == nil {
		path := g.cfg.PolicyPath
		if path == "" {
			path = os.Getenv("BASHY_LLM_BUDGET_POLICY")
		}
		explicitPath := path != ""
		if path == "" && g.cfg.StatePath != "" {
			path = filepath.Join(filepath.Dir(g.cfg.StatePath), "llm-budget-policy.json")
		}
		if path == "" {
			return &Policy{Version: 1, missing: true}, nil
		}
		b, err := readBounded(path, 1<<20)
		if os.IsNotExist(err) {
			if explicitPath {
				return nil, errors.New("llmbudget: explicitly configured policy is missing")
			}
			return &Policy{Version: 1, missing: true}, nil
		}
		if err != nil {
			return nil, errors.New("llmbudget: policy unreadable")
		}
		p = new(Policy)
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err = dec.Decode(p); err != nil {
			return nil, errors.New("llmbudget: invalid policy")
		}
		if dec.Decode(new(any)) != io.EOF {
			return nil, errors.New("llmbudget: trailing policy data")
		}
	}
	if p.Version != 1 {
		return nil, errors.New("llmbudget: unsupported policy version")
	}
	if len(p.Bindings) > 10000 || len(p.Constraints) > 10000 || len(p.Sources) > 256 || len(p.Routes) > 10000 {
		return nil, errors.New("llmbudget: policy collection limit exceeded")
	}
	seen := map[string]bool{}
	for _, b := range p.Bindings {
		if !safePolicyStrings(b.Model, b.Agent, b.Provider, b.Account, b.Pool) {
			return nil, errors.New("llmbudget: invalid binding text")
		}
		if b.Model == "" || b.Provider == "" || b.Account == "" || b.Pool == "" || !validLane(b.Lane) {
			return nil, errors.New("llmbudget: binding requires model, provider, account, pool and lane")
		}
		k := b.Model + "\x00" + b.Agent
		if seen[k] {
			return nil, errors.New("llmbudget: duplicate model/agent binding")
		}
		seen[k] = true
	}
	for _, c := range p.Constraints {
		if !safePolicyStrings(c.Provider, c.Account, c.Pool, c.Host) || c.Lane != "" && !validLane(c.Lane) {
			return nil, errors.New("llmbudget: invalid constraint scope")
		}
		for _, v := range []*int64{c.DailyTokens, c.WeeklyTokens, c.DailySpendMicroUSD, c.WeeklySpendMicroUSD} {
			if v != nil && *v < 0 {
				return nil, errors.New("llmbudget: negative limit")
			}
		}
		if c.Concurrency != nil && *c.Concurrency < 0 || c.HostSlots != nil && *c.HostSlots < 0 {
			return nil, errors.New("llmbudget: negative concurrency")
		}
	}
	seen = map[string]bool{}
	for _, r := range p.Routes {
		if r.From == "" || r.To == "" || r.From == r.To || !safePolicyStrings(r.From, r.To) {
			return nil, errors.New("llmbudget: invalid route")
		}
	}
	for _, s := range p.Sources {
		if !safePolicyStrings(s.ID, s.Kind, s.Provider, s.Account, s.Pool, s.Organization, s.CredentialRef, s.Path) || s.RefreshSeconds < 0 || s.RefreshSeconds > 86400 || s.TimeoutSeconds < 0 || s.TimeoutSeconds > 30 {
			return nil, errors.New("llmbudget: invalid source configuration")
		}
		if s.Kind == "openai-organization" {
			if s.Provider != "openai" || s.Lane != LaneAPIKey {
				return nil, errors.New("llmbudget: OpenAI organization source needs its API lane")
			}
			for _, r := range s.Organization {
				if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
					return nil, errors.New("llmbudget: invalid organization header")
				}
			}
		}
		if strings.HasPrefix(s.CredentialRef, "env:") {
			name := strings.TrimPrefix(s.CredentialRef, "env:")
			if name == "" {
				return nil, errors.New("llmbudget: empty credential environment reference")
			}
			for _, r := range name {
				if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
					return nil, errors.New("llmbudget: invalid credential environment reference")
				}
			}
		}
		if s.ID == "" || seen[s.ID] || s.Provider == "" || s.Account == "" || s.Pool == "" || !validLane(s.Lane) {
			return nil, errors.New("llmbudget: invalid/duplicate source identity")
		}
		seen[s.ID] = true
	}
	return p, nil
}
func validLane(l Lane) bool { return l == LaneAPIKey || l == LaneSubscription || l == LaneLocal }
func bindingFor(p *Policy, model, agent string) (Binding, bool) {
	var fallback Binding
	found := false
	for _, b := range p.Bindings {
		if b.Model == model {
			b.AccountKnown = true
			if b.Agent == agent && agent != "" {
				return b, true
			}
			if b.Agent == "" {
				fallback = b
				found = true
			}
		}
	}
	return fallback, found
}
func poolKey(b Binding) string {
	v, _ := json.Marshal([]string{b.Provider, b.Account, b.Pool, string(b.Lane)})
	return string(v)
}
func requestBinding(r Request) Binding {
	return Binding{Provider: r.Provider, Account: r.Account, Pool: r.Pool, Lane: r.Lane, Model: r.Model, Agent: r.Agent, AccountKnown: r.Account != ""}
}
func matches(c Constraint, b Binding, host string) bool {
	return (c.Provider == "" || c.Provider == b.Provider) && (c.Account == "" || c.Account == b.Account) && (c.Pool == "" || c.Pool == b.Pool) && (c.Lane == "" || c.Lane == b.Lane) && (c.Host == "" || c.Host == host)
}

func hasHardPolicy(p *Policy) bool {
	for _, c := range p.Constraints {
		if !c.Soft {
			return true
		}
	}
	return false
}

func safePolicyStrings(values ...string) bool {
	for _, v := range values {
		if len(v) > 4096 {
			return false
		}
		for _, r := range v {
			if unicode.IsControl(r) {
				return false
			}
		}
	}
	return true
}
