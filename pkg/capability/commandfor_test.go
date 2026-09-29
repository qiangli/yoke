package capability

import (
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

func TestCommandForMatchesCapabilityTagAndAliases(t *testing.T) {
	tool := fleet.Tool{Name: "demo", Commands: []fleet.ToolCommand{
		{Name: "exit", Slash: "/exit", Mode: "tui"},                             // no tag: never matches
		{Name: "review", Slash: "/review", Mode: "print", Capability: "review"}, // alias
		{Name: "plan", Slash: "/plan", Mode: "tui", Capability: "planning"},
		{Name: "odd", Slash: "/odd", Mode: "print", Capability: "no-such-capability"},
	}}
	for _, tc := range []struct {
		c    Capability
		want string
	}{
		{CapCodeReview, "review"},
		{CapPlanning, "plan"},
		{CapDeepResearch, ""},
		{CapCoding, ""},
	} {
		got, ok := CommandFor(tool, tc.c)
		if tc.want == "" {
			if ok {
				t.Errorf("%s: matched %q, want none", tc.c, got.Name)
			}
			continue
		}
		if !ok || got.Name != tc.want {
			t.Errorf("%s: got %q (%v), want %q", tc.c, got.Name, ok, tc.want)
		}
	}
}
