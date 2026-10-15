package llmbudget

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func EnableProbe(id string) error { return defaultGate.setProbeConsent(context.Background(), id, true) }
func DisableProbe(id string) error {
	return defaultGate.setProbeConsent(context.Background(), id, false)
}
func (g *Gate) EnableProbe(id string) error { return g.setProbeConsent(context.Background(), id, true) }
func (g *Gate) DisableProbe(id string) error {
	return g.setProbeConsent(context.Background(), id, false)
}

func (g *Gate) setProbeConsent(ctx context.Context, id string, enable bool) error {
	if !strings.HasPrefix(id, "probe:") || !safePolicyStrings(id) {
		return errors.New("llmbudget: invalid probe id")
	}
	path := g.cfg.PolicyPath
	if path == "" {
		path = os.Getenv("BASHY_LLM_BUDGET_POLICY")
	}
	if path == "" && g.cfg.StatePath != "" {
		path = filepath.Join(filepath.Dir(g.cfg.StatePath), "llm-budget-policy.json")
	}
	if path == "" || g.cfg.Policy != nil {
		return errors.New("llmbudget: no writable file policy authority")
	}
	lockPath := g.cfg.StatePath + ".lock"
	if g.cfg.StatePath == "" {
		lockPath = path + ".lock"
	}
	lock, err := acquireMeter(ctx, lockPath)
	if err != nil {
		return err
	}
	defer lock.Release()
	p, err := g.policy()
	if err != nil {
		return err
	}
	p.missing = false
	next := make([]string, 0, len(p.ProbeConsent)+1)
	for _, v := range p.ProbeConsent {
		if v != id {
			next = append(next, v)
		}
	}
	if enable {
		next = append(next, id)
	}
	p.ProbeConsent = next
	return atomicJSON(path, p)
}
