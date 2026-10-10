// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package handoff

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RepoMembers resolves the project members (root + sibling repos + nested modules).
//
// Algorithm:
//   - Start with root.
//   - Walk up from root to find the nearest enclosing go.work (stop at filesystem root).
//   - If found, add every "use" directory (both single-line "use ./x" and block
//     "use ( ... )" forms, stripping comments) resolved relative to the go.work dir,
//     keeping only directories that exist.
//   - Scoping rule: when a go.work is found, include only use-dirs that are top-level
//     siblings of root (same parent dir as root) plus nested modules inside root;
//     do not include nested modules of other siblings (e.g. ./yoke/external/podman/src
//     counts only for the yoke claim). Root itself is always included.
//   - If no go.work is found, fall back to scanning root's go.mod for "../" replace lines.
//   - De-duplicate, clean paths, root first, rest sorted.
func RepoMembers(root string) []string {
	root = filepath.Clean(root)
	absRoot, err := filepath.Abs(root)
	if err != nil {
		absRoot = root
	}

	var candidates []string
	workFile := findEnclosingGoWork(absRoot)
	if workFile != "" {
		candidates = resolveGoWorkMembers(absRoot, workFile)
	} else {
		candidates = scanGoModReplaces(absRoot)
	}

	seen := map[string]bool{absRoot: true, root: true}
	var rest []string
	for _, c := range candidates {
		clean := filepath.Clean(c)
		if !seen[clean] {
			seen[clean] = true
			rest = append(rest, clean)
		}
	}
	sort.Strings(rest)
	return append([]string{root}, rest...)
}

// findEnclosingGoWork walks up from root to find the nearest enclosing go.work.
// Returns empty string if not found before reaching the filesystem root.
func findEnclosingGoWork(root string) string {
	dir := filepath.Clean(root)
	for {
		candidate := filepath.Join(dir, "go.work")
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// resolveGoWorkMembers reads the go.work file, extracts use directories,
// and filters them according to the scoping rule relative to root.
func resolveGoWorkMembers(root, workFile string) []string {
	data, err := os.ReadFile(workFile)
	if err != nil {
		return nil
	}
	workDir := filepath.Dir(workFile)
	parentDir := filepath.Dir(root)
	useDirs := parseGoWork(data)

	var members []string
	for _, raw := range useDirs {
		p := filepath.Clean(filepath.Join(workDir, raw))
		fi, err := os.Stat(p)
		if err != nil || !fi.IsDir() {
			continue
		}
		if p == root {
			continue
		}
		// Scoping rule:
		// include only use-dirs that are TOP-LEVEL siblings of root (same parent dir as root)
		// plus nested modules INSIDE root; do not include nested modules of other siblings.
		if filepath.Dir(p) == parentDir {
			members = append(members, p)
		} else if isSubdir(p, root) {
			members = append(members, p)
		}
	}
	return members
}

// isSubdir reports whether child is a descendant subdirectory of parent.
func isSubdir(child, parent string) bool {
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// parseGoWork parses "use" directives from go.work content, supporting both
// single-line "use ./x" and block "use ( ... )" forms, and strips comments.
func parseGoWork(data []byte) []string {
	var dirs []string
	inBlock := false

	lines := strings.Split(string(data), "\n")
	for _, rawLine := range lines {
		line := stripComments(rawLine)
		if line == "" {
			continue
		}

		if inBlock {
			if line == ")" {
				inBlock = false
				continue
			}
			if strings.HasSuffix(line, ")") {
				line = strings.TrimSpace(strings.TrimSuffix(line, ")"))
				inBlock = false
			}
			if line != "" {
				for _, tok := range extractTokens(line) {
					tok = cleanToken(tok)
					if tok != "" && tok != "(" && tok != ")" {
						dirs = append(dirs, tok)
					}
				}
			}
			continue
		}

		if line == "use" {
			inBlock = true
			continue
		}

		if strings.HasPrefix(line, "use ") || strings.HasPrefix(line, "use\t") {
			rest := strings.TrimSpace(line[3:])
			if rest == "(" {
				inBlock = true
				continue
			}
			if strings.HasPrefix(rest, "(") {
				rest = strings.TrimSpace(rest[1:])
				if strings.HasSuffix(rest, ")") {
					rest = strings.TrimSpace(strings.TrimSuffix(rest, ")"))
					for _, tok := range extractTokens(rest) {
						tok = cleanToken(tok)
						if tok != "" && tok != "(" && tok != ")" {
							dirs = append(dirs, tok)
						}
					}
					continue
				}
				inBlock = true
				for _, tok := range extractTokens(rest) {
					tok = cleanToken(tok)
					if tok != "" && tok != "(" && tok != ")" {
						dirs = append(dirs, tok)
					}
				}
				continue
			}
			for _, tok := range extractTokens(rest) {
				tok = cleanToken(tok)
				if tok != "" && tok != "(" && tok != ")" {
					dirs = append(dirs, tok)
				}
			}
		}
	}
	return dirs
}

// stripComments removes line comments (// ...) and block comments (/* ... */).
func stripComments(line string) string {
	if idx := strings.Index(line, "//"); idx >= 0 {
		line = line[:idx]
	}
	for {
		start := strings.Index(line, "/*")
		if start < 0 {
			break
		}
		end := strings.Index(line[start+2:], "*/")
		if end < 0 {
			line = line[:start]
			break
		}
		line = line[:start] + line[start+2+end+2:]
	}
	return strings.TrimSpace(line)
}

// extractTokens tokenizes words in s, keeping quoted substrings intact.
func extractTokens(s string) []string {
	var tokens []string
	var cur strings.Builder
	inQuote := byte(0)

	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case inQuote != 0:
			if ch == inQuote {
				inQuote = 0
			} else {
				cur.WriteByte(ch)
			}
		case ch == '"' || ch == '\'':
			inQuote = ch
		case ch == ' ' || ch == '\t' || ch == '\r':
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(ch)
		}
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

func cleanToken(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'`)
	return strings.TrimSpace(s)
}

// scanGoModReplaces parses "../" replace targets from root's go.mod file.
func scanGoModReplaces(root string) []string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil
	}
	var members []string
	for _, line := range strings.Split(string(data), "\n") {
		i := strings.Index(line, "=>")
		if i < 0 {
			continue
		}
		rhs := strings.TrimSpace(line[i+2:])
		if sp := strings.IndexAny(rhs, " \t"); sp >= 0 {
			rhs = rhs[:sp]
		}
		if !strings.HasPrefix(rhs, "../") {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rhs), "/")
		if len(parts) < 2 || parts[1] == "" || parts[1] == ".." {
			continue
		}
		abs := filepath.Clean(filepath.Join(root, "..", parts[1]))
		if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
			members = append(members, abs)
		}
	}
	return members
}
