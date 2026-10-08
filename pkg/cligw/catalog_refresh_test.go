package cligw

import (
	"context"
	"github.com/qiangli/yoke/pkg/fleet"
	"testing"
)

func TestCatalogRefreshesMissingAgentWithoutRestart(t *testing.T) {
	cat := testFleet(t)
	cat.inventory()
	if err := cat.Registry().SaveAgent(fleet.Agent{Name: "new-agent", Tool: "alpha-tool", Model: "small"}); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(ServerOptions{Catalog: cat, Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok := s.Agent("new-agent"); !ok {
		t.Fatal("server retained stale agent inventory")
	}
	sel, err := cat.ParseModelSelector("new-agent")
	if err != nil {
		t.Fatal(err)
	}
	if got := cat.Candidates(context.Background(), sel, Filter{}); len(got) != 1 {
		t.Fatalf("new agent not routable: %+v", got)
	}
}

func TestCatalogRefreshesWhenToolBecomesLaunchable(t *testing.T) {
	cat := testFleet(t)
	cat.inventory() // unlaunchable references a tool with no launch contract
	tool, ok := cat.Registry().Tool("alpha-tool")
	if !ok {
		t.Fatal("missing fixture tool")
	}
	tool.Name = "dead-tool"
	if err := cat.Registry().SaveTool(tool); err != nil {
		t.Fatal(err)
	}
	sel, err := cat.ParseModelSelector("unlaunchable")
	if err != nil {
		t.Fatal(err)
	}
	got := cat.Candidates(context.Background(), sel, Filter{})
	if len(got) != 1 || got[0].Name != "unlaunchable" {
		t.Fatalf("new tool still missing: %+v", got)
	}
}
