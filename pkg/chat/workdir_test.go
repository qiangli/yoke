package chat

import (
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

func TestDefaultWorkdirUsesAgentRecordUnlessCwdWasExplicit(t *testing.T) {
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root), fleet.WithoutCloudOverlay())
	if err := cat.SaveAgent(fleet.Agent{Name: "worker", Tool: "codex", Model: "gpt", Workdir: "/agent/work", Permission: fleet.PermissionAuto}); err != nil {
		t.Fatal(err)
	}
	opt := Options{Catalog: cat}
	defaultWorkdir("worker", &opt)
	if opt.Cwd != "/agent/work" {
		t.Fatalf("default cwd = %q", opt.Cwd)
	}
	opt.Cwd = "/caller/chose"
	defaultWorkdir("worker", &opt)
	if opt.Cwd != "/caller/chose" {
		t.Fatalf("explicit cwd overwritten: %q", opt.Cwd)
	}
}

func TestInvokeUsesAgentWorkdirByDefault(t *testing.T) {
	permitUnsafeLaunch(t)
	pinCatalog(t)
	isolateRoom(t)
	cat := newCatalog()
	if err := cat.SaveTool(fleet.Tool{Name: "runner", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: "runner", Launch: fleet.ToolLaunch{Exec: "runner --model {model} {prompt}"}}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{Name: "model", Kind: fleet.ModelKindLocal, UpstreamID: "model"}); err != nil {
		t.Fatal(err)
	}
	a := fleet.Agent{Name: "worker", Tool: "runner", Model: "model", Workdir: t.TempDir(), Permission: fleet.PermissionAuto}
	if err := cat.SaveAgent(a); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	if _, err := Invoke(t.Context(), Options{Agent: "worker", Instruction: "check workdir"}, runner); err != nil {
		t.Fatal(err)
	}
	if runner.cwd != a.Workdir {
		t.Fatalf("runner cwd = %q, want agent workdir %q", runner.cwd, a.Workdir)
	}
}
