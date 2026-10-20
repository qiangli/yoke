package scaffold

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type lineInfo struct {
	indent int
	text   string
}

// parseYAML parses a YAML document into generic Go data (map[string]any, []any, string, bool, etc.)
// using only standard library. It handles standard YAML mappings, sequences, flow sequences ([a, b]),
// and comments, then serializes to JSON for unmarshaling into strongly-typed structs.
func parseYAML(r io.Reader) (any, error) {
	scanner := bufio.NewScanner(r)
	var lines []lineInfo

	for scanner.Scan() {
		raw := scanner.Text()
		trimmed := strings.TrimRight(raw, " \t\r")
		// Find comment start (not inside quotes)
		cleaned := stripComment(trimmed)
		if strings.TrimSpace(cleaned) == "" {
			continue
		}
		indent := len(cleaned) - len(strings.TrimLeft(cleaned, " "))
		lines = append(lines, lineInfo{
			indent: indent,
			text:   strings.TrimSpace(cleaned),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, nil
	}

	idx := 0
	val, _, err := parseNode(lines, &idx, lines[0].indent)
	return val, err
}

func stripComment(s string) string {
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' && !inDouble {
			inSingle = !inSingle
		} else if c == '"' && !inSingle {
			inDouble = !inDouble
		} else if c == '#' && !inSingle && !inDouble {
			return s[:i]
		}
	}
	return s
}

func parseNode(lines []struct {
	indent int
	text   string
}, idx *int, currentIndent int) (any, bool, error) {
	if *idx >= len(lines) {
		return nil, false, nil
	}

	line := lines[*idx]
	if line.indent < currentIndent {
		return nil, false, nil
	}

	// Check if this line is a list item: starts with "- "
	if strings.HasPrefix(line.text, "- ") || line.text == "-" {
		var list []any
		for *idx < len(lines) {
			cur := lines[*idx]
			if cur.indent < currentIndent {
				break
			}
			if cur.indent > currentIndent {
				return nil, false, fmt.Errorf("unexpected indent at %q", cur.text)
			}
			if !strings.HasPrefix(cur.text, "- ") && cur.text != "-" {
				break
			}
			rest := strings.TrimSpace(strings.TrimPrefix(cur.text, "-"))
			*idx++
			if rest == "" {
				// Nested block under list item
				if *idx < len(lines) && lines[*idx].indent > cur.indent {
					nested, _, err := parseNode(lines, idx, lines[*idx].indent)
					if err != nil {
						return nil, false, err
					}
					list = append(list, nested)
				} else {
					list = append(list, nil)
				}
			} else if strings.HasPrefix(rest, "- ") || rest == "-" {
				// Inline nested item or scalar
				val := parseScalar(rest)
				list = append(list, val)
			} else if colonIdx := findColon(rest); colonIdx > 0 {
				// "- key: value" starts a mapping inside a list item
				key := strings.TrimSpace(rest[:colonIdx])
				valStr := strings.TrimSpace(rest[colonIdx+1:])
				m := make(map[string]any)
				if valStr != "" {
					m[unquote(key)] = parseScalar(valStr)
				} else if *idx < len(lines) && lines[*idx].indent > cur.indent {
					child, _, err := parseNode(lines, idx, lines[*idx].indent)
					if err != nil {
						return nil, false, err
					}
					m[unquote(key)] = child
				} else {
					m[unquote(key)] = nil
				}
				// Parse remaining sibling keys at lines[*idx].indent
				for *idx < len(lines) && lines[*idx].indent > cur.indent {
					subCur := lines[*idx]
					subColon := findColon(subCur.text)
					if subColon <= 0 {
						break
					}
					subK := strings.TrimSpace(subCur.text[:subColon])
					subV := strings.TrimSpace(subCur.text[subColon+1:])
					*idx++
					if subV != "" {
						m[unquote(subK)] = parseScalar(subV)
					} else if *idx < len(lines) && lines[*idx].indent > subCur.indent {
						nested, _, err := parseNode(lines, idx, lines[*idx].indent)
						if err != nil {
							return nil, false, err
						}
						m[unquote(subK)] = nested
					} else {
						m[unquote(subK)] = nil
					}
				}
				list = append(list, m)
			} else {
				list = append(list, parseScalar(rest))
			}
		}
		return list, true, nil
	}

	// Otherwise it's a mapping: "key: value"
	mapping := make(map[string]any)
	for *idx < len(lines) {
		cur := lines[*idx]
		if cur.indent < currentIndent {
			break
		}
		if cur.indent > currentIndent {
			return nil, false, fmt.Errorf("unexpected indent at %q", cur.text)
		}
		colonIdx := findColon(cur.text)
		if colonIdx <= 0 {
			break
		}
		key := strings.TrimSpace(cur.text[:colonIdx])
		valStr := strings.TrimSpace(cur.text[colonIdx+1:])
		*idx++

		if valStr != "" {
			mapping[unquote(key)] = parseScalar(valStr)
		} else {
			// Sub-block
			if *idx < len(lines) && lines[*idx].indent > cur.indent {
				child, _, err := parseNode(lines, idx, lines[*idx].indent)
				if err != nil {
					return nil, false, err
				}
				mapping[unquote(key)] = child
			} else {
				mapping[unquote(key)] = nil
			}
		}
	}
	return mapping, true, nil
}

func findColon(s string) int {
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' && !inDouble {
			inSingle = !inSingle
		} else if c == '"' && !inSingle {
			inDouble = !inDouble
		} else if c == ':' && !inSingle && !inDouble {
			if i+1 == len(s) || s[i+1] == ' ' || s[i+1] == '\t' {
				return i
			}
		}
	}
	return -1
}

func parseScalar(s string) any {
	s = strings.TrimSpace(s)
	if s == "" || s == "~" || s == "null" {
		return nil
	}
	if s == "true" {
		return true
	}
	if s == "false" {
		return false
	}
	// Check for flow array: [a, b, c]
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if inner == "" {
			return []any{}
		}
		var items []any
		for _, part := range splitComma(inner) {
			part = strings.TrimSpace(part)
			if part != "" {
				items = append(items, parseScalar(part))
			}
		}
		return items
	}
	// Check for flow mapping: {a: b, c: d}
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err == nil {
			return m
		}
	}
	// Quoted string
	if (strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`)) ||
		(strings.HasPrefix(s, `'`) && strings.HasSuffix(s, `'`)) {
		return unquote(s)
	}
	// Integer or float
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

func splitComma(s string) []string {
	var parts []string
	start := 0
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' && !inDouble {
			inSingle = !inSingle
		} else if c == '"' && !inSingle {
			inDouble = !inDouble
		} else if c == ',' && !inSingle && !inDouble {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func unquote(s string) string {
	if len(s) >= 2 {
		if s[0] == '"' && s[len(s)-1] == '"' {
			var unq string
			if json.Unmarshal([]byte(s), &unq) == nil {
				return unq
			}
			return s[1 : len(s)-1]
		}
		if s[0] == '\'' && s[len(s)-1] == '\'' {
			return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
		}
	}
	return s
}

// yamlToJSON converts YAML bytes to JSON bytes.
func yamlToJSON(b []byte) ([]byte, error) {
	// If it's already valid JSON, return as-is
	var test any
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&test); err == nil {
		return b, nil
	}
	val, err := parseYAML(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return json.Marshal(val)
}
