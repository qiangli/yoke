// Package recommend answers "the agent guessed the wrong name" from the shell's
// ground truth. The LLM cannot see the local filesystem, so it probes with a
// name it *expects* (CLAUDE.md, confg.yaml, main_test.go) — bashy KNOWS what is
// actually there. When a read-only command fails on a not-found target, or a
// search comes back empty, this package ranks the real, existing candidates the
// agent most likely meant, so the shell can hand back "no X; did you mean Y?"
// instead of an error the agent must round-trip to resolve.
//
// P2 P0 covers the not-found-target case with two no-LLM signals: lexical
// similarity (typos, close variants, same extension) and a small curated family
// of known-equivalent agent files (CLAUDE.md ⇄ AGENTS.md ⇄ …). Semantic
// (embedding), graph-adjacency, and co-occurrence signals are later slices.
// Invariant: recommend, never substitute — surface it, the agent decides.
package recommend

import (
	"path"
	"sort"
	"strings"
)

// Scored is a candidate and its relevance (higher = better).
type Scored struct {
	Name  string
	Score float64
	Why   string // "similar-name" | "known-equivalent"
}

// knownEquivalents groups files that mean the same thing to an agent; a lookup
// for one recommends the others when they exist. Bidirectional within a group.
var knownEquivalents = [][]string{
	{"CLAUDE.md", "AGENTS.md", "GEMINI.md", ".cursorrules", ".github/copilot-instructions.md", ".windsurfrules"},
	{"README.md", "README", "README.txt", "readme.md"},
	{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"},
	{".env", ".env.local", ".env.example"},
}

// Recommend ranks existing candidates the agent likely meant by `missing`.
// candidates is the list of names that actually exist (a directory listing, a
// repo file list). Returns up to limit results, best first; empty if nothing is
// close enough.
func Recommend(missing string, candidates []string, limit int) []Scored {
	if missing == "" || len(candidates) == 0 {
		return nil
	}
	exists := make(map[string]bool, len(candidates))
	base := make(map[string]string) // basename -> full candidate (first wins)
	for _, c := range candidates {
		exists[c] = true
		b := path.Base(c)
		if _, ok := base[b]; !ok {
			base[b] = c
		}
	}

	seen := map[string]bool{}
	var out []Scored
	add := func(name string, score float64, why string) {
		if name == missing || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, Scored{Name: name, Score: score, Why: why})
	}

	// (1) Known-equivalent family: strong signal, matched on basename.
	mbase := path.Base(missing)
	for _, group := range knownEquivalents {
		if !groupContains(group, mbase) {
			continue
		}
		for _, member := range group {
			if path.Base(member) == mbase {
				continue
			}
			if full, ok := base[path.Base(member)]; ok {
				add(full, 1.0, "known-equivalent")
			} else if exists[member] {
				add(member, 1.0, "known-equivalent")
			}
		}
	}

	// (2) Lexical similarity on the basename.
	for _, c := range candidates {
		s := similar(mbase, path.Base(c))
		if s >= 0.5 {
			add(c, s, "similar-name")
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func groupContains(group []string, base string) bool {
	for _, m := range group {
		if path.Base(m) == base {
			return true
		}
	}
	return false
}

// similar scores two basenames in [0,1]: the max of token-set Jaccard and a
// normalized edit-distance similarity, with a bonus for a shared extension.
func similar(a, b string) float64 {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	if la == lb {
		return 1
	}
	j := jaccard(tokens(la), tokens(lb))
	e := 1 - float64(levenshtein(la, lb))/float64(max(len(la), len(lb)))
	s := max(j, e)
	if ext(la) != "" && ext(la) == ext(lb) {
		s = max(s, 0.5) // same extension: at least a weak match
	}
	return s
}

func ext(name string) string { return strings.ToLower(path.Ext(name)) }

func tokens(s string) map[string]bool {
	out := map[string]bool{}
	cur := strings.Builder{}
	flush := func() {
		if cur.Len() > 0 {
			out[cur.String()] = true
			cur.Reset()
		}
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(min(prev[j]+1, cur[j-1]+1), prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}
