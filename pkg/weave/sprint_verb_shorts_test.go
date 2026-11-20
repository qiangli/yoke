package weave

import (
	"strings"
	"testing"
)

// Regression for story 900e6757: sprint review shared assign's Short
// verbatim and grade/merge shared one Short, so `sprint --help`
// described none of them. Each verb's one-liner must be its own and
// name what that verb does, so a shared constructor cannot silently
// re-copy a sibling's text.
func TestSprintVerbShortsAreDistinctAndSpecific(t *testing.T) {
	shorts := map[string]string{
		"assign": newSprintAssignCmd().Short,
		"review": newSprintReviewCmd().Short,
	}
	for _, cmd := range newSprintGradeCommands() {
		shorts[cmd.Name()] = cmd.Short
	}
	wantAnchor := map[string]string{
		"assign": "worker",
		"review": "reviewer",
		"grade":  "Grade",
		"merge":  "Merge",
	}
	seen := map[string]string{}
	for verb, anchor := range wantAnchor {
		short, ok := shorts[verb]
		if !ok || strings.TrimSpace(short) == "" {
			t.Errorf("sprint %s: missing Short", verb)
			continue
		}
		if !strings.Contains(short, anchor) {
			t.Errorf("sprint %s: Short %q does not name its action (want %q)", verb, short, anchor)
		}
		if other, dup := seen[short]; dup {
			t.Errorf("sprint %s and %s share Short %q", verb, other, short)
		}
		seen[short] = verb
	}
}
