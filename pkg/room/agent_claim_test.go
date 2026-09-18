package room

import "testing"

func TestAgentClaimIDMatchesEstablishedChatIdentitySpelling(t *testing.T) {
	for _, tc := range []struct {
		name, want string
	}{
		{name: "codex-gpt5.6-sol", want: "codex-gpt5-6-sol"},
		{name: "  agent / topic  ", want: "agent-topic"},
		{name: "...", want: "agent"},
		{name: "007", want: "007"},
	} {
		if got := AgentClaimID(tc.name); got != tc.want {
			t.Errorf("AgentClaimID(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
