package fleet

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestRetireOverlayRoundTrip(t *testing.T) {
	base := fstest.MapFS{"baseline/models/seat.yaml": &fstest.MapFile{Data: []byte("name: seat\ndisplay: Original\n")}}
	root := t.TempDir()
	opts := []Option{WithRoot(root), WithBaselineFS(base), WithoutCloudOverlay()}
	if _, err := runCmd(t, NewModelsCmd(opts...), "retire", "seat", "--reason", "superseded", "--replaced-by", "next"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "models", "seat.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "overlay: true") || strings.Contains(string(data), "Original") {
		t.Fatalf("not sparse: %s", data)
	}
	for _, args := range [][]string{{"list", "--json"}, {"list", "--all", "--json"}} {
		out, err := runCmd(t, NewModelsCmd(opts...), args...)
		if err != nil || strings.Contains(out, `"seat"`) {
			t.Fatalf("retired visible: %s %v", out, err)
		}
	}
	out, err := runCmd(t, NewModelsCmd(opts...), "list", "--retired", "--json")
	if err != nil || !strings.Contains(out, `"seat"`) {
		t.Fatalf("retired missing: %s %v", out, err)
	}
	base["baseline/models/seat.yaml"].Data = []byte("name: seat\ndisplay: Updated\n")
	out, err = runCmd(t, NewModelsCmd(opts...), "show", "seat", "--json")
	if err != nil || !strings.Contains(out, "Updated") || !strings.Contains(out, "replaced_by") {
		t.Fatalf("show: %s %v", out, err)
	}
	if _, err := runCmd(t, NewModelsCmd(opts...), "unretire", "seat"); err != nil {
		t.Fatal(err)
	}
	out, err = runCmd(t, NewModelsCmd(opts...), "show", "seat", "--json")
	if err != nil || strings.Contains(out, `"retired"`) {
		t.Fatalf("unretire: %s %v", out, err)
	}
}

func TestRetirementEveryKind(t *testing.T) {
	for _, spec := range kinds {
		t.Run(spec.Name, func(t *testing.T) {
			shared := t.TempDir()
			root := t.TempDir()
			body := "name: entry\n"
			switch spec.Name {
			case KindTool:
				body += "kind: cli\ncli:\n  launch:\n    exec: runner {prompt}\n"
			case KindPerson:
				body = "handle: entry\ndisplay: Person\n"
			case KindAgent:
				body = "agents:\n  - name: entry\n    tool: runner\n    model: seat\n  - name: sibling\n    tool: runner\n    model: seat\n"
			case KindCommand:
				body += "kind: command\nscript: echo ok\n"
			case KindApp:
				body += "port: 8181\n"
			}
			if err := os.WriteFile(filepath.Join(shared, "entry.yaml"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(spec.PathEnv, shared)
			cat := New(WithRoot(root), WithBaselineFS(fstest.MapFS{}), WithoutCloudOverlay())
			if err := cat.Retire(spec.Name, "entry", "superseded", "next"); err != nil {
				t.Fatal(err)
			}
			life, ok := spec.Lifecycle(cat, "entry")
			if !ok || !life.IsRetired() || life.Retired.ReplacedBy != "next" {
				t.Fatalf("retirement did not resolve: %+v %v", life, ok)
			}
			if _, err := time.Parse(time.RFC3339, life.Retired.At); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(root, spec.Plural, "entry.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if !isOverlay(data) || strings.Contains(string(data), "runner") || strings.Contains(string(data), "8181") {
				t.Fatalf("not sparse: %s", data)
			}
			if err := cat.Unretire(spec.Name, "entry"); err != nil {
				t.Fatal(err)
			}
			life, ok = spec.Lifecycle(cat, "entry")
			if !ok || life.IsRetired() {
				t.Fatalf("unretire: %+v %v", life, ok)
			}
			if spec.Name == KindAgent {
				a, ok := cat.Agent("sibling")
				if !ok || a.IsRetired() {
					t.Fatalf("sibling changed: %+v", a)
				}
			}
		})
	}
}

func TestRetirementLocalBundleAndSingle(t *testing.T) {
	for _, bundled := range []bool{false, true} {
		t.Run(fmt.Sprint(bundled), func(t *testing.T) {
			for _, local := range []bool{false, true} {
				root := t.TempDir()
				body := "name: worker\ntool: runner\nmodel: seat\ndescription: preserved\n"
				if bundled {
					body = "agents:\n - name: worker\n   tool: runner\n   model: seat\n   description: preserved\n - name: sibling\n   tool: runner\n   model: seat\n"
				}
				base := fstest.MapFS{}
				if local {
					if err := os.MkdirAll(filepath.Join(root, "agents"), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(root, "agents", "worker.yaml"), []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					base["baseline/agents/worker.yaml"] = &fstest.MapFile{Data: []byte(body)}
				}
				c := New(WithRoot(root), WithBaselineFS(base), WithoutCloudOverlay())
				if err := c.Retire(KindAgent, "worker", "", "next"); err != nil {
					t.Fatal(err)
				}
				a, ok := c.Agent("worker")
				if !ok || !a.IsRetired() || a.Description != "preserved" {
					t.Fatalf("local=%v agent=%+v ok=%v", local, a, ok)
				}
				if err := c.Unretire(KindAgent, "worker"); err != nil {
					t.Fatal(err)
				}
				a, ok = c.Agent("worker")
				if !ok || a.IsRetired() {
					t.Fatalf("unretire: %+v %v", a, ok)
				}
			}
		})
	}
}

func TestRetiredDependencyAvailability(t *testing.T) {
	for _, kind := range []string{KindTool, KindModel} {
		t.Run(kind, func(t *testing.T) {
			c := New(WithRoot(t.TempDir()), WithBaselineFS(fstest.MapFS{}), WithoutCloudOverlay())
			if err := c.SaveTool(Tool{Name: "runner", Kind: ToolKindCLI}); err != nil {
				t.Fatal(err)
			}
			if err := c.SaveModel(Model{Name: "seat"}); err != nil {
				t.Fatal(err)
			}
			if err := c.SaveAgent(Agent{Name: "worker", Tool: "runner", Model: "seat"}); err != nil {
				t.Fatal(err)
			}
			name := "runner"
			if kind == KindModel {
				name = "seat"
			}
			if err := c.Retire(kind, name, "", "next"); err != nil {
				t.Fatal(err)
			}
			a, _, _, err := c.Binding("worker")
			if err != nil || a.IsRetired() {
				t.Fatalf("history binding: %+v %v", a, err)
			}
			if err := c.AgentAvailability(a); err == nil || !strings.Contains(err.Error(), "unavailable: dependency retired") || !strings.Contains(err.Error(), "use next") {
				t.Fatalf("availability: %v", err)
			}
		})
	}
}

func TestRetirementReplacesInheritedMetadata(t *testing.T) {
	base := fstest.MapFS{"baseline/models/seat.yaml": &fstest.MapFile{Data: []byte("name: seat\nretired:\n  at: before\n  reason: old\n  replaced_by: obsolete\n")}}
	c := New(WithRoot(t.TempDir()), WithBaselineFS(base), WithoutCloudOverlay())
	if err := c.Retire(KindModel, "seat", "", ""); err != nil {
		t.Fatal(err)
	}
	m, ok := c.Model("seat")
	if !ok || m.Retired == nil || m.Retired.Reason != "" || m.Retired.ReplacedBy != "" {
		t.Fatalf("inherited stale metadata: %+v", m.Retired)
	}
	if err := c.Unretire(KindModel, "seat"); err != nil {
		t.Fatal(err)
	}
	m, ok = c.Model("seat")
	if !ok || m.IsRetired() {
		t.Fatalf("inherited retirement was not cleared: %+v", m)
	}
}

func TestRetirementRMWarning(t *testing.T) {
	for _, retired := range []bool{false, true} {
		c := New(WithRoot(t.TempDir()), WithBaselineFS(fstest.MapFS{}), WithoutCloudOverlay())
		if err := c.SaveModel(Model{Name: "seat"}); err != nil {
			t.Fatal(err)
		}
		if retired {
			if err := c.Retire(KindModel, "seat", "", ""); err != nil {
				t.Fatal(err)
			}
		}
		out, err := runCmd(t, NewModelsCmd(WithRoot(c.Root()), WithBaselineFS(fstest.MapFS{}), WithoutCloudOverlay()), "rm", "seat")
		if err != nil || strings.Contains(out, "not retired") == retired {
			t.Fatalf("retired=%v output=%s error=%v", retired, out, err)
		}
	}
}

func TestRetiredDependencyRejectsNewBinding(t *testing.T) {
	opts := []Option{WithRoot(t.TempDir()), WithBaselineFS(fstest.MapFS{}), WithoutCloudOverlay()}
	c := New(opts...)
	if err := c.SaveTool(Tool{Name: "runner", Kind: ToolKindCLI}); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveModel(Model{Name: "seat"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Retire(KindModel, "seat", "", "next"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, NewAgentsCmd(opts...), "add", "new-worker", "--tool", "runner", "--model", "seat"); err == nil || !strings.Contains(err.Error(), "retired; use next") {
		t.Fatalf("new binding: %v", err)
	}
	if _, err := runCmd(t, NewAgentsCmd(opts...), "add", "new-worker", "--tool", "runner", "--model", "seat", "--allow-retired"); err != nil {
		t.Fatal(err)
	}
	cmd := NewAgentsCmd(opts...)
	cmd.SetIn(strings.NewReader("agents:\n - name: imported-worker\n   tool: runner\n   model: seat\n"))
	if _, err := runCmd(t, cmd, "add", "-"); err == nil || !strings.Contains(err.Error(), "retired; use next") {
		t.Fatalf("import binding: %v", err)
	}
}
