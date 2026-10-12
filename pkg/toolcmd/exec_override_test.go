package toolcmd

import (
	"context"
	"reflect"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

// Sprint 410, story 1919, end to end: a print command with its own exec: on a
// tool that declares cli.launch.setup and cli.launch.env runs the tool's
// resolved binary directly. The real chat.Invoke renders the dry-run argv (no
// stubs), so a setup wrapper swallowing the binary fails here exactly as it
// did for `tool cmd run openclaw:send`.
func TestPrintExecWithSetupRunsToolBinary(t *testing.T) {
	fleettest.Ring(t)
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("HOME", home)

	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	tool := fleet.Tool{Name: "msgtool", Kind: fleet.ToolKindCLI}
	tool.CLI.Binary = "msgtool"
	tool.CLI.Launch.Exec = "msgtool -p {prompt}"
	tool.CLI.Launch.Env = []string{"MSGTOOL_HOME={state_dir}"}
	tool.CLI.Launch.Setup = "mkdir -p {state_dir}"
	if err := cat.SaveTool(tool); err != nil {
		t.Fatal(err)
	}
	cmd := fleet.ToolCommand{Name: "send", Slash: "/send {args}", Mode: "print", Exec: "msgtool message send --json {args}"}
	res, err := Run(context.Background(), tool, cmd, "--dry-run", Options{Catalog: cat, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeDryRun {
		t.Fatalf("outcome = %q, want %q (%+v)", res.Outcome, OutcomeDryRun, res)
	}
	want := []string{"msgtool", "message", "send", "--json", "--dry-run"}
	if !reflect.DeepEqual(res.Argv, want) {
		t.Fatalf("dry-run argv = %q, want %q (the setup wrapper swallowed the tool binary)", res.Argv, want)
	}
}
