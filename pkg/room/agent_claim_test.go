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
		{name: "claude+2", want: "claude+2"},
		{name: "codex-gpt5.6-sol+3", want: "codex-gpt5-6-sol+3"},
		{name: "a+b", want: "a-b"},
		{name: "+2", want: "2"},
	} {
		if got := AgentClaimID(tc.name); got != tc.want {
			t.Errorf("AgentClaimID(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
