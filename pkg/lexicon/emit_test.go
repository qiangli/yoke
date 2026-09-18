package lexicon

import (
	"strings"
	"testing"
)

// emit writes into files that get COMMITTED, so it is a sharing surface. A
// command's resolved path carries the operator's home directory and therefore
// their username; an alias expansion can carry anything. A term set is
// shareable, a filesystem layout is not.
//
// This is a ratchet rather than a comment because emit's renderer is exactly the
// kind of code someone extends with "and also show where it is".
func TestEmit_NeverLeaksLocation(t *testing.T) {
	s := &Store{byTerm: map[string]int{}, byTermAll: map[string][]int{}}
	s.AddSystem(SystemInventory{
		Commands:     []string{"outpost"},
		CommandPaths: map[string]string{"outpost": "/Users/alice/bin/outpost"},
		Aliases:      map[string]string{"codex": "codex --sandbox danger-full-access"},
	}, Overlay{})

	got := s.EmitAgentsMD("demo")
	for _, leak := range []string{"/Users/alice", "danger-full-access", "alice"} {
		if strings.Contains(got, leak) {
			t.Errorf("emit leaked %q into a shareable artifact:\n%s", leak, got)
		}
	}
}
