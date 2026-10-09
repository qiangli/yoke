package secrets

import (
	"os"
	"strings"
)

// ProjectAgentEnv restores only the resolved binding's credential contract.
// Parent entries win. Missing entries resolve through the same secrets.map
// parser, vault render, and offline cache used by secret env; neither the
// process environment nor the shared cache is modified by a launch.
func ProjectAgentEnv(child, parent, names []string, aliases map[string][]string) []string {
	sources := append([]string(nil), names...)
	for _, candidates := range aliases {
		sources = append(sources, candidates...)
	}
	projected := projectMissingBindings(parent, sources)
	child = PreserveEnvNames(child, projected, names)
	return PreserveEnvAliases(child, projected, aliases)
}

func projectMissingBindings(parent, names []string) []string {
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			wanted[name] = true
		}
	}
	for _, kv := range parent {
		if name, _, ok := strings.Cut(kv, "="); ok {
			delete(wanted, name)
		}
	}
	if len(wanted) == 0 {
		return parent
	}
	bindings, err := readTemplate(defaultTemplatePath())
	if err != nil {
		return parent
	}
	selected := make([]binding, 0, len(wanted))
	needsVault := false
	for _, b := range bindings {
		if wanted[b.local] {
			selected = append(selected, b)
			needsVault = needsVault || b.isRef
		}
	}
	if len(selected) == 0 {
		return parent
	}
	var items []Item
	var cached map[string]string
	if needsVault {
		client, resolveErr := (Config{}).Resolve()
		err = resolveErr
		if err == nil {
			items, err = client.List()
		}
		if err != nil {
			if data, cacheErr := os.ReadFile(cacheFile()); cacheErr == nil {
				cached = ParseEnv(data)
			}
		}
	}
	rendered, missing := renderEnv(selected, items)
	values := ParseEnv(rendered)
	// A reachable vault is authoritative, including deletion. Only an
	// unavailable vault uses cached values, and only for still-declared refs.
	if err != nil {
		for _, b := range missing {
			if value, ok := cached[b.local]; ok {
				values[b.local] = value
			}
		}
	}
	out := append([]string(nil), parent...)
	for _, b := range selected {
		if value, ok := values[b.local]; ok && wanted[b.local] {
			out = append(out, b.local+"="+value)
			delete(wanted, b.local)
		}
	}
	return out
}
