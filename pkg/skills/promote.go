package skills

// promote renders the human-review bundle for pushing a learned skill
// upstream. It NEVER commits, pushes, or touches any shared catalog —
// the whole point is that a model-authored fix reaches shared space-time
// only through a human-reviewed change.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	dhntskills "github.com/dhnt/dhnt/skills"
)

// promoteBundle writes <out>/: SKILL.md (as authored), skill.dhnt (the
// promoted canonical — the host's latest learned version if one exists,
// else the base), bindings.json (learned executor bindings, if any),
// and PROMOTION.md (lineage + reviewer checklist).
func promoteBundle(cfg *config, sk Skill, src Source, out string) (string, error) {
	if !sk.Dhnt.Valid() {
		return "", fmt.Errorf("skills: %q has no valid canonical face — nothing to promote", sk.Name)
	}
	body, ok := src.Body(sk.Name)
	if !ok {
		return "", fmt.Errorf("skills: %q body unreadable", sk.Name)
	}
	canonBytes, _ := src.File(sk.Name, "skill.dhnt")
	base, err := dhntskills.ParseDhnt(strings.TrimSpace(string(canonBytes)))
	if err != nil {
		return "", err
	}

	promoted := base
	promotedFrom := "base (no learned version on this host)"
	var overlay dhntskills.DerivedVersion
	hasOverlay := false
	if vs := cfg.versions(); vs != nil {
		if v, ok, _ := vs.Latest(sk.Dhnt.Identity); ok {
			if s, err := dhntskills.ParseDhnt(v.Canonical); err == nil {
				promoted, overlay, hasOverlay = s, v, true
				promotedFrom = fmt.Sprintf("host overlay %s (learned at context %s)", shortID(v.ID), shortID(v.ContextKey))
			}
		}
	}
	promotedCanon, err := dhntskills.LineariseDhnt(promoted)
	if err != nil {
		return "", err
	}
	promotedID, err := dhntskills.Identity(promoted)
	if err != nil {
		return "", err
	}

	// Mechanical preservation check: folding adapts the how, never the
	// what — state it verifiably in the review doc.
	preserved := contractsEqual(base, promoted)

	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(out, "SKILL.md"), body, 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(out, "skill.dhnt"), []byte(promotedCanon+"\n"), 0o644); err != nil {
		return "", err
	}
	bindings := loadBindings(cfg.cfgDir, sk.Name)
	if len(bindings) > 0 {
		data, _ := os.ReadFile(bindingsPath(cfg.cfgDir, sk.Name))
		if err := os.WriteFile(filepath.Join(out, "bindings.json"), data, 0o644); err != nil {
			return "", err
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Promotion: %s\n\n", sk.Name)
	fmt.Fprintf(&b, "Rendered %s by `skills promote`. This bundle is a PROPOSAL — review\nand merge it through the catalog's normal change process; nothing has\nbeen committed anywhere.\n\n", time.Now().UTC().Format(time.RFC3339))
	b.WriteString("## Lineage\n\n")
	fmt.Fprintf(&b, "- base identity: `%s`\n", sk.Dhnt.Identity)
	fmt.Fprintf(&b, "- promoted identity: `%s`\n", promotedID)
	fmt.Fprintf(&b, "- promoted from: %s\n", promotedFrom)
	if hasOverlay {
		fmt.Fprintf(&b, "- learned-at coordinate: `%s`\n", overlay.ContextKey)
		fmt.Fprintf(&b, "- verifying attestation: tier `%s`, passed `%s`, effects `%v`, valid `%v`\n",
			overlay.Attest.Tier, strings.Join(overlay.Attest.Passed, " "), overlay.Attest.Effects, overlay.Attest.Valid)
	}
	fmt.Fprintf(&b, "- contract + effect cap preserved from base: **%v** (checked mechanically)\n", preserved)
	if len(bindings) > 0 {
		b.WriteString("\n## Learned executor bindings (bindings.json)\n\n")
		b.WriteString("These commands were introduced by repair on the learning host. They\nare EXECUTOR-side bindings, not part of the canonical skill — review\nthem as you would any proposed script.\n\n")
		for _, k := range sortedKeys(bindings) {
			fmt.Fprintf(&b, "- `%s`: `%s`\n", k, bindings[k])
		}
	}
	b.WriteString("\n## Reviewer checklist\n\n")
	b.WriteString("- [ ] The contract (`enisure` blocks) is unchanged from the base skill\n")
	b.WriteString("- [ ] The effect cap (`efefecato`) is unchanged from the base skill\n")
	b.WriteString("- [ ] The folded steps are safe and sensible OUTSIDE the learning host\n")
	b.WriteString("- [ ] Learned bindings (if any) contain no secrets, no host-specific paths,\n      and nothing destructive\n")
	b.WriteString("- [ ] The environment guard (`conitexuto` arm) correctly scopes where the\n      fix applies — a fix right for one coordinate may be wrong globally\n")
	b.WriteString("- [ ] metadata.requires still describes the applicability region honestly\n")
	if err := os.WriteFile(filepath.Join(out, "PROMOTION.md"), []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return out, nil
}

func contractsEqual(a, b dhntskills.Skill) bool {
	if len(a.Contract) != len(b.Contract) || len(a.EffectCap) != len(b.EffectCap) {
		return false
	}
	for i := range a.Contract {
		if a.Contract[i].Predicate != b.Contract[i].Predicate || len(a.Contract[i].Args) != len(b.Contract[i].Args) {
			return false
		}
	}
	for i := range a.EffectCap {
		if a.EffectCap[i] != b.EffectCap[i] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
