package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/qiangli/yoke/pkg/assetring"
	"github.com/spf13/cobra"
)

func TestSparseSetFollowsLaterBaselineFix(t *testing.T) {
	for _, tc := range []struct {
		noun, file, name, old, fixed string
		command                      func(...Option) *cobra.Command
		args                         []string
		check                        func(*Catalog) (string, string)
	}{
		{"tools", "baseline/tools/runner.yaml", "runner", "old {prompt}", "fixed {prompt}", NewToolsCmd,
			[]string{"--binary", "pinned"}, func(c *Catalog) (string, string) { x, _ := c.Tool("runner"); return x.CLI.Binary, x.CLI.Launch.Exec }},
		{"models", "baseline/models/seat.yaml", "seat", "old", "fixed", NewModelsCmd,
			[]string{"--upstream", "pinned"}, func(c *Catalog) (string, string) { x, _ := c.Model("seat"); return x.UpstreamID, x.Display }},
		{"agents", "baseline/agents/worker.yaml", "worker", "old", "fixed", NewAgentsCmd,
			[]string{"--nick", "Pinned"}, func(c *Catalog) (string, string) { x, _ := c.Agent("worker"); return x.Nick, x.Description }},
	} {
		t.Run(tc.noun, func(t *testing.T) {
			base := fstest.MapFS{}
			baseline := func(v string) string {
				switch tc.noun {
				case "tools":
					return "name: runner\nkind: cli\ncli:\n  binary: runner\n  launch:\n    exec: " + v + "\n"
				case "models":
					return "name: seat\nupstream_id: upstream\ndisplay: " + v + "\n"
				default:
					return "agents:\n  - name: worker\n    tool: runner\n    model: seat\n    description: " + v + "\n"
				}
			}
			base[tc.file] = &fstest.MapFile{Data: []byte(baseline(tc.old))}
			root := t.TempDir()
			opts := []Option{WithRoot(root), WithBaselineFS(base), WithoutCloudOverlay()}
			if _, err := runCmd(t, tc.command(opts...), append([]string{"set", tc.name}, tc.args...)...); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(root, tc.noun, tc.name+".yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), tc.old) {
				t.Fatalf("local copy froze baseline: %s", data)
			}
			base[tc.file] = &fstest.MapFile{Data: []byte(baseline(tc.fixed))}
			pin, inherited := tc.check(New(opts...))
			if pin != "pinned" && pin != "Pinned" || inherited != tc.fixed {
				t.Fatalf("pin=%q inherited=%q", pin, inherited)
			}
		})
	}
}

func TestSparseSetDistinguishesClearFromAbsent(t *testing.T) {
	base := fstest.MapFS{"baseline/models/seat.yaml": &fstest.MapFile{Data: []byte("name: seat\ndisplay: Before\nquality: 0.8\n")}}
	root := t.TempDir()
	opts := []Option{WithRoot(root), WithBaselineFS(base), WithoutCloudOverlay()}
	if _, err := runCmd(t, NewModelsCmd(opts...), "set", "seat", "--display", "", "--quality", "0"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "models", "seat.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "display: null") || !strings.Contains(string(data), "quality: null") {
		t.Fatalf("clear was lost: %s", data)
	}
	base["baseline/models/seat.yaml"] = &fstest.MapFile{Data: []byte("name: seat\ndisplay: Later\nquality: 0.9\nprovider: improved\n")}
	m, _ := New(opts...).Model("seat")
	if m.Display != "" || m.Quality != 0 || m.Provider != "improved" {
		t.Fatalf("clear/inheritance = %+v", m)
	}
}

func TestMigrateOverrideRequiresReviewAndBacksUp(t *testing.T) {
	base := fstest.MapFS{"baseline/tools/runner.yaml": &fstest.MapFile{Data: []byte("name: runner\nkind: cli\ncli:\n  binary: runner\n  launch:\n    exec: old {prompt}\n")}}
	root := t.TempDir()
	opts := []Option{WithRoot(root), WithBaselineFS(base), WithoutCloudOverlay()}
	cat := New(opts...)
	tool, _ := cat.Tool("runner")
	tool.CLI.Binary = "pin"
	legacy, err := Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeEntry(filepath.Join(root, "tools"), "runner", legacy); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "tools", "runner.yaml")
	old, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := runCmd(t, NewToolsCmd(opts...), "migrate", "runner")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(preview, "overlay: true") || !strings.Contains(preview, "binary: pin") || strings.Contains(preview, "exec:") {
		t.Fatalf("candidate = %s", preview)
	}
	reviewed := filepath.Join(root, "reviewed.yaml")
	if err := os.WriteFile(reviewed, []byte("overlay: true\nname: runner\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, NewToolsCmd(opts...), "migrate", "runner", "--reviewed", reviewed); err == nil {
		t.Fatal("changed effective value was accepted")
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("backup created for rejected migration: %v", err)
	}
	if err := os.WriteFile(reviewed, []byte("overlay: true\nname: runner\ncli:\n  binary: pin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, NewToolsCmd(opts...), "migrate", "runner", "--reviewed", reviewed); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != string(old) {
		t.Fatal("backup changed")
	}
	base["baseline/tools/runner.yaml"] = &fstest.MapFile{Data: []byte("name: runner\nkind: cli\ncli:\n  binary: runner\n  launch:\n    exec: fixed {prompt}\n")}
	got, _ := cat.Tool("runner")
	if got.CLI.Binary != "pin" || got.CLI.Launch.Exec != "fixed {prompt}" {
		t.Fatalf("migrated override = %+v", got)
	}
}

func TestOverlayUsesNextLowerWinnerAndKeepsEarlierPins(t *testing.T) {
	base := fstest.MapFS{"baseline/tools/runner.yaml": &fstest.MapFile{Data: []byte("name: runner\nkind: cli\ncli:\n  binary: runner\n  launch:\n    exec: embedded {prompt}\n")}}
	shared := fstest.MapFS{"runner.yaml": &fstest.MapFile{Data: []byte("name: runner\nkind: cli\ncli:\n  binary: runner\n  launch:\n    exec: shared-old {prompt}\n")}}
	root := t.TempDir()
	opts := []Option{WithRoot(root), WithBaselineFS(base), WithSource(dirTools, assetring.FileFS(shared, assetring.RingShared, ext)), WithoutCloudOverlay()}
	for _, args := range [][]string{{"set", "runner", "--binary", "pinned"}, {"set", "runner", "--display", "Local"}} {
		if _, err := runCmd(t, NewToolsCmd(opts...), args...); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "tools", "runner.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "shared-old") {
		t.Fatalf("froze shared value: %s", data)
	}
	shared["runner.yaml"] = &fstest.MapFile{Data: []byte("name: runner\nkind: cli\ncli:\n  binary: runner\n  launch:\n    exec: shared-fixed {prompt}\n")}
	got, _ := New(opts...).Tool("runner")
	if got.CLI.Binary != "pinned" || got.Display != "Local" || got.CLI.Launch.Exec != "shared-fixed {prompt}" {
		t.Fatalf("merged tool = %+v", got)
	}
}

func TestSparseClearFalseAndBinaryFallback(t *testing.T) {
	base := fstest.MapFS{
		"baseline/tools/runner.yaml":  &fstest.MapFile{Data: []byte("name: runner\nkind: cli\ncli:\n  binary: runner\n  launch:\n    exec: runner {prompt}\n")},
		"baseline/agents/worker.yaml": &fstest.MapFile{Data: []byte("agents:\n  - name: worker\n    tool: runner\n    model: seat\n    ephemeral: true\n")},
	}
	root := t.TempDir()
	opts := []Option{WithRoot(root), WithBaselineFS(base), WithoutCloudOverlay()}
	if _, err := runCmd(t, NewToolsCmd(opts...), "set", "runner", "--binary", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, NewAgentsCmd(opts...), "set", "worker", "--ephemeral=false"); err != nil {
		t.Fatal(err)
	}
	tool, _ := New(opts...).Tool("runner")
	agent, _ := New(opts...).Agent("worker")
	if tool.CLI.Binary != "" || agent.Ephemeral {
		t.Fatalf("clear failed: binary=%q ephemeral=%v", tool.CLI.Binary, agent.Ephemeral)
	}
}

func TestSparseAgentOverlayAcceptsBareLowerFile(t *testing.T) {
	base := fstest.MapFS{"baseline/agents/worker.yaml": &fstest.MapFile{Data: []byte("name: worker\ntool: runner\nmodel: seat\ndescription: before\n")}}
	root := t.TempDir()
	opts := []Option{WithRoot(root), WithBaselineFS(base), WithoutCloudOverlay()}
	if _, err := runCmd(t, NewAgentsCmd(opts...), "set", "worker", "--nick", "Pinned"); err != nil {
		t.Fatal(err)
	}
	base["baseline/agents/worker.yaml"] = &fstest.MapFile{Data: []byte("name: worker\ntool: runner\nmodel: seat\ndescription: after\n")}
	a, ok := New(opts...).Agent("worker")
	if !ok || a.Nick != "Pinned" || a.Description != "after" {
		t.Fatalf("bare lower merged to %+v, found=%v", a, ok)
	}
}

func TestSparseAgentOverlayInMultiAgentFile(t *testing.T) {
	base := fstest.MapFS{"baseline/agents/team.yaml": &fstest.MapFile{Data: []byte("agents:\n  - name: first\n    tool: runner\n    model: seat\n    description: first-old\n  - name: second\n    tool: runner\n    model: seat\n    description: second-old\n")}}
	root := t.TempDir()
	opts := []Option{WithRoot(root), WithBaselineFS(base), WithoutCloudOverlay()}
	for _, name := range []string{"first", "second"} {
		if _, err := runCmd(t, NewAgentsCmd(opts...), "set", name, "--nick", name+"-pin"); err != nil {
			t.Fatal(err)
		}
	}
	base["baseline/agents/team.yaml"] = &fstest.MapFile{Data: []byte("agents:\n  - name: first\n    tool: runner\n    model: seat\n    description: first-fixed\n  - name: second\n    tool: runner\n    model: seat\n    description: second-fixed\n")}
	cat := New(opts...)
	for _, name := range []string{"first", "second"} {
		a, ok := cat.Agent(name)
		if !ok || a.Nick != name+"-pin" || a.Description != name+"-fixed" {
			t.Fatalf("%s = %+v found=%v", name, a, ok)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "team.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := cat.RemoveAgent("first"); err != nil {
		t.Fatal(err)
	}
	first, _ := cat.Agent("first")
	second, _ := cat.Agent("second")
	if first.Nick != "" || second.Nick != "second-pin" {
		t.Fatalf("removal affected sibling: first=%+v second=%+v", first, second)
	}
}
