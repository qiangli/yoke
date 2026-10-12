package chat

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

// A print command with its own exec: is the tool's own subcommand, not an
// agent turn (sprint 410, story 1919): the launch setup wrapper must not
// swallow the tool binary, and the launch-declared env/state_dir redirect
// must not apply, so the subcommand sees the user's own tool config.
func TestExecOverrideRunsToolBinaryDirectly(t *testing.T) {
	permitUnsafeLaunch(t)
	isolateRoom(t)
	fleettest.Ring(t)
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("HOME", home)

	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveTool(fleet.Tool{
		Name: "msgtool", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Binary: "msgtool", Launch: fleet.ToolLaunch{
			Exec:  "msgtool -p {prompt}",
			Env:   []string{"MSGTOOL_HOME={state_dir}", "MSGTOOL_MODE=pinned"},
			Setup: "mkdir -p {state_dir}",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	execArgv := []string{"message", "send", "--json", "--dry-run"}

	// Dry run: argv[0] is the tool binary, not the bashy setup shell.
	res, err := Invoke(context.Background(), Options{
		Agent: "msgtool", Instruction: "send it", DryRun: true,
		ExecArgv: execArgv, Catalog: cat,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if exeName(res.Agent) != "msgtool" {
		t.Fatalf("dry-run argv[0] = %q, want the tool binary (the setup wrapper swallowed it)", res.Agent)
	}
	if !slices.Equal(res.Args, execArgv) {
		t.Fatalf("dry-run args = %q, want %q", res.Args, execArgv)
	}

	// Live: the runner sees the tool binary with the exec argv, and the
	// launch carries no launch-declared env.
	var gotAgent string
	var gotArgs []string
	var seen Launch
	var ok bool
	r := runnerFunc(func(ctx context.Context, agent string, args []string, cwd string) (string, int, error) {
		gotAgent, gotArgs = agent, append([]string(nil), args...)
		seen, ok = LaunchFrom(ctx)
		return "ok\n", 0, nil
	})
	if _, err := Invoke(context.Background(), Options{
		Agent: "msgtool", Instruction: "send it",
		ExecArgv: execArgv, Catalog: cat, Cwd: t.TempDir(),
	}, r); err != nil {
		t.Fatal(err)
	}
	if exeName(gotAgent) != "msgtool" {
		t.Fatalf("runner argv[0] = %q, want the tool binary (the setup wrapper swallowed it)", gotAgent)
	}
	if !slices.Equal(gotArgs, execArgv) {
		t.Fatalf("runner args = %q, want %q", gotArgs, execArgv)
	}
	if !ok {
		t.Fatal("runner saw no launch")
	}
	for _, kv := range seen.Env {
		if strings.HasPrefix(kv, "MSGTOOL_") {
			t.Fatalf("launch env applied to an exec subcommand: %q (full env %q)", kv, seen.Env)
		}
	}
}

// The owning tool selects the exec binary even when the launch resolves from
// an agent-nick override naming a different tool: `tool cmd run msgtool:send
// --agent other` still runs msgtool, not other's setup shell.
func TestExecOverrideOwnerWinsOverAgentNick(t *testing.T) {
	permitUnsafeLaunch(t)
	isolateRoom(t)
	fleettest.Ring(t)
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("HOME", home)

	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	for _, name := range []string{"msgtool", "other"} {
		if err := cat.SaveTool(fleet.Tool{
			Name: name, Kind: fleet.ToolKindCLI,
			CLI: fleet.ToolCLI{Binary: name, Launch: fleet.ToolLaunch{
				Exec:  name + " -p {prompt}",
				Env:   []string{strings.ToUpper(name) + "_HOME={state_dir}"},
				Setup: "mkdir -p {state_dir}",
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	res, err := Invoke(context.Background(), Options{
		Agent: "other", Instruction: "send it", DryRun: true,
		ExecArgv: []string{"message", "send"}, ExecTool: "msgtool", Catalog: cat,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if exeName(res.Agent) != "msgtool" {
		t.Fatalf("dry-run argv[0] = %q, want the owning tool's binary", res.Agent)
	}
}
