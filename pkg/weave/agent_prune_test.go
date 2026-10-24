package weave

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
	"github.com/qiangli/yoke/pkg/prune"
)

func TestAgentPruneArchivesOnlyOldIdleClone(t *testing.T) {
	root := fleettest.Ring(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(home, "room"))
	cat := fleet.New(fleet.WithRoot(root), fleet.WithoutCloudOverlay())
	now := time.Now().UTC()
	for _, a := range []fleet.Agent{
		{Name: "base", Tool: "codex", Model: "gpt5.6-sol"},
		{Name: "old-clone", Tool: "codex", Model: "gpt5.6-sol", ClonedFrom: "base", ClonedAt: now.Add(-40 * 24 * time.Hour).Format(time.RFC3339)},
		{Name: "busy-clone", Tool: "codex", Model: "gpt5.6-sol", ClonedFrom: "base", ClonedAt: now.Add(-40 * 24 * time.Hour).Format(time.RFC3339)},
		{Name: "young-clone", Tool: "codex", Model: "gpt5.6-sol", ClonedFrom: "base", ClonedAt: now.Add(-2 * 24 * time.Hour).Format(time.RFC3339)},
	} {
		if err := cat.SaveAgent(a); err != nil {
			t.Fatal(err)
		}
	}
	family, ok, err := cat.FamilyOf("busy-clone")
	if err != nil || !ok {
		t.Fatalf("busy family: %v", err)
	}
	if _, err := fleet.NewInstanceStore(fleet.InstanceDir()).Open(family, fleet.OpenOptions{}); err != nil {
		t.Fatalf("busy instance: %v", err)
	}
	path := filepath.Join(root, "agents", "old-clone.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p := AgentPruner{Catalog: cat, Now: func() time.Time { return now }}
	items, err := p.DryRun(context.Background(), prune.Options{})
	if err != nil || len(items) != 1 || items[0].ID != "old-clone" {
		t.Fatalf("dry-run = %+v, %v", items, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("dry-run changed the agent definition")
	}
	if _, err := os.Stat(filepath.Join(home, "room")); !os.IsNotExist(err) {
		t.Fatal("dry-run created room state")
	}
	counts, err := p.Apply(context.Background(), items)
	if err != nil || counts.Removed != 1 {
		t.Fatalf("apply = %+v, %v", counts, err)
	}
	if _, ok := cat.Agent("old-clone"); ok {
		t.Fatal("old clone remains")
	}
	if archives, _ := filepath.Glob(filepath.Join(root, "agents", "archive", "old-clone-*.yaml")); len(archives) != 1 {
		t.Fatalf("old clone archive = %v", archives)
	}
	for _, name := range []string{"base", "busy-clone", "young-clone"} {
		if _, ok := cat.Agent(name); !ok {
			t.Fatalf("%s was removed", name)
		}
	}
}
