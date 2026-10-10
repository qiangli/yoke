package agentlaunch

import (
	"github.com/qiangli/yoke/pkg/fleet"
	"strings"
	"testing"
	"testing/fstest"
)

func TestResolveRetiredBindings(t *testing.T) {
	for _, tc := range []struct{ kind, name string }{{fleet.KindTool, "runner"}, {fleet.KindModel, "seat"}, {fleet.KindAgent, "worker"}} {
		t.Run(tc.kind, func(t *testing.T) {
			cat := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithBaselineFS(fstest.MapFS{}), fleet.WithoutCloudOverlay())
			if err := cat.SaveTool(fleet.Tool{Name: "runner", Aliases: []string{"run-alias"}, Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Launch: fleet.ToolLaunch{Exec: "runner --model {model} {prompt}"}}}); err != nil {
				t.Fatal(err)
			}
			if err := cat.SaveModel(fleet.Model{Name: "seat", Aliases: []string{"model-alias"}}); err != nil {
				t.Fatal(err)
			}
			if err := cat.SaveAgent(fleet.Agent{Name: "worker", Aliases: []string{"worker-alias"}, Tool: "runner", Model: "seat"}); err != nil {
				t.Fatal(err)
			}
			get := func() *fleet.Catalog { return cat }
			if _, err := ResolveWithCatalog("worker", Options{AllowUnsafe: true}, get); err != nil {
				t.Fatal(err)
			}
			if err := cat.Retire(tc.kind, tc.name, "old", "next"); err != nil {
				t.Fatal(err)
			}
			names := []string{"worker", "worker-alias"}
			if tc.kind != fleet.KindAgent {
				names = append(names, "run-alias:model-alias")
			}
			if tc.kind == fleet.KindTool {
				names = append(names, "run-alias")
			}
			for _, name := range names {
				if _, err := ResolveWithCatalog(name, Options{AllowUnsafe: true}, get); err == nil || !strings.Contains(err.Error(), "retired; use next") {
					t.Fatalf("%s error=%v", name, err)
				}
				if _, err := ResolveWithCatalog(name, Options{AllowUnsafe: true, AllowRetired: true}, get); err != nil {
					t.Fatalf("allow %s: %v", name, err)
				}
			}
		})
	}
}
