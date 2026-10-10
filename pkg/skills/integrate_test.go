package skills

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/fleet"
)

func TestExpandIntegrationPath(t *testing.T) {
	env := map[string]string{"CODEX_HOME": "/srv/codex"}
	get := func(k string) string { return env[k] }
	for in, want := range map[string]string{
		"~/.claude/skills":                 "/h/.claude/skills",
		"${CODEX_HOME:-~/.codex}/skills":   "/srv/codex/skills",
		"${HERMES_HOME:-~/.hermes}/skills": "/h/.hermes/skills",
		"$CODEX_HOME/AGENTS.md":            "/srv/codex/AGENTS.md",
		".agents/skills":                   ".agents/skills",
	} {
		if got := fleet.ExpandIntegrationPath(in, "/h", get); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func integrationTools() []fleet.Tool {
	return []fleet.Tool{
		{Name: "qwenish", Aliases: []string{"qw"}, Integration: fleet.ToolIntegration{
			Detect:       "~/.qwenish",
			Skills:       fleet.IntegrationSkills{User: []string{"~/.qwenish/skills", "~/.agents/skills"}, Project: []string{".qwenish/skills"}},
			Instructions: []fleet.IntegrationFile{{File: "~/.qwenish/QWEN.md"}, {File: "AGENTS.md", Scope: fleet.ScopeProject}},
		}},
		{Name: "absent", Integration: fleet.ToolIntegration{
			Detect: "~/.absent",
			Skills: fleet.IntegrationSkills{User: []string{"~/.absent/skills"}},
		}},
		{Name: "bare"},
	}
}

func TestToolTargetsNamedDetectedAndErrors(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".qwenish"), 0o755); err != nil {
		t.Fatal(err)
	}
	get := func(string) string { return "" }

	// Detected: qwenish (its config dir exists), not absent.
	tgs, err := toolTargets(integrationTools(), nil, true, fleet.ScopeUser, home, "", get)
	if err != nil || len(tgs) != 1 || tgs[0].Tool != "qwenish" {
		t.Fatalf("detected = %+v, %v", tgs, err)
	}
	if !slices.Contains(tgs[0].SkillRoots, filepath.Join(home, ".qwenish/skills")) ||
		!slices.Equal(tgs[0].Instructions, []string{filepath.Join(home, ".qwenish/QWEN.md")}) {
		t.Fatalf("qwenish target = %+v", tgs[0])
	}
	// Named by alias, explicitly: absent is a target even though undetected.
	tgs, err = toolTargets(integrationTools(), []string{"absent", "qw"}, false, fleet.ScopeUser, home, "", get)
	if err != nil || len(tgs) != 2 {
		t.Fatalf("named = %+v, %v", tgs, err)
	}
	// Project scope joins the repo root.
	tgs, err = toolTargets(integrationTools(), []string{"qwenish"}, false, fleet.ScopeProject, home, "/repo", get)
	if err != nil || !slices.Equal(tgs[0].SkillRoots, []string{"/repo/.qwenish/skills"}) || !slices.Equal(tgs[0].Instructions, []string{"/repo/AGENTS.md"}) {
		t.Fatalf("project = %+v, %v", tgs, err)
	}
	// A typo, or a tool that declares nothing, is an error, never a silent no-op.
	if _, err := toolTargets(integrationTools(), []string{"nope"}, false, fleet.ScopeUser, home, "", get); err == nil {
		t.Fatal("unknown tool accepted")
	}
	if _, err := toolTargets(integrationTools(), []string{"bare"}, false, fleet.ScopeUser, home, "", get); err == nil {
		t.Fatal("tool without integration accepted")
	}
}

func TestUpsertInstructionBlockPreservesTheRestOfTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "AGENTS.md")
	// Created when missing.
	if changed, err := UpsertInstructionBlock(path, "bashy", "v1\n"); err != nil || !changed {
		t.Fatalf("create: %v %v", changed, err)
	}
	// The user's own content around the block survives byte for byte.
	data, _ := os.ReadFile(path)
	user := "# My rules\nkeep me\n\n" + string(data) + "\ntrailing user text\n"
	if err := os.WriteFile(path, []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil { // WriteFile keeps an existing file's mode
		t.Fatal(err)
	}
	if changed, err := UpsertInstructionBlock(path, "bashy", "v2\n"); err != nil || !changed {
		t.Fatalf("replace: %v %v", changed, err)
	}
	got, _ := os.ReadFile(path)
	s := string(got)
	if !strings.HasPrefix(s, "# My rules\nkeep me\n\n") || !strings.HasSuffix(s, "\ntrailing user text\n") {
		t.Fatalf("user content not preserved:\n%s", s)
	}
	if strings.Contains(s, "v1") || strings.Count(s, ":begin") != 1 || !strings.Contains(s, "v2\n") {
		t.Fatalf("block not replaced in place:\n%s", s)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode changed to %v", fi.Mode().Perm())
	}
	// Idempotent.
	if changed, err := UpsertInstructionBlock(path, "bashy", "v2\n"); err != nil || changed {
		t.Fatalf("re-upsert: %v %v", changed, err)
	}
	// A begin marker without its end is refused, not guessed at.
	begin, _ := instructionMarkers("bashy")
	broken := filepath.Join(t.TempDir(), "X.md")
	_ = os.WriteFile(broken, []byte("a\n"+begin+"\nhalf\n"), 0o644)
	if _, err := UpsertInstructionBlock(broken, "bashy", "v3\n"); err == nil {
		t.Fatal("unterminated block accepted")
	}
}

// End to end through the export command: --tool writes the skill folder into
// the tool's declared roots and the pointer block into its instruction file.
func TestExportToolWritesDeclaredRootsAndInstructions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	orig := fleetTools
	fleetTools = integrationTools
	t.Cleanup(func() { fleetTools = orig })

	cfg, _, _ := exportFixture(t)
	var out, errb bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	if err := runExport(cmd, cfg, "guide", "", []string{"absent", "qwenish"}, false, false, false, false, false); err != nil {
		t.Fatalf("export: %v\n%s", err, errb.String())
	}
	for _, dir := range []string{".absent/skills/guide", ".qwenish/skills/guide", ".agents/skills/guide"} {
		if _, err := os.Stat(filepath.Join(home, dir, "SKILL.md")); err != nil {
			t.Errorf("missing %s: %v", dir, err)
		}
	}
	md, err := os.ReadFile(filepath.Join(home, ".qwenish/QWEN.md"))
	if err != nil || !strings.Contains(string(md), "bashy skill show guide") || !strings.Contains(string(md), "the guide") {
		t.Fatalf("instruction file = %q, %v", md, err)
	}
}
