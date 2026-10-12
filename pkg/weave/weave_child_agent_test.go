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

// The manager-side fix (chat restores BASHY_SPRINT_LEASE_TOKEN for a managed
// owner session's binding child) must not bleed here: a worker is NOT the
// manager, and a worker holding the manager's lease could drive every gated
// sprint verb the admission gate exists to refuse. The credential firewall in
// the worker's own env build drops the token by shape; booths block it by
// name as well (TestBoothScrubEnv).
func TestWeaveChildEnvDropsSprintLeaseToken(t *testing.T) {
	launcher := []string{"PATH=/bin", "BASHY_SPRINT_LEASE_TOKEN=lease-secret", "BASHY_MINT_SPRINT=story-uuid"}
	it := &weaveItem{ID: 64, Owner: "codex-gpt6-sol-j", LaunchSpec: &weaveLaunchSpec{Agent: "worker-fixture-agent"}}
	env := weaveChildEnv(launcher, "/workspace", "agent/test", "main", t.TempDir(), it, nil)
	for _, kv := range env {
		if strings.HasPrefix(kv, "BASHY_SPRINT_LEASE_TOKEN=") {
			t.Fatalf("manager lease leaked into the worker: %s", kv)
		}
	}
}

// A named launch is the actual agent. When the run record carries no launch
// spec agent, the queue owner is the per-issue seat (`007-a`), which resolves
// to no principal and must never be exported as the worker's identity.
func TestWeaveChildEnvUsesNamedLaunchNotSeat(t *testing.T) {
	launcher := []string{"PATH=/bin", "BASHY_AGENT=s414-manager"}
	it := &weaveItem{ID: 65, Owner: "fixture-seat-a"}
	l := &weaveAgentLaunch{ToolName: "codex", Nick: "fixture-worker"}
	env := weaveChildEnv(launcher, "/workspace", "agent/test", "main", t.TempDir(), it, l)
	var got []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "BASHY_AGENT=") {
			got = append(got, kv)
		}
	}
	if len(got) != 1 || got[0] != "BASHY_AGENT=fixture-worker" {
		t.Fatalf("worker BASHY_AGENT = %v, want exactly [BASHY_AGENT=fixture-worker]", got)
	}
}
