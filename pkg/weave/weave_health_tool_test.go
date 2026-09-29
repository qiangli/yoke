package weave

import (
	"strings"
	"testing"
)

// A run launched from a managed, versioned binary path is the SAME tool as its
// recorded bare name. Comparing the path against the name as strings made every
// such run report "inconsistent" (Sprint #314, story #1229).
func TestHealthAcceptsManagedBinaryPathForRecordedTool(t *testing.T) {
	for _, tc := range []struct{ recorded, launched string }{
		{"codex", "/Users/u/.bashy/tools/codex/0.157.1/codex"},
		{"claude", "/Users/u/.bashy/tools/claude/2.1.283/claude"},
		{"codex", `C:\Users\u\.bashy\tools\codex\0.157.1\codex.exe`},
		{"codex", "codex"},
	} {
		s := weaveHealthSnapshot{State: "working", Owner: "a", Tool: tc.recorded, WrapperPID: 42, WorkspaceExists: true}
		it := &weaveItem{LaunchSpec: &weaveLaunchSpec{Tool: tc.launched}}
		if msg, _, bad := weaveHealthConsistencyIssue(s, it); bad {
			t.Errorf("recorded %q launched %q: healthy run reported %q", tc.recorded, tc.launched, msg)
		}
	}
}

// A genuinely different tool is still a contradiction.
func TestHealthStillFlagsADifferentTool(t *testing.T) {
	s := weaveHealthSnapshot{State: "working", Owner: "a", Tool: "codex", WrapperPID: 42, WorkspaceExists: true}
	it := &weaveItem{LaunchSpec: &weaveLaunchSpec{Tool: "/Users/u/.bashy/tools/claude/2.1.283/claude"}}
	msg, _, bad := weaveHealthConsistencyIssue(s, it)
	if !bad || !strings.Contains(msg, "contradicts recorded tool") {
		t.Fatalf("a claude launch recorded as codex must be flagged, got bad=%v %q", bad, msg)
	}
}
