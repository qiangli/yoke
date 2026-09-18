package treesitter

import (
	"context"
	"fmt"
	"strings"

	"github.com/odvcencio/gotreesitter"
)

// Search performs a structural search on the given source code using
// a tree-sitter query pattern. The pattern should be a tree-sitter
// S-expression query.
//
// For convenience, simple patterns are also supported:
//   - Node type matching: "(function_declaration)" finds all functions
//   - Named captures: "(function_declaration name: (identifier) @name)"
//   - Predicates: '#eq?' for exact matching
func Search(ctx context.Context, parser *Parser, source []byte, lang, pattern, file string) ([]Match, error) {
	tree, err := parser.Parse(ctx, source, lang)
	if err != nil {
		return nil, err
	}

	language := GetLanguage(lang)
	if language == nil {
		return nil, fmt.Errorf("unsupported language: %s", lang)
	}

	q, err := gotreesitter.NewQuery(pattern, language)
	if err != nil {
		return nil, fmt.Errorf("compile query: %w", err)
	}

	cursor := q.Exec(tree.Root, language, source)

	var matches []Match
	for {
		m, ok := cursor.NextMatch()
		if !ok {
			break
		}

		for _, capture := range m.Captures {
			node := capture.Node
			matchedCode := nodeText(node, source)

			captures := make(map[string]string)
			for _, c := range m.Captures {
				captures[c.Name] = nodeText(c.Node, source)
			}

			matches = append(matches, Match{
				File:        file,
				StartLine:   int(node.StartPoint().Row) + 1,
				EndLine:     int(node.EndPoint().Row) + 1,
				StartCol:    int(node.StartPoint().Column),
				EndCol:      int(node.EndPoint().Column),
				MatchedCode: matchedCode,
				Captures:    captures,
			})
		}
	}

	return matches, nil
}

// SearchText performs a structural search using an ast-grep-style text pattern.
// It translates common patterns to tree-sitter queries where possible.
//
// Pattern syntax (subset of ast-grep):
//   - Literal code: matched structurally against the AST
//   - $NAME: matches any single AST node
//   - $$$: matches zero or more AST nodes
//
// Note: Full ast-grep pattern translation is complex. This function
// provides basic support; for full ast-grep patterns, use the container
// fallback.
func SearchText(ctx context.Context, parser *Parser, source []byte, lang, pattern, file string) ([]Match, error) {
	tree, err := parser.Parse(ctx, source, lang)
	if err != nil {
		return nil, err
	}

	// Simple text-based matching: walk the AST and compare node text.
	var matches []Match
	WalkNodes(tree.Root, tree.language, func(node *gotreesitter.Node) bool {
		text := nodeText(node, source)
		if matchPattern(text, pattern) {
			matches = append(matches, Match{
				File:        file,
				StartLine:   int(node.StartPoint().Row) + 1,
				EndLine:     int(node.EndPoint().Row) + 1,
				StartCol:    int(node.StartPoint().Column),
				EndCol:      int(node.EndPoint().Column),
				MatchedCode: text,
			})
			return false // don't descend into matched nodes
		}
		return true
	})

	return matches, nil
}

// matchPattern checks if text matches an ast-grep-style pattern.
// Handles $VAR (single token wildcard) and $$$VAR (multi-token wildcard).
func matchPattern(text, pattern string) bool {
	// Simple case: no wildcards — check if the node text starts with or
	// contains the pattern (structural match is prefix-based since inner
	// nodes are substrings of outer nodes).
	if !strings.Contains(pattern, "$") {
		trimmed := strings.TrimSpace(text)
		pat := strings.TrimSpace(pattern)
		return trimmed == pat || strings.HasPrefix(trimmed, pat)
	}

	// Convert pattern to a simple regex-like matcher.
	// This is a basic implementation; full ast-grep semantics require
	// AST-level matching.
	parts := strings.Split(pattern, "$$$")
	if len(parts) > 1 {
		// $$$ matches anything.
		return containsAllParts(text, parts)
	}

	// $VAR matches a single token.
	dollarParts := splitOnDollar(pattern)
	return matchPartsAgainstText(text, dollarParts)
}

func containsAllParts(text string, parts []string) bool {
	remaining := text
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.Index(remaining, part)
		if idx < 0 {
			return false
		}
		remaining = remaining[idx+len(part):]
	}
	return true
}

func splitOnDollar(pattern string) []string {
	var parts []string
	current := ""
	inVar := false

	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '$' && !inVar {
			if current != "" {
				parts = append(parts, current)
				current = ""
			}
			inVar = true
			continue
		}
		if inVar {
			if pattern[i] == ' ' || pattern[i] == '(' || pattern[i] == ')' ||
				pattern[i] == ',' || pattern[i] == '{' || pattern[i] == '}' {
				parts = append(parts, "*") // wildcard marker
				inVar = false
				current = string(pattern[i])
				continue
			}
			continue // skip variable name chars
		}
		current += string(pattern[i])
	}
	if inVar {
		parts = append(parts, "*")
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}

func matchPartsAgainstText(text string, parts []string) bool {
	remaining := strings.TrimSpace(text)
	for _, part := range parts {
		if part == "*" {
			// Wildcard: skip ahead to next literal part.
			continue
		}
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.Index(remaining, part)
		if idx < 0 {
			return false
		}
		remaining = remaining[idx+len(part):]
	}
	return true
}
