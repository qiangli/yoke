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

func TestShapingBriefRequestsCoverageList(t *testing.T) {
	brief := ShapingBrief("notes", "Add sorting, filtering, and export")
	for _, want := range []string{"End your final response", "every part of the user's request", "COVERED:", "NOT COVERED:", "- (none)"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief missing %q", want)
		}
	}
}

// TestShapingBriefNewAppIntent (S526.B1i): the brief lets Genie say a wish
// is for a separate new app — one NEW-APP line, no change — and the first
// wish on an app made that way is briefed as such instead, so one wish
// makes at most one app. The protected-page brief permits no change at all.
func TestShapingBriefNewAppIntent(t *testing.T) {
	brief := ShapingBrief("notes", "make a sign-up sheet app for Saturday's class")
	for _, want := range []string{"SEPARATE NEW APP", "NEW-APP: <short name> | <label>", "make no change at all", "e.g. signup", "e.g. Sign-up sheet"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief missing %q", want)
		}
	}
	if strings.Contains(brief, "just created, blank") {
		t.Error("an ordinary wish is briefed as the first wish on a new app")
	}
	first := shapingTaskFor("signup", wish{Request: "make a sign-up sheet app", first: true})
	if !strings.Contains(first, "just created, blank, for this very request") || !strings.Contains(first, "never answer NEW-APP") {
		t.Errorf("first wish brief does not say the app was just made for it:\n%s", first)
	}
	if strings.Contains(first, "SEPARATE NEW APP") {
		t.Error("the first wish on a new app is still offered NEW-APP")
	}
	intent := intentTaskFor("console", wish{Request: "make a sign-up sheet app", Page: "/pair"})
	for _, want := range []string{"Make NO change", "NEW-APP: <short name> | <label>", "FILED", "page: /pair", "<<<REQUEST\nmake a sign-up sheet app\nREQUEST>>>"} {
		if !strings.Contains(intent, want) {
			t.Errorf("intent brief missing %q", want)
		}
	}
	if strings.Contains(intent, "Make the smallest change") {
		t.Error("the intent brief asks for a change")
	}
}

// G1 (operator live, 2026-10-31): "make a shooting game" on the launcher was
// built as a page + link inside the launcher. The brief must say that a
// self-contained thing such as a game is a separate new app, and that a
// separate thing is never built inside this app as a page or a link.
func TestShapingBriefSelfContainedThingIsNewApp(t *testing.T) {
	brief := shapingTask("console", "make a shooting game")
	for _, want := range []string{"a game", "IS a separate new app", "Never build a separate thing inside this app as a new page or a link"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q", want)
		}
	}
}
