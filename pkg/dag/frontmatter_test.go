// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const fmTasks = "\n## Tasks\n\n### build\nBuild it.\n\n```bash\necho build\n```\n\n### test\nTest it.\nRequires: build\n\n```bash\necho test\n```\n"

func parseFM(t *testing.T, fm string) *Document {
	t.Helper()
	doc, err := Parse(strings.NewReader("---\n"+fm+"---\n"+fmTasks), "dag.md")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return doc
}

// The legacy line-reading shapes parse exactly as before.
func TestFrontmatterLegacyShapes(t *testing.T) {
	doc := parseFM(t, "name: uv\ndescription: fetch, build — and test\ndefault: test\n"+
		"include: a.md b.md\nvars:\n  TEST_CRATE ?= uv-pep440\n  MODE := fast\n")
	if doc.Name != "uv" || doc.Desc != "fetch, build — and test" || doc.Default != "test" {
		t.Errorf("name/desc/default = %q / %q / %q", doc.Name, doc.Desc, doc.Default)
	}
	if !reflect.DeepEqual(doc.Includes, []string{"a.md", "b.md"}) {
		t.Errorf("includes = %v", doc.Includes)
	}
	want := []DocVar{{"TEST_CRATE", "?=", "uv-pep440"}, {"MODE", ":=", "fast"}}
	if !reflect.DeepEqual(doc.Vars, want) {
		t.Errorf("vars = %+v, want %+v", doc.Vars, want)
	}
	if doc.FrontmatterYAMLErr != "" {
		t.Errorf("legacy shape is strict YAML, got err %q", doc.FrontmatterYAMLErr)
	}
}

// A description with `: ` inside is not strict YAML: the line reading still
// wins, and the reason is recorded for --check.
func TestFrontmatterNotYAMLKeepsLineReading(t *testing.T) {
	doc := parseFM(t, "name: x\ndescription: runs it: fast\n")
	if doc.Desc != "runs it: fast" {
		t.Errorf("desc = %q", doc.Desc)
	}
	if doc.FrontmatterYAMLErr == "" {
		t.Error("want a YAML error recorded")
	}
}

// The spec-clean shapes an Agent Skill / OKF author writes.
func TestFrontmatterYAMLShapes(t *testing.T) {
	doc := parseFM(t, "name: uv\n"+
		"description: >\n  Build and test uv.\n  Use when asked to build it.\n"+
		"type: dag # OKF page type\n"+
		"metadata:\n  default: test\n  include:\n    - a.md\n    - b.md\n"+
		"  vars:\n    - TEST_CRATE ?= uv-pep440\n    - MODE = fast\n  author: someone\n")
	if doc.Desc != "Build and test uv. Use when asked to build it." {
		t.Errorf("desc = %q", doc.Desc)
	}
	if doc.Type != "dag" || doc.Default != "test" {
		t.Errorf("type/default = %q / %q", doc.Type, doc.Default)
	}
	if !reflect.DeepEqual(doc.Includes, []string{"a.md", "b.md"}) {
		t.Errorf("includes = %v", doc.Includes)
	}
	want := []DocVar{{"TEST_CRATE", "?=", "uv-pep440"}, {"MODE", "=", "fast"}}
	if !reflect.DeepEqual(doc.Vars, want) {
		t.Errorf("vars = %+v, want %+v", doc.Vars, want)
	}
}

func TestFrontmatterTopLevelWinsOverMetadata(t *testing.T) {
	doc := parseFM(t, "name: x\ndefault: build\nmetadata:\n  default: test\n  vars:\n    HOST: a\n")
	if doc.Default != "build" {
		t.Errorf("default = %q, want the top-level build", doc.Default)
	}
	if !reflect.DeepEqual(doc.Vars, []DocVar{{"HOST", "=", "a"}}) {
		t.Errorf("vars = %+v", doc.Vars)
	}
}

func TestFrontmatterWarnings(t *testing.T) {
	clean := parseFM(t, "name: uv\ndescription: Build uv.\ntype: dag\n")
	if w := frontmatterWarnings(clean); len(w) != 0 {
		t.Errorf("clean header warned: %v", w)
	}
	bad := parseFM(t, "name: My Build\ndescription: runs it: fast\n")
	got := strings.Join(frontmatterWarnings(bad), "\n")
	for _, want := range []string{"not strict YAML", "not a skill name", "no type"} {
		if !strings.Contains(got, want) {
			t.Errorf("warnings missing %q:\n%s", want, got)
		}
	}
	none, _ := Parse(strings.NewReader(fmTasks), "dag.md")
	if w := frontmatterWarnings(none); len(w) != 1 || !strings.Contains(w[0], "no frontmatter") {
		t.Errorf("no-frontmatter warnings = %v", w)
	}
}

func TestSkillProjection(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "dag.md")
	os.WriteFile(src, []byte("---\nname: Demo Build\ndescription: Build the <demo>.\ndefault: test\n---\n"+fmTasks), 0o644)
	out := filepath.Join(dir, "skills")

	run := func() (string, error) {
		cmd := NewDagCmd()
		var b bytes.Buffer
		cmd.SetOut(&b)
		cmd.SetErr(&b)
		cmd.SetArgs([]string{"--file", src, "--skill", out})
		err := cmd.Execute()
		return b.String(), err
	}
	if msg, err := run(); err != nil {
		t.Fatalf("--skill: %v\n%s", err, msg)
	}
	b, err := os.ReadFile(filepath.Join(out, "demo-build", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)

	// Frontmatter: strict YAML, Agent Skills shape.
	parts := strings.SplitN(s, "---\n", 3)
	if len(parts) != 3 || parts[0] != "" {
		t.Fatalf("no frontmatter block:\n%s", s)
	}
	var fm map[string]any
	if err := yaml.Unmarshal([]byte(parts[1]), &fm); err != nil {
		t.Fatalf("frontmatter not YAML: %v", err)
	}
	for k := range fm {
		switch k {
		case "name", "description", "license", "allowed-tools", "metadata", "compatibility":
		default:
			t.Errorf("top-level key %q is not allowed in an Agent Skill", k)
		}
	}
	if fm["name"] != "demo-build" {
		t.Errorf("name = %v", fm["name"])
	}
	desc, _ := fm["description"].(string)
	if strings.ContainsAny(desc, "<>\n") || !strings.Contains(desc, "Use when") || !strings.Contains(desc, "build, test") {
		t.Errorf("description = %q", desc)
	}
	for _, want := range []string{"bashy dag -f dag.md --list", "| `test` | build | Test it. |", "runs `test`"} {
		if !strings.Contains(s, want) {
			t.Errorf("SKILL.md missing %q", want)
		}
	}
	if strings.Contains(s, dir) {
		t.Errorf("SKILL.md leaks the host path %s", dir)
	}

	// Regenerating overwrites its own output; a hand-written SKILL.md is safe.
	if msg, err := run(); err != nil {
		t.Fatalf("re-run: %v\n%s", err, msg)
	}
	os.WriteFile(filepath.Join(out, "demo-build", "SKILL.md"), []byte("---\nname: demo-build\n---\nmine\n"), 0o644)
	if _, err := run(); err == nil {
		t.Error("clobbered a hand-written SKILL.md")
	}
}
