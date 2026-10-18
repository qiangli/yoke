package weave

import (
	"strings"
	"testing"
)

// TestWeaveChildEnvMintsDelegateEpisode: a spawned worker is a delegate — a
// new episode whose parent is the launcher's.
func TestWeaveChildEnvMintsDelegateEpisode(t *testing.T) {
	env := weaveChildEnv([]string{"PATH=/bin", "BASHY_EPISODE=ep-conductor"}, "/workspace", "agent/test", "main", t.TempDir(), &weaveItem{ID: 9}, nil)
	var episode, parent string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "BASHY_EPISODE="); ok {
			episode = v
		}
		if v, ok := strings.CutPrefix(kv, "BASHY_PARENT_EPISODE="); ok {
			parent = v
		}
	}
	if episode == "" || episode == "ep-conductor" || parent != "ep-conductor" {
		t.Fatalf("worker episode=%q parent=%q, want a new episode under ep-conductor", episode, parent)
	}
}
