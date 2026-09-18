package repomap

import (
	"math"
	"strings"
	"unicode"
)

// pageRank computes PageRank scores for nodes in a directed graph.
// graph maps node -> list of outgoing edges (neighbors).
// personalization provides bias toward certain nodes (nil = uniform).
// Returns a map of node -> score.
func pageRank(graph map[string][]string, personalization map[string]float64, iterations int, damping float64) map[string]float64 {
	if len(graph) == 0 {
		return nil
	}
	if iterations <= 0 {
		iterations = 20
	}
	if damping <= 0 {
		damping = 0.85
	}

	// Collect all nodes.
	nodes := make(map[string]bool)
	for n, edges := range graph {
		nodes[n] = true
		for _, e := range edges {
			nodes[e] = true
		}
	}

	n := float64(len(nodes))
	rank := make(map[string]float64, len(nodes))

	// Initialize with personalization or uniform.
	for node := range nodes {
		if personalization != nil {
			if p, ok := personalization[node]; ok {
				rank[node] = p
			} else {
				rank[node] = 1.0 / n
			}
		} else {
			rank[node] = 1.0 / n
		}
	}

	// Build incoming edges map.
	incoming := make(map[string][]string)
	for src, edges := range graph {
		for _, dst := range edges {
			incoming[dst] = append(incoming[dst], src)
		}
	}

	// Iterate.
	for iter := 0; iter < iterations; iter++ {
		newRank := make(map[string]float64, len(nodes))
		for node := range nodes {
			sum := 0.0
			for _, src := range incoming[node] {
				outDegree := len(graph[src])
				if outDegree > 0 {
					sum += rank[src] / float64(outDegree)
				}
			}

			base := (1.0 - damping) / n
			if personalization != nil {
				if p, ok := personalization[node]; ok {
					base = (1.0 - damping) * p
				}
			}
			newRank[node] = base + damping*sum
		}
		rank = newRank
	}

	return rank
}

// scoreByRelevanceGraph enhances scoring with Aider-inspired heuristics:
// naming conventions, frequency normalization, and optional graph ranking.
func scoreByRelevanceGraph(rm *RepoMap, query string, chatFiles []string) {
	queryLower := strings.ToLower(query)
	queryWords := strings.Fields(queryLower)

	// Build define/reference graph from symbols.
	// For now, we use a simplified approach based on symbol names.
	graph := make(map[string][]string)
	symbolFiles := make(map[string]string) // symbol -> file

	for _, entry := range rm.Entries {
		for _, sym := range entry.Symbols {
			symbolFiles[sym.Name] = entry.Path
			// Files that share symbol references form edges.
			graph[entry.Path] = append(graph[entry.Path], sym.Name)
		}
	}

	// Build personalization vector.
	personalization := make(map[string]float64)
	chatFileSet := make(map[string]bool)
	for _, f := range chatFiles {
		chatFileSet[f] = true
		personalization[f] = 50.0 // 50x boost for chat context files
	}

	// Run PageRank if we have a graph.
	var ranks map[string]float64
	if len(graph) > 3 {
		ranks = pageRank(graph, personalization, 20, 0.85)
	}

	for i := range rm.Entries {
		entry := &rm.Entries[i]
		score := 0.0

		// PageRank contribution.
		if ranks != nil {
			if r, ok := ranks[entry.Path]; ok {
				score += r * 100 // scale up
			}
		}

		// Chat context boost.
		if chatFileSet[entry.Path] {
			score += 50.0
		}

		// Path scoring.
		pathLower := strings.ToLower(entry.Path)
		for _, word := range queryWords {
			if strings.Contains(pathLower, word) {
				score += 5.0
			}
		}

		// Symbol scoring with Aider heuristics.
		for _, sym := range entry.Symbols {
			nameLower := strings.ToLower(sym.Name)
			symScore := 0.0

			for _, word := range queryWords {
				if strings.Contains(nameLower, word) {
					symScore += 3.0
				}
			}

			// Naming heuristics.
			mul := 1.0

			// Meaningful names (camelCase/snake_case, >= 8 chars) get boosted.
			if len(sym.Name) >= 8 && (isCamelCase(sym.Name) || isSnakeCase(sym.Name)) {
				mul *= 10.0
			}

			// Private/unexported symbols get penalized.
			if !sym.Exported {
				mul *= 0.1
			}

			// Frequency normalization (sublinear scaling).
			// Count how many files define this symbol name.
			symScore = math.Sqrt(symScore+1)*mul - 1

			if sym.Exported {
				symScore += 0.5
			}

			score += symScore
		}

		// Exported symbol density bonus.
		exported := 0
		for _, sym := range entry.Symbols {
			if sym.Exported {
				exported++
			}
		}
		score += float64(exported) * 0.2

		entry.Score = score
	}
}

func isCamelCase(s string) bool {
	hasUpper := false
	hasLower := false
	for _, r := range s {
		if unicode.IsUpper(r) {
			hasUpper = true
		}
		if unicode.IsLower(r) {
			hasLower = true
		}
	}
	return hasUpper && hasLower
}

func isSnakeCase(s string) bool {
	return strings.Contains(s, "_") && !strings.ContainsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
}
