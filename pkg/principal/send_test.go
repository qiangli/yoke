package principal

import (
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

// lookupSend is the cheap half of resolution the send path rides on. It must
// answer from the same sources, with the same precedence, as Resolve — one
// resolver — while touching no network and running no probes.

func TestLookupSendResolvesRolesAgentsAndPersons(t *testing.T) {
	withRoles(t, HostRole{Label: "steward", Topic: "steward.dragon-u501"})
	env := obsEnv(t)
	snap := &snapshot{
		agents: []fleet.Agent{{Name: "codex-gpt5.6-sol", Aliases: []string{"Omar"}}},
		people: []fleet.Person{{Handle: "alice", Aliases: []string{"al"}}},
	}

	for _, tt := range []struct {
		query string
		kind  Kind
		name  string
	}{
		// A role resolves to its seat TOPIC — the address that survives a
		// handover — by label or by topic, case-insensitively.
		{"steward", KindRole, "steward.dragon-u501"},
		{"Steward", KindRole, "steward.dragon-u501"},
		// An agent resolves to its canonical roster name, from any alias.
		{"codex-gpt5.6-sol", KindAgent, "codex-gpt5.6-sol"},
		{"Omar", KindAgent, "codex-gpt5.6-sol"},
		{"omar", KindAgent, "codex-gpt5.6-sol"},
		// A person resolves to their handle; the OS login resolves bare.
		{"al", KindPerson, "alice"},
		{"localguy", KindPerson, "localguy"},
	} {
		got := lookupSend(snap, env, tt.query)
		if len(got) != 1 || got[0].Kind != tt.kind || got[0].Name != tt.name {
			t.Errorf("lookupSend(%q) = %+v, want one %s %q", tt.query, got, tt.kind, tt.name)
		}
	}

	if got := lookupSend(snap, env, "nobody-anywhere"); len(got) != 0 {
		t.Errorf("lookupSend(nobody-anywhere) = %+v, want nothing", got)
	}
}

// Observation is subordinate here exactly as it is in Resolve: a trace
// answers only when the declared sources name nothing, so the two resolution
// paths cannot disagree about a declared name.
func TestLookupSendObservationIsSubordinate(t *testing.T) {
	env := obsEnv(t)
	// A board cursor is the trace: this seat polls, though no catalog has it.
	writeFile(t, filepath.Join(env.BoardDir, "seen", "weave-w7"), "12\n")

	got := lookupSend(&snapshot{}, env, "weave-w7")
	if len(got) != 1 || got[0].Kind != KindAgent || got[0].Name != "weave-w7" {
		t.Fatalf("an observed-only seat must be sendable, got %+v", got)
	}

	// The same name declared in the catalog answers from the catalog alone.
	snap := &snapshot{agents: []fleet.Agent{{Name: "weave-w7"}}}
	got = lookupSend(snap, env, "weave-w7")
	if len(got) != 1 || got[0].Kind != KindAgent {
		t.Fatalf("declared name = %+v, want the single catalog answer", got)
	}
}
