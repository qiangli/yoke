package llmbudget

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// PolicyChange intentionally includes counts only, never credential references.
type PolicyChange struct {
	Path        string `json:"path"`
	Applied     bool   `json:"applied"`
	Version     int    `json:"version"`
	Bindings    int    `json:"bindings"`
	Constraints int    `json:"constraints"`
	Sources     int    `json:"sources"`
}

func ConfigurePolicy(ctx context.Context, file string, apply bool) (PolicyChange, error) {
	return defaultGate.ConfigurePolicy(ctx, file, apply)
}

// ConfigurePolicy validates first. Only apply=true atomically publishes policy.
// Existing state is untouched; malformed input never replaces current policy.
func (g *Gate) ConfigurePolicy(ctx context.Context, file string, apply bool) (PolicyChange, error) {
	var out PolicyChange
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if file == "" {
		return out, errors.New("llmbudget: policy input file required")
	}
	parser := New(Config{PolicyPath: file})
	p, err := parser.policy()
	if err != nil {
		return out, err
	}
	path := g.cfg.PolicyPath
	if path == "" {
		path = os.Getenv("BASHY_LLM_BUDGET_POLICY")
	}
	if path == "" && g.cfg.StatePath != "" {
		path = filepath.Join(filepath.Dir(g.cfg.StatePath), "llm-budget-policy.json")
	}
	if path == "" || g.cfg.Policy != nil {
		return out, errors.New("llmbudget: no writable file policy authority")
	}
	out = PolicyChange{Path: path, Version: p.Version, Bindings: len(p.Bindings), Constraints: len(p.Constraints), Sources: len(p.Sources)}
	if !apply {
		return out, nil
	}
	lockPath := g.cfg.StatePath + ".lock"
	if g.cfg.StatePath == "" {
		lockPath = path + ".lock"
	}
	l, err := acquireMeter(ctx, lockPath)
	if err != nil {
		return out, err
	}
	defer l.Release()
	if err = ctx.Err(); err != nil {
		return out, err
	}
	if err = atomicJSON(path, p); err != nil {
		return out, err
	}
	out.Applied = true
	return out, nil
}
