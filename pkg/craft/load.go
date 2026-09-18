package craft

// Loading the catalog into a queryable index.
//
// The bridge between pkg/skills (which knows what skills exist and whether they
// apply here) and Resolve/Compose (which need typed contracts and their
// bindings). It reads; it writes nothing.

import (
	"strings"

	dhntskills "github.com/dhnt/dhnt/skills"

	"github.com/qiangli/yoke/pkg/skills"
)

// binding key prefixes, matching the L3 convention pkg/skills executes with:
// a contract predicate X resolves `check-X`, a step primitive P resolves
// `step-P`. Reusing the same prefixes is what makes the determinism ratio mean
// the same thing here as it does at run time.
const (
	checkPrefix = "check-"
	stepPrefix  = "step-"
)

// LoadImplementations reads every APPLICABLE skill that carries a canonical
// face, and returns them as indexable implementations.
//
// Applicability is respected rather than ignored: a skill gated to another
// platform is not an answer here, and offering it would be a confidently wrong
// one. A skill with no canonical face carries no contract, so there is nothing
// to key it by — it is skipped, not guessed at.
//
// A malformed entry is SKIPPED, never fatal. Loading happens on a lookup path,
// and one bad skill in a store must not make every other skill unreachable.
func LoadImplementations(cat *skills.Catalog, ps *skills.ProbeSet) []Implementation {
	if cat == nil {
		return nil
	}
	rows, err := cat.List(ps)
	if err != nil {
		return nil
	}
	var out []Implementation
	for _, r := range rows {
		if !r.Verdict.Applicable {
			continue
		}
		sk, src, ok := cat.Get(r.Name)
		if !ok || !sk.HasDhnt {
			continue
		}
		// Read through the SOURCE, not the filesystem. An embedded skill has no
		// Dir by design ("" for embedded), so a filesystem read silently skips
		// exactly the skills that ship with the binary — which is most of them.
		canon, ok := src.File(sk.Name, "skill.dhnt")
		if !ok {
			continue
		}
		ast, err := dhntskills.ParseDhnt(strings.TrimSpace(string(canon)))
		if err != nil {
			continue
		}
		out = append(out, Implementation{
			Name:        sk.Name,
			Skill:       ast,
			Description: sk.Description,
			Bindings:    bindingsOf(sk.Meta),
		})
	}
	return out
}

// bindingsOf extracts the check-*/step-* keys from a skill's frontmatter.
//
// Only those two prefixes: the rest of the metadata is classification
// (`requires`, `tier`, `tasks`), and treating an arbitrary key as a command is
// how a manifest field becomes an accidental execution.
func bindingsOf(meta map[string]string) map[string]string {
	if len(meta) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range meta {
		if strings.HasPrefix(k, checkPrefix) || strings.HasPrefix(k, stepPrefix) {
			if strings.TrimSpace(v) != "" {
				out[k] = v
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
