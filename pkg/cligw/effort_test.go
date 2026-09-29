package cligw

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
)

// Declared effort reaches the CLI with the tool's own flag; undeclared, the
// argv is byte-for-byte what TestMeasuredWarmArgv pins.
func TestEffortArgv(t *testing.T) {
	claude := func(effort string) *Worker {
		return &Worker{mode: WarmStdinStreamJSON, launch: agentlaunch.Launch{Tool: "claude", Effort: effort, Args: []string{"--model", "M", "-p"}},
			tool: fleet.Tool{Name: "claude", CLI: fleet.ToolCLI{Launch: fleet.ToolLaunch{EventsStdout: "--output-format stream-json --verbose"}}}}
	}
	codex := func(effort string) *Worker {
		return &Worker{cwd: "/tmp/cligw-test", mode: WarmStdin, launch: agentlaunch.Launch{Tool: "codex", Effort: effort, Args: []string{"exec", "--skip-git-repo-check", "--sandbox", "read-only"}},
			tool: fleet.Tool{Name: "codex", CLI: fleet.ToolCLI{Launch: fleet.ToolLaunch{EventsStdout: "--json"}}}}
	}
	claudeBase := claude("").argv("", "")
	wantClaude := append(append([]string{}, claudeBase...), "--effort", "high")
	if got := claude("high").argv("", ""); !reflect.DeepEqual(got, wantClaude) {
		t.Errorf("claude argv = %#v\nwant %#v", got, wantClaude)
	}
	codexBase := codex("").argv("", "")
	wantCodex := append([]string{"codex", "exec", "-c", `model_reasoning_effort="xhigh"`}, codexBase[2:]...)
	if got := codex("xhigh").argv("", ""); !reflect.DeepEqual(got, wantCodex) {
		t.Errorf("codex argv = %#v\nwant %#v", got, wantCodex)
	}
	for _, base := range [][]string{claudeBase, codexBase} {
		joined := strings.Join(base, " ")
		if strings.Contains(joined, "effort") {
			t.Errorf("undeclared effort leaked into argv: %s", joined)
		}
	}
}

func installEffortCatalog(t *testing.T, toolName, effort string) {
	t.Helper()
	t.Setenv("BASHY_SELF", filepath.Join(t.TempDir(), "no-such-bashy"))
	cat := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithBaselineFS(fstest.MapFS{}))
	launch := fleet.ToolLaunch{Exec: "/nonexistent/" + toolName + " -p {prompt}", Warm: string(WarmCold), EventsStdout: "--json"}
	if err := cat.SaveTool(fleet.Tool{Name: toolName, Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: "/nonexistent/" + toolName, Launch: launch}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "effort-agent", Tool: toolName, Effort: effort}); err != nil {
		t.Fatal(err)
	}
	old := agentlaunch.NewCatalog
	agentlaunch.NewCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { agentlaunch.NewCatalog = old })
}

// The declared effort flows fleet agent -> launch -> worker argv (a cold
// worker starts no process until Do, so nothing is exec'd here).
func TestNewWorkerCarriesDeclaredEffort(t *testing.T) {
	installEffortCatalog(t, "claude", "max")
	w, err := NewWorker(context.Background(), "effort-agent")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	got := strings.Join(w.argv("hi", ""), " ")
	if !strings.Contains(got, "--effort max") {
		t.Fatalf("argv lacks --effort max: %s", got)
	}
}

// A tool with no known effort flag is refused loudly, never silently served
// at its default while the identity claims otherwise.
func TestNewWorkerRefusesEffortForUnsupportedTool(t *testing.T) {
	installEffortCatalog(t, "agy", "high")
	w, err := NewWorker(context.Background(), "effort-agent")
	if err == nil {
		w.Close()
		t.Fatal("agy with a declared effort was accepted")
	}
	if !strings.Contains(err.Error(), "no known effort flag") || !strings.Contains(err.Error(), `"agy"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestSaveAgentRejectsFlagShapedEffort(t *testing.T) {
	cat := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithBaselineFS(fstest.MapFS{}))
	for _, bad := range []string{"high --x", "High", `high" -c x="y`} {
		if err := cat.SaveAgent(fleet.Agent{Name: "a", Tool: "claude", Effort: bad}); err == nil {
			t.Errorf("effort %q accepted", bad)
		}
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "a", Tool: "claude", Effort: "xhigh"}); err != nil {
		t.Errorf("xhigh refused: %v", err)
	}
}

// The catalog projection carries the effort the broker freezes and re-checks.
func TestCatalogAgentCarriesEffort(t *testing.T) {
	cat := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithBaselineFS(fstest.MapFS{}))
	launch := fleet.ToolLaunch{Exec: "/nonexistent/claude -p {prompt}", Warm: string(WarmCold)}
	if err := cat.SaveTool(fleet.Tool{Name: "claude", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: "/nonexistent/claude", Launch: launch}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{Name: "m1", Band: 3}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "claude-m1", Tool: "claude", Model: "m1", Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	a, ok := NewFleetCatalog(cat).Agent("claude-m1")
	if !ok {
		t.Fatal("agent not projected")
	}
	if a.Effort != "high" {
		t.Fatalf("effort = %q", a.Effort)
	}
}
