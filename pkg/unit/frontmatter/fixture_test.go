// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// This file is Sprint 420 story #1895's acceptance test: ONE table of
// frontmatter fixtures, fed through BOTH consumers of the shared reader —
// pkg/skills.ParseFrontmatter and pkg/dag.Parse — so the two can never
// drift apart on name, description or type again. dag's own keys (default,
// include, vars) layer on top of the shared four; skills ignores them.

package frontmatter_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/dag"
	"github.com/qiangli/yoke/pkg/skills"
	"github.com/qiangli/yoke/pkg/unit/frontmatter"
)

// tasksBody keeps dag.Parse happy: one target after the frontmatter.
const tasksBody = "\n## Tasks\n\n### build\nBuild it.\n\n```bash\necho build\n```\n"

var fmFixtures = []struct {
	name string
	fm   string

	// Shared expectations (the keys both readers honor).
	wantName, wantDesc, wantType string

	skillsErr bool // skills refuses the block (not strict YAML)

	// dag-only layering, kept exactly as the line reading + YAML read
	// produced it before the shared reader existed.
	wantDefault  string
	wantIncludes []string
	wantVars     []dag.DocVar
}{
	{
		name: "spec-clean skill/OKF shape",
		fm: "name: uv\n" +
			"description: >\n  Build and test uv.\n  Use when asked to build it.\n" +
			"type: dag # OKF page type\n" +
			"license: MIT-0\n" +
			"metadata:\n  default: fmt-check\n  author: someone\n",
		wantName:    "uv",
		wantDesc:    "Build and test uv. Use when asked to build it.",
		wantType:    "dag",
		wantDefault: "fmt-check",
	},
	{
		name: "legacy dag header",
		fm: "name: demo\ndescription: fetch, build — and test\ndefault: test\n" +
			"include: a.md b.md\nvars:\n  TEST_CRATE ?= uv-pep440\n  MODE := fast\n",
		wantName:     "demo",
		wantDesc:     "fetch, build — and test",
		wantDefault:  "test",
		wantIncludes: []string{"a.md", "b.md"},
		wantVars: []dag.DocVar{
			{Name: "TEST_CRATE", Op: "?=", Value: "uv-pep440"},
			{Name: "MODE", Op: ":=", Value: "fast"},
		},
	},
	{
		name: "skill with type and metadata",
		fm: "name: notes\ndescription: Keep notes.\ntype: skill\ncompatibility: advisory text\n" +
			"metadata:\n  requires: os=linux\n",
		wantName: "notes",
		wantDesc: "Keep notes.",
		wantType: "skill",
	},
	{
		name:      "not strict yaml",
		fm:        "name: x\ndescription: runs it: fast\n",
		wantName:  "x",
		wantDesc:  "runs it: fast",
		skillsErr: true, // dag keeps the line reading and records why
	},
}

func TestSameFixturesThroughSkillsAndDag(t *testing.T) {
	for _, fx := range fmFixtures {
		t.Run(fx.name, func(t *testing.T) {
			doc := []byte("---\n" + fx.fm + "---\n" + tasksBody)

			// skills: the shared four keys, or a refusal.
			sk, skErr := skills.ParseFrontmatter(doc)
			if fx.skillsErr {
				if skErr == nil || !strings.Contains(skErr.Error(), "frontmatter") {
					t.Fatalf("skills: want a frontmatter error, got %v", skErr)
				}
			} else if skErr != nil {
				t.Fatalf("skills: %v", skErr)
			}

			// dag: the same four keys plus its own layered on top.
			d, err := dag.Parse(strings.NewReader(string(doc)), "fixture.md")
			if err != nil {
				t.Fatalf("dag.Parse: %v", err)
			}
			if fx.skillsErr != (d.FrontmatterYAMLErr != "") {
				t.Errorf("dag FrontmatterYAMLErr = %q, want recorded iff skills refuses", d.FrontmatterYAMLErr)
			}

			// The two readers agree on every key they both honor.
			if skErr == nil {
				if sk.Name != d.Name || sk.Description != d.Desc || sk.Type != d.Type {
					t.Errorf("readers disagree: skills (%q, %q, %q) dag (%q, %q, %q)",
						sk.Name, sk.Description, sk.Type, d.Name, d.Desc, d.Type)
				}
				if fx.wantName != "" && sk.Name != fx.wantName {
					t.Errorf("name = %q, want %q", sk.Name, fx.wantName)
				}
				if fx.wantDesc != "" && sk.Description != fx.wantDesc {
					t.Errorf("description = %q, want %q", sk.Description, fx.wantDesc)
				}
				if fx.wantType != "" && sk.Type != fx.wantType {
					t.Errorf("type = %q, want %q", sk.Type, fx.wantType)
				}
			}
			// dag's line reading still feeds the degraded block.
			if d.Name != fx.wantName || d.Desc != fx.wantDesc {
				t.Errorf("dag line reading = (%q, %q), want (%q, %q)", d.Name, d.Desc, fx.wantName, fx.wantDesc)
			}
			if d.Default != fx.wantDefault {
				t.Errorf("dag default = %q, want %q", d.Default, fx.wantDefault)
			}
			if !reflect.DeepEqual(d.Includes, fx.wantIncludes) {
				t.Errorf("dag includes = %v, want %v", d.Includes, fx.wantIncludes)
			}
			if !reflect.DeepEqual(d.Vars, fx.wantVars) {
				t.Errorf("dag vars = %+v, want %+v", d.Vars, fx.wantVars)
			}
		})
	}
}

// The shared reader is what both consumers call: skills through
// frontmatter.Parse (refusing a non-YAML block), dag through frontmatter.Read
// (recording the reason). One spelling of `description:` must not parse two
// ways, and dag's `metadata:` nesting must not leak into skills' Meta.
func TestMetadataNestingThroughBoth(t *testing.T) {
	doc := []byte("---\nname: m\ndescription: both read me.\ntype: dag\n" +
		"default: build\nmetadata:\n  default: test\n  tier: local\n---\n" + tasksBody)

	sk, err := skills.ParseFrontmatter(doc)
	if err != nil {
		t.Fatalf("skills: %v", err)
	}
	if sk.Meta["default"] != "test" || sk.Meta["tier"] != "local" {
		t.Errorf("skills Meta = %v", sk.Meta)
	}
	d, err := dag.Parse(strings.NewReader(string(doc)), "m.md")
	if err != nil {
		t.Fatalf("dag.Parse: %v", err)
	}
	if d.Default != "build" {
		t.Errorf("dag default = %q, want the top-level build", d.Default)
	}

	// And the reader each of them calls is this one.
	if fm, err := frontmatter.Parse(doc); err != nil || fm.Name != "m" || fm.Type != "dag" {
		t.Errorf("frontmatter.Parse = %+v %v", fm, err)
	}
}
