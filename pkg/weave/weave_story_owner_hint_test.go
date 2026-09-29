package weave

import (
	"strings"
	"testing"
)

// An unknown --owner must not be answered with "re-run with --owner <the same
// name>": that sends the caller straight back into the refusal it just got.
func TestSprintOwnerRefusalDoesNotEchoTheRejectedName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	err := validateSprintOwner("nobody-by-this-name")
	if err == nil {
		t.Fatal("an unregistered name was accepted as a sprint owner")
	}
	msg := err.Error()
	if strings.Contains(msg, "--owner nobody-by-this-name") {
		t.Errorf("refusal tells the caller to retry with the name it just refused:\n%s", msg)
	}
	for _, want := range []string{"owns nothing here", "bashy agent list", "--owner NAME"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal missing %q:\n%s", want, msg)
		}
	}
}

// The unclaimed-story hint must never suggest a placeholder owner: claim
// refuses "conductor", so suggesting it is a dead end.
func TestUnclaimedSubmitHintNeverSuggestsAPlaceholder(t *testing.T) {
	for _, who := range []string{"conductor", "steward", "agent", "unknown", ""} {
		hint := unclaimedSubmitHint(328, "537c076cb969", who)
		if strings.Contains(hint, "--owner "+who+"`") || strings.Contains(hint, "--owner conductor") {
			t.Errorf("who=%q: hint suggests a placeholder owner: %s", who, hint)
		}
		if !strings.Contains(hint, "--owner NAME") || !strings.Contains(hint, "bashy agent list") {
			t.Errorf("who=%q: hint does not point at the agent roster: %s", who, hint)
		}
	}
	// A real identity is still named verbatim, so the command stays copy-pasteable.
	if hint := unclaimedSubmitHint(328, "537c076cb969", "claude-opus5"); !strings.Contains(hint, "--owner claude-opus5`") {
		t.Errorf("hint dropped a real identity: %s", hint)
	}
}
