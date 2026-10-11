// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/unit/frontmatter"
)

// A dag.md frontmatter block is meant to be read by more than bashy: an Agent
// Skill loader indexes `name`/`description`, an OKF (Open Knowledge Format)
// reader wants `type`. Both read the block as YAML, so a dag file whose header
// is strict YAML is a first-class citizen for them too.
//
// Parse keeps its line reading (every file written before this parses exactly
// as it did) and then, when the block is strict YAML, lets YAML settle the
// file-level keys: a folded `description: >`, quoted values with colons, a
// trailing `# comment`, and the spec-clean nesting of the dag-only keys under
// `metadata:` (skill validators accept only a handful of top-level keys):
//
//	---
//	name: uv
//	description: Build and test uv. Use when asked to build, lint or test it.
//	type: dag
//	metadata:
//	  default: fmt-check
//	  vars:
//	    - TEST_CRATE ?= uv-pep440
//	---
//
// A top-level key wins over the same key under `metadata:`.

// applyYAMLFrontmatter layers the shared reader's strict-YAML read
// (pkg/unit/frontmatter — name, description, type, metadata and every key
// as a node) over the line reading's result. dag's own keys (default,
// include, vars) are picked from that read here. On a parse error it
// records why in doc.FrontmatterYAMLErr and leaves the line reading's
// result alone.
func applyYAMLFrontmatter(doc *Document, block string) {
	doc.hasFrontmatter = true
	fm := frontmatter.Read(block)
	if fm.YAMLErr != "" {
		doc.FrontmatterYAMLErr = fm.YAMLErr
		return
	}
	if fm.Top == nil {
		return // empty block
	}
	pick := func(keys ...string) *yaml.Node {
		for _, m := range []map[string]*yaml.Node{fm.Top, fm.Meta} {
			for _, k := range keys {
				if n, ok := m[k]; ok {
					return n
				}
			}
		}
		return nil
	}

	doc.Name = fm.Name
	doc.Desc = fm.Description
	doc.Type = fm.Type
	doc.Default = frontmatter.Scalar(pick("default", "default_goal"))

	doc.Includes = nil
	if n := pick("include", "includes"); n != nil {
		switch n.Kind {
		case yaml.SequenceNode:
			for _, it := range n.Content {
				if v := strings.TrimSpace(frontmatter.Scalar(it)); v != "" {
					doc.Includes = append(doc.Includes, v)
				}
			}
		case yaml.ScalarNode:
			doc.Includes = splitList(n.Value)
		}
	}

	// A top-level `vars:` block of `NAME ?= value` lines is a plain multi-line
	// scalar to YAML — its line breaks folded away — so the line reading, which
	// saw the lines, stays authoritative for that one shape.
	topVars := fm.Top["vars"]
	if topVars == nil {
		topVars = fm.Top["variables"]
	}
	n := pick("vars", "variables")
	if n == nil || (n == topVars && n.Kind == yaml.ScalarNode && !strings.Contains(n.Value, "\n")) {
		if n == nil {
			doc.Vars = nil
		}
		return
	}
	doc.Vars = nil
	add := func(line string) {
		if name, op, val, ok := parseVarLine(line); ok {
			doc.Vars = append(doc.Vars, DocVar{Name: name, Op: op, Value: val})
		}
	}
	switch n.Kind {
	case yaml.SequenceNode:
		for _, it := range n.Content {
			add(frontmatter.Scalar(it))
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			doc.Vars = append(doc.Vars, DocVar{Name: n.Content[i].Value, Op: "=", Value: frontmatter.Scalar(n.Content[i+1])})
		}
	case yaml.ScalarNode:
		if strings.Contains(n.Value, "\n") { // `vars: |` literal block
			for _, ln := range strings.Split(n.Value, "\n") {
				add(ln)
			}
		} else { // inline `vars: A=1 B=2`
			for _, f := range strings.Fields(n.Value) {
				add(f)
			}
		}
	}
}

// skillNameRE is the Agent Skills `name` shape: lowercase letters, digits and
// single hyphens, at most 64 characters.
var skillNameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// frontmatterWarnings lints the file header for the readers beyond bashy —
// Agent Skill loaders and OKF. Advisory only: bashy runs the file either way.
func frontmatterWarnings(doc *Document) []string {
	if !doc.hasFrontmatter {
		return []string{"no frontmatter: add name, description and type: dag so agent-skill and OKF readers can index this file"}
	}
	var w []string
	if doc.FrontmatterYAMLErr != "" {
		w = append(w, "frontmatter is not strict YAML ("+doc.FrontmatterYAMLErr+"); skill and OKF readers will reject it")
	}
	if doc.Name == "" {
		w = append(w, "frontmatter has no name")
	} else if len(doc.Name) > 64 || !skillNameRE.MatchString(doc.Name) {
		w = append(w, "frontmatter name "+doc.Name+" is not a skill name (lowercase letters, digits, hyphens; at most 64)")
	}
	if doc.Desc == "" {
		w = append(w, "frontmatter has no description (say what the file builds and when to use it)")
	}
	if doc.Type == "" {
		w = append(w, "frontmatter has no type (OKF requires it; use type: dag)")
	}
	return w
}
