// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"strings"
	"testing"
)

func TestShapingBriefExpandThenContractRuleAndExample(t *testing.T) {
	app := "notes"
	req := "Add color tags to notes"
	brief := ShapingBrief(app, req)

	// Must contain the app name and user request
	if !strings.Contains(brief, `"`+app+`"`) {
		t.Errorf("brief does not contain app name %q", app)
	}
	if !strings.Contains(brief, req) {
		t.Errorf("brief does not contain user request %q", req)
	}

	// Must state the expand-then-contract rule for data evolution
	rulePhrases := []string{
		"expand-then-contract",
		"ADD properties/elements with a fallback for old data",
		"removing or renaming stored data or interface elements",
		"unit names, flags, arguments, routes, declared stored properties",
		"removal only via a separate explicit user-approved contract step",
	}
	for _, phrase := range rulePhrases {
		if !strings.Contains(brief, phrase) {
			t.Errorf("brief missing required expand-then-contract rule phrase: %q", phrase)
		}
	}

	// Must include an add-with-fallback example
	examplePhrases := []string{
		"Add-with-fallback example:",
		"display_name",
		"??",
		":-",
	}
	for _, phrase := range examplePhrases {
		if !strings.Contains(brief, phrase) {
			t.Errorf("brief missing required example phrase: %q", phrase)
		}
	}

	// shapingTask should produce the identical brief
	if task := shapingTask(app, req); task != brief {
		t.Errorf("shapingTask and ShapingBrief differ:\ntask:\n%s\nbrief:\n%s", task, brief)
	}
}
