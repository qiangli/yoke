package fleet

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

func TestArchiveAgentPreservesEvidenceAndRequiresGuard(t *testing.T) {
	root := fleettest.Ring(t)
	t.Setenv("BASHY_HOME", root)
	cat := New()
	a := Agent{Name: "disposable", Tool: "codex", Model: "gpt5.6-sol", Ephemeral: true, Lifecycle: &AgentLifecycle{SprintID: "sprint-uuid"}}
	if err := cat.SaveAgent(a); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(root, "ledger.jsonl")
	evidence := []byte("historical evidence for disposable\n")
	if err := os.WriteFile(ledger, evidence, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ArchiveAgent(a.Name, nil); err == nil {
		t.Fatal("unguarded removal allowed")
	}
	busy := errors.New("live or leased")
	if _, err := cat.ArchiveAgent(a.Name, func(Agent) error { return busy }); !errors.Is(err, busy) {
		t.Fatalf("busy guard: %v", err)
	}
	if _, ok := cat.Agent(a.Name); !ok {
		t.Fatal("guarded agent lost")
	}
	archive, err := cat.ArchiveAgent(a.Name, func(Agent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	record, err := ParseAgentFile("disposable", body, nil)
	if err != nil || len(record.Agents) != 1 || record.Agents[0].Lifecycle.SprintID != "sprint-uuid" {
		t.Fatalf("unrestorable archive: %s %v", body, err)
	}
	got, err := os.ReadFile(ledger)
	if err != nil || string(got) != string(evidence) {
		t.Fatalf("evidence changed: %s %v", got, err)
	}
	if _, ok := cat.Agent(a.Name); ok {
		t.Fatal("archived definition remains")
	}
}

func TestArchiveAgentRetainsPermanentAndLowerRingIdentities(t *testing.T) {
	cat := cloneCatalog(t)
	if err := cat.SaveAgent(Agent{Name: "permanent", Tool: "codex", Model: "gpt5.6-sol"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ArchiveAgent("permanent", func(Agent) error { return nil }); err == nil {
		t.Fatal("permanent removal allowed")
	}
	agents, _ := cat.Agents()
	for _, a := range agents {
		if a.Ring == ringLocal() {
			continue
		}
		a.Ephemeral = true
		a.Lifecycle = &AgentLifecycle{SprintID: "sprint-uuid"}
		if err := cat.SaveAgent(a); err != nil {
			t.Fatal(err)
		}
		if _, err := cat.ArchiveAgent(a.Name, func(Agent) error { return nil }); err == nil {
			t.Fatal("lower-ring shadow removed")
		}
		return
	}
	t.Fatal("test requires a lower-ring agent")
}

func TestExplicitSprintMint(t *testing.T) {
	fleettest.Ring(t)
	t.Setenv("BASHY_MINT_SPRINT", "ambient-sprint")
	_, err := runCmd(t, NewAgentsCmd(), "add", "explicit-seat", "--tool", "codex", "--model", "gpt5.6-sol", "--sprint", "explicit-sprint")
	if err != nil {
		t.Fatal(err)
	}
	a, ok := New().Agent("explicit-seat")
	if !ok || !a.Ephemeral || a.Lifecycle == nil || a.Lifecycle.SprintID != "explicit-sprint" {
		t.Fatalf("mint: %+v", a)
	}
}
