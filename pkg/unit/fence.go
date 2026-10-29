// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package unit

import (
	"slices"
	"strings"
)

// BashSharpTags are every fence info tag that marks a Bash# body, official
// spellings first (bsh / bashsharp; bashpp / bash++ stay as aliases). dag
// registers its Bash# interpreter under exactly these, and the extractor
// below selects exactly these, so a body cannot run under one surface and be
// invisible to the other.
var BashSharpTags = []string{"bsh", "bashsharp", "bashpp", "bash++"}

// IsBashSharpTag reports whether a fence info string selects Bash#. Only the
// first word is the language; the rest of the info string is attributes.
func IsBashSharpTag(info string) bool {
	fields := strings.Fields(info)
	if len(fields) == 0 {
		return false
	}
	return slices.Contains(BashSharpTags, fields[0])
}

// FenceOpen reports a code fence line: its marker ("```" or "~~~") and the
// trimmed info string. It is dag.md's grammar, and the one fence grammar every
// unit reader shares: a fence closes only on a line whose marker equals the
// opening marker and whose info string is empty, so a fence with an info
// string nested inside another fence never closes it.
func FenceOpen(line string) (marker, info string, ok bool) {
	s := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(s, "```"):
		return "```", strings.TrimSpace(strings.TrimPrefix(s, "```")), true
	case strings.HasPrefix(s, "~~~"):
		return "~~~", strings.TrimSpace(strings.TrimPrefix(s, "~~~")), true
	}
	return "", "", false
}

// BashSharpBody is the one fence extractor: the executable Bash# unit of a
// document (SKILL.md, a model's repair reply) — the bytes of every top-level
// Bash# fence, newlines included, concatenated in document order. Fences
// nested inside another fence are examples, not the unit, and are skipped.
// An unterminated Bash# fence is not an executable unit: the result is "".
// A document with no Bash# fence yields "" — a prose-only unit.
func BashSharpBody(document string) string {
	var body strings.Builder
	var marker string
	selected := false
	for _, line := range strings.SplitAfter(document, "\n") {
		m, info, ok := FenceOpen(line)
		if marker != "" {
			if ok && m == marker && info == "" {
				marker, selected = "", false
			} else if selected {
				body.WriteString(line)
			}
			continue
		}
		if ok {
			marker, selected = m, IsBashSharpTag(info)
		}
	}
	if marker != "" && selected {
		return ""
	}
	return body.String()
}
