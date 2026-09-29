package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

const toolWithCommands = `name: demo
kind: cli
cli:
  launch:
    exec: demo -p {prompt}
    steer_exec: demo
commands:
  - name: plan
    slash: /plan {args}
    mode: tui
    timeout: 5m
    output: turn
    steps:
      - wait_idle: 30s
      - say: "1"
      - key: esc
    quit: /exit
  - name: review
    slash: /review {args}
    mode: print
    capability: review
    exec: demo exec review --json {args}
    output: file:*.md
`

func TestToolCommandsParse(t *testing.T) {
	tl, err := ParseTool("demo", []byte(toolWithCommands), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Commands) != 2 {
		t.Fatalf("commands = %d, want 2", len(tl.Commands))
	}
	plan, ok := tl.Command("plan")
	if !ok {
		t.Fatal("Command(plan) not found")
	}
	if plan.Slash != "/plan {args}" || plan.Mode != ToolCommandTUI || plan.Timeout != "5m" || plan.Quit != "/exit" {
		t.Errorf("plan parsed wrong: %+v", plan)
	}
	if len(plan.Steps) != 3 || plan.Steps[0].WaitIdle != "30s" || plan.Steps[1].Say != "1" || plan.Steps[2].Key != "esc" {
		t.Errorf("plan steps parsed wrong: %+v", plan.Steps)
	}
	rev, _ := tl.Command("review")
	if rev.Mode != ToolCommandPrint || rev.Capability != "review" || rev.Exec != "demo exec review --json {args}" || rev.Output != "file:*.md" {
		t.Errorf("review parsed wrong: %+v", rev)
	}
	if errs, warns := tl.ValidateCommands(); len(errs) != 0 || len(warns) != 0 {
		t.Errorf("valid commands: errs=%v warns=%v", errs, warns)
	}
	// Canonical YAML round-trips the block.
	out, err := Marshal(tl)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"commands:", "slash: /plan {args}", "wait_idle: 30s", "exec: demo exec review --json {args}"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("marshal lost %q:\n%s", want, out)
		}
	}
}

func TestToolCommandsValidate(t *testing.T) {
	steerable := Tool{Name: "demo", Kind: ToolKindCLI, CLI: ToolCLI{Launch: ToolLaunch{Exec: "demo -p {prompt}", SteerExec: "demo"}}}
	printOnly := Tool{Name: "demo", Kind: ToolKindCLI, CLI: ToolCLI{Launch: ToolLaunch{Exec: "demo -p {prompt}"}}}

	cases := []struct {
		name    string
		tool    Tool
		cmds    []ToolCommand
		wantErr string // substring; "" = valid
	}{
		{"ok print", printOnly, []ToolCommand{{Name: "review", Slash: "/review", Mode: "print"}}, ""},
		{"ok tui", steerable, []ToolCommand{{Name: "exit", Slash: "/exit", Mode: "tui"}}, ""},
		{"empty name", printOnly, []ToolCommand{{Slash: "/x", Mode: "print"}}, "name is empty"},
		{"empty slash", printOnly, []ToolCommand{{Name: "review", Mode: "print"}}, "slash is empty"},
		{"blank slash", printOnly, []ToolCommand{{Name: "review", Slash: "  ", Mode: "print"}}, "slash is empty"},
		{"bad mode", printOnly, []ToolCommand{{Name: "review", Slash: "/r", Mode: "batch"}}, `mode "batch"`},
		{"missing mode", printOnly, []ToolCommand{{Name: "review", Slash: "/r"}}, `mode ""`},
		{"tui needs steer_exec", printOnly, []ToolCommand{{Name: "plan", Slash: "/plan", Mode: "tui"}}, "steer_exec"},
		{"duplicate", printOnly, []ToolCommand{
			{Name: "review", Slash: "/r", Mode: "print"},
			{Name: "review", Slash: "/r2", Mode: "print"},
		}, "duplicate"},
		{"steps on print", steerable, []ToolCommand{{Name: "review", Slash: "/r", Mode: "print", Steps: []ToolCommandStep{{Say: "1"}}}}, "tui only"},
		{"quit on print", steerable, []ToolCommand{{Name: "review", Slash: "/r", Mode: "print", Quit: "/exit"}}, "tui only"},
		{"exec on tui", steerable, []ToolCommand{{Name: "plan", Slash: "/plan", Mode: "tui", Exec: "demo x"}}, "print only"},
		{"empty step", steerable, []ToolCommand{{Name: "plan", Slash: "/plan", Mode: "tui", Steps: []ToolCommandStep{{}}}}, "exactly one"},
		{"two-key step", steerable, []ToolCommand{{Name: "plan", Slash: "/plan", Mode: "tui", Steps: []ToolCommandStep{{Say: "1", Key: "esc"}}}}, "exactly one"},
		{"bad wait_idle", steerable, []ToolCommand{{Name: "plan", Slash: "/plan", Mode: "tui", Steps: []ToolCommandStep{{WaitIdle: "soon"}}}}, "wait_idle"},
		{"bad timeout", printOnly, []ToolCommand{{Name: "review", Slash: "/r", Mode: "print", Timeout: "forever"}}, "timeout"},
		{"bad output", printOnly, []ToolCommand{{Name: "review", Slash: "/r", Mode: "print", Output: "stdout"}}, "output"},
		{"empty file glob", printOnly, []ToolCommand{{Name: "review", Slash: "/r", Mode: "print", Output: "file:"}}, "output"},
		{"bad file glob", printOnly, []ToolCommand{{Name: "review", Slash: "/r", Mode: "print", Output: "file:[x"}}, "output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tl := tc.tool
			tl.Commands = tc.cmds
			errs, _ := tl.ValidateCommands()
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("want valid, got %v", errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("want error containing %q, got none", tc.wantErr)
			}
			if !strings.Contains(errs[0].Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", errs[0], tc.wantErr)
			}
		})
	}
}

// Any name is allowed; a name outside the canonical vocabulary only warns.
func TestToolCommandsUndeclaredNameWarnsNotRefuses(t *testing.T) {
	tl := Tool{Name: "demo", Kind: ToolKindCLI, Commands: []ToolCommand{
		{Name: "frobnicate", Slash: "/frob", Mode: "print"},
		{Name: "review", Slash: "/review", Mode: "print"},
	}}
	errs, warns := tl.ValidateCommands()
	if len(errs) != 0 {
		t.Fatalf("an undeclared name must not be an error: %v", errs)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "frobnicate") {
		t.Fatalf("want one warning naming frobnicate, got %v", warns)
	}
	for _, n := range []string{"plan", "review", "deep-research", "exit"} {
		if !IsCanonicalToolCommand(n) {
			t.Errorf("%q must be canonical", n)
		}
	}
}

// The catalog reports an invalid command without dropping the tool.
func TestCatalogReportsInvalidCommandKeepsTool(t *testing.T) {
	base := fstest.MapFS{
		baselineRoot + "/tools/demo.yaml": {Data: []byte("name: demo\nkind: cli\ncli:\n  launch:\n    exec: demo -p {prompt}\ncommands:\n  - name: plan\n    slash: /plan\n    mode: tui\n")},
	}
	c := New(WithRoot(t.TempDir()), WithoutCloudOverlay(), WithBaselineFS(base))
	tools, errs := c.Tools(false)
	if len(tools) != 1 || tools[0].Name != "demo" || len(tools[0].Commands) != 1 {
		t.Fatalf("tool must survive an invalid command: %+v", tools)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "steer_exec") {
		t.Fatalf("want one steer_exec error, got %v", errs)
	}
}

// A local tool YAML shadows the embedded one, so an overlay adds or overrides
// commands with no rebuild.
func TestToolCommandsOverlayAddsAndOverrides(t *testing.T) {
	base := fstest.MapFS{
		baselineRoot + "/tools/demo.yaml":  {Data: []byte(toolWithCommands)},
		baselineRoot + "/tools/other.yaml": {Data: []byte("name: other\nkind: cli\ncli:\n  launch:\n    exec: other {prompt}\n")},
	}
	root := t.TempDir()
	c := New(WithRoot(root), WithoutCloudOverlay(), WithBaselineFS(base))
	if tl, _ := c.Tool("demo"); len(tl.Commands) != 2 {
		t.Fatalf("baseline commands = %d, want 2", len(tl.Commands))
	}
	if tl, _ := c.Tool("other"); len(tl.Commands) != 0 {
		t.Fatalf("other has no baseline commands, got %d", len(tl.Commands))
	}

	// Override demo:review and add demo:exit; add other:review to a tool
	// that shipped none.
	demo := strings.Replace(toolWithCommands, "slash: /review {args}", "slash: /code-review {args}", 1) +
		"  - name: exit\n    slash: /exit\n    mode: tui\n"
	other := "name: other\nkind: cli\ncli:\n  launch:\n    exec: other {prompt}\ncommands:\n  - name: review\n    slash: /review {args}\n    mode: print\n"
	dir := filepath.Join(root, dirTools)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"demo.yaml": demo, "other.yaml": other} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	c = New(WithRoot(root), WithoutCloudOverlay(), WithBaselineFS(base))
	tl, _ := c.Tool("demo")
	if rev, _ := tl.Command("review"); rev.Slash != "/code-review {args}" {
		t.Errorf("override: review slash = %q", rev.Slash)
	}
	if _, ok := tl.Command("exit"); !ok {
		t.Error("overlay did not add demo:exit")
	}
	if tl.Ring.String() != "local" {
		t.Errorf("ring = %s, want local", tl.Ring)
	}
	ot, _ := c.Tool("other")
	if _, ok := ot.Command("review"); !ok {
		t.Error("overlay did not add other:review")
	}
	if _, errs := c.Tools(false); len(errs) != 0 {
		t.Errorf("overlay commands are valid, got %v", errs)
	}
}

// The schema lists the new fields, each with a doc tag.
func TestToolSchemaListsCommandFields(t *testing.T) {
	got := map[string]string{}
	for _, f := range schemaFields(KindTool) {
		got[f.Path] = f.Description
	}
	for _, p := range []string{
		"commands", "commands.<index>.name", "commands.<index>.slash", "commands.<index>.mode",
		"commands.<index>.capability", "commands.<index>.timeout", "commands.<index>.output",
		"commands.<index>.exec", "commands.<index>.steps", "commands.<index>.steps.<index>.say",
		"commands.<index>.steps.<index>.key", "commands.<index>.steps.<index>.wait_idle",
		"commands.<index>.quit",
	} {
		d, ok := got[p]
		if !ok {
			t.Errorf("schema lacks %s", p)
			continue
		}
		if d == "" {
			t.Errorf("schema %s has no doc", p)
		}
	}
}

// The shipped baseline carries only MEASURED commands (Sprint #324 S6), and
// every one must validate with no error and no warning: a seed that warns is
// a seed nobody measured under its canonical name.
func TestBaselineToolCommandsValidate(t *testing.T) {
	entries, err := baselineFS.ReadDir("baseline/tools")
	if err != nil {
		t.Fatal(err)
	}
	seeded := map[string][]string{}
	for _, e := range entries {
		body, err := baselineFS.ReadFile("baseline/tools/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		tool, err := ParseTool(strings.TrimSuffix(e.Name(), ".yaml"), body, nil)
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		errs, warns := tool.ValidateCommands()
		for _, e := range errs {
			t.Errorf("baseline %v", e)
		}
		for _, w := range warns {
			t.Errorf("baseline warning: %s", w)
		}
		for _, c := range tool.Commands {
			seeded[tool.Name] = append(seeded[tool.Name], c.Name+"/"+c.Mode)
		}
	}
	want := map[string][]string{"claude": {"plan/tui"}, "codex": {"review/print"}}
	if len(seeded) != len(want) {
		t.Errorf("seeded commands %v, want exactly %v (seed only what is measured)", seeded, want)
	}
	for tool, cmds := range want {
		if strings.Join(seeded[tool], ",") != strings.Join(cmds, ",") {
			t.Errorf("%s commands %v, want %v", tool, seeded[tool], cmds)
		}
	}
}
