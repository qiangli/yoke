package weave

import (
	"strings"
	"testing"
)

// A manager that launches workers with its own BASHY_AGENT exported must not
// lend that identity to the worker: the worker's commits would be attributed
// to the manager (Sprint 329 scored detected-bypass this way).
func TestWeaveChildEnvReplacesLauncherBashyAgent(t *testing.T) {
	launcher := []string{"PATH=/bin", "BASHY_AGENT=s414-manager"}
	it := &weaveItem{ID: 62, Owner: "codex-gpt6-sol-j", LaunchSpec: &weaveLaunchSpec{Agent: "worker-fixture-agent"}}
	env := weaveChildEnv(launcher, "/workspace", "agent/test", "main", t.TempDir(), it, nil)
	var got []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "BASHY_AGENT=") {
			got = append(got, kv)
		}
	}
	if len(got) != 1 || got[0] != "BASHY_AGENT=worker-fixture-agent" {
		t.Fatalf("worker BASHY_AGENT = %v, want exactly [BASHY_AGENT=worker-fixture-agent]", got)
	}
}

func TestWeaveChildEnvDropsLauncherBashyAgentWithoutWorkerIdentity(t *testing.T) {
	env := weaveChildEnv([]string{"PATH=/bin", "BASHY_AGENT=s414-manager"}, "/workspace", "agent/test", "main", t.TempDir(), &weaveItem{ID: 63}, nil)
	for _, kv := range env {
		if strings.HasPrefix(kv, "BASHY_AGENT=") {
			t.Fatalf("launcher identity leaked into the worker: %s", kv)
		}
	}
}
