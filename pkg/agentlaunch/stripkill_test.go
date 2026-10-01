package agentlaunch

import (
	"reflect"
	"testing"
)

// TestStripKillSwitches — the attended transform removes the auto-approve
// kill-switches (so the tool's own approval gate stays on and the uncontained-
// host guard has nothing to refuse) while leaving write flags and model args
// intact, and downgrades --sandbox danger-full-access to the tool default.
func TestStripKillSwitches(t *testing.T) {
	in := []string{
		"--danger-skip-permissions", "--model", "deepseek-v4-pro",
		"--dangerously-skip-permissions", "--sandbox", "danger-full-access", "prompt",
	}
	got := StripKillSwitches("ycode", in)
	want := []string{"--model", "deepseek-v4-pro", "prompt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StripKillSwitches = %v, want %v", got, want)
	}

	// And its whole point: what it produces passes the guard on an uncontained host.
	if err := GuardUnsafeArgs("ycode", got); err != nil {
		t.Fatalf("stripped argv should pass the guard, got: %v", err)
	}
	// A sandbox mode that is not danger-full-access is preserved.
	if got := StripKillSwitches("codex", []string{"--sandbox", "workspace-write", "--yolo"}); !reflect.DeepEqual(got, []string{"--sandbox", "workspace-write"}) {
		t.Fatalf("workspace-write sandbox should survive, got %v", got)
	}
}

// Codex uses its bypass by default, across headless and interactive launches.
func TestCodexYoloDisablesApproval(t *testing.T) {
	base := []string{"exec", "--sandbox", "workspace-write", "--model", "gpt-5.6-sol"}
	has := func(a []string, f string) bool { return containsArg(a, f) }

	// Default: bypasses approval and sandbox even when the template is scoped.
	def := ApplySandbox("codex", append([]string{}, base...), Options{})
	if !has(def, "--dangerously-bypass-approvals-and-sandbox") || has(def, "--sandbox") {
		t.Fatalf("default should bypass approvals and sandbox, got %v", def)
	}
	// --yolo: bypass, no --sandbox.
	yolo := ApplySandbox("codex", append([]string{}, base...), Options{AllowUnsafe: true})
	if !has(yolo, "--dangerously-bypass-approvals-and-sandbox") || has(yolo, "--sandbox") {
		t.Fatalf("--yolo should bypass approvals + drop --sandbox, got %v", yolo)
	}
	// ReadOnly wins even with AllowUnsafe.
	ro := ApplySandbox("codex", []string{"exec", "--sandbox", "read-only", "--model", "m"}, Options{ReadOnly: true, AllowUnsafe: true})
	if has(ro, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("read-only must not be bypassed, got %v", ro)
	}
	ro = ApplySandbox("codex", base, Options{ReadOnly: true, Sandbox: "danger-full-access"})
	if has(ro, "--dangerously-bypass-approvals-and-sandbox") || !has(ro, "read-only") {
		t.Fatalf("read-only must win over explicit full access, got %v", ro)
	}
}
