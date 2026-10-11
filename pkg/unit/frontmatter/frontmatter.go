// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package frontmatter is the one reader of `---`-fenced frontmatter blocks
// (SKILL.md, dag.md, OKF pages). It splits the fences, reads the keys every
// bashy surface honours — name, description, type and the metadata map — and
// keeps every other key as a yaml.Node so callers with their own keys (dag's
// default/include/vars) layer them on top of the shared four.
//
// A block that is not strict YAML yields no keys; the reason is recorded in
// Block.YAMLErr the way dag's Document.FrontmatterYAMLErr records it, because
// the permissive line reading a format may keep (dag's legacy header) still
// ran and stays authoritative. LineKV is that line reading, shared.
package frontmatter

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Block is one document's frontmatter, read the one way.
type Block struct {
	Name        string // `name` as written
	Description string // `description`, trimmed
	Type        string // `type` — the OKF page type (`dag`)

	// Metadata is the `metadata:` mapping's scalar values, keys as written
	// (skills stringifies them into Skill.Meta). nil when `metadata:` is
	// absent or not a mapping.
	Metadata map[string]string

	// Top indexes every top-level key — unknown keys included — lower-cased
	// (both readings are case-insensitive about keys), for callers layering
	// their own keys. nil when the block is not a key: value mapping.
	Top map[string]*yaml.Node
	// Meta is the `metadata:` mapping's keys, lower-cased. nil when
	// `metadata:` is absent or not a mapping.
	Meta map[string]*yaml.Node

	// YAMLErr is why the block is not strict YAML ("" when it is). Empty
	// block and absent block are not errors.
	YAMLErr string
}

// Split cuts a document into its frontmatter block (the text between the
// `---` fences, fences excluded) and the rest (starting at the closing fence
// line), so "---\n" + block' + rest reassembles the document with only the
// block replaced.
func Split(doc []byte) (block, rest string, err error) {
	s := string(doc)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return "", "", fmt.Errorf("frontmatter: no --- block")
	}
	_, after, _ := strings.Cut(s, "\n")
	end := strings.Index(after, "\n---")
	if end < 0 {
		return "", "", fmt.Errorf("frontmatter: unterminated --- block")
	}
	return after[:end], after[end+1:], nil
}

// Read parses one frontmatter block (Split's block: fences excluded).
func Read(block string) Block {
	var b Block
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(block), &root); err != nil {
		b.YAMLErr = strings.TrimPrefix(err.Error(), "yaml: ")
		return b
	}
	if len(root.Content) == 0 {
		return b // empty block
	}
	top := keyedMapping(root.Content[0])
	if top == nil {
		b.YAMLErr = "frontmatter is not a key: value mapping"
		return b
	}
	b.Top = top
	b.Name = Scalar(top["name"])
	b.Description = strings.TrimSpace(Scalar(top["description"]))
	b.Type = Scalar(top["type"])
	if mn := top["metadata"]; mn != nil && mn.Kind == yaml.MappingNode {
		b.Meta = keyedMapping(mn)
		b.Metadata = make(map[string]string, len(mn.Content)/2)
		for i := 0; i+1 < len(mn.Content); i += 2 {
			b.Metadata[mn.Content[i].Value] = Scalar(mn.Content[i+1])
		}
	}
	return b
}

// Parse splits and reads a document's frontmatter. The error is only about
// the fences (no block, unterminated block); a block that is not strict YAML
// is not an error here — it is Block.YAMLErr, and the caller decides (dag
// records it for --check; skills refuses the file).
func Parse(doc []byte) (Block, error) {
	block, _, err := Split(doc)
	if err != nil {
		return Block{}, err
	}
	return Read(block), nil
}

// Scalar is a node's scalar value, "" for anything else (absent, a
// sequence, a nested mapping).
func Scalar(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// LineKV reads one `key: value` line permissively — the legacy line reading
// a format keeps for headers that predate strict YAML (dag's). The key is
// lower-cased; the value is trimmed and unquoted.
func LineKV(line string) (key, value string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i <= 0 {
		return "", "", false
	}
	return strings.ToLower(strings.TrimSpace(line[:i])),
		strings.Trim(strings.TrimSpace(line[i+1:]), `"'`), true
}

// keyedMapping indexes a mapping node by lower-cased key; nil for anything
// else.
func keyedMapping(n *yaml.Node) map[string]*yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	m := make(map[string]*yaml.Node, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		m[strings.ToLower(n.Content[i].Value)] = n.Content[i+1]
	}
	return m
}
