package skills

// Writing SKILL.md frontmatter back. ParseFrontmatter (skill.go) reads the
// block permissively — bashy consumes the world's skills — and that is the
// constraint on writing it: a `skill set` that re-marshalled the block from
// the struct it parsed would drop every key the struct does not know and
// re-flow every one it does. So the block is edited as a yaml.Node tree —
// unknown keys, their order, quoting and comments survive — and the body
// after the closing `---` is copied byte for byte.

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FrontmatterEdit is one set of changes to a SKILL.md frontmatter block. A
// nil Description and an absent key mean "leave it alone", so a caller can
// change one field without restating the rest.
type FrontmatterEdit struct {
	Description *string           // replace the description
	SetMeta     map[string]string // metadata keys to set (requires, check-*, step-*, …)
	UnsetMeta   []string          // metadata keys to drop
}

// splitFrontmatter cuts a SKILL.md into its YAML block and the rest, with the
// same cut ParseFrontmatter makes. rest starts at the closing `---` line, so
// "---\n" + block' + rest is the file with only the block replaced.
func splitFrontmatter(b []byte) (block, rest string, err error) {
	s := string(b)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return "", "", fmt.Errorf("skills: no frontmatter block")
	}
	_, after, _ := strings.Cut(s, "\n")
	end := strings.Index(after, "\n---")
	if end < 0 {
		return "", "", fmt.Errorf("skills: unterminated frontmatter block")
	}
	return after[:end], after[end+1:], nil
}

// WriteFrontmatter applies edit to the frontmatter block of skillMD and
// returns the whole file. Every key the edit does not name is preserved as
// written; the body is untouched. An empty metadata map left behind by an
// unset is dropped rather than emitted as `metadata: {}`.
func WriteFrontmatter(skillMD []byte, edit FrontmatterEdit) ([]byte, error) {
	block, rest, err := splitFrontmatter(skillMD)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(block), &doc); err != nil {
		return nil, fmt.Errorf("skills: frontmatter: %w", err)
	}
	root := documentMapping(&doc)
	if root == nil {
		return nil, fmt.Errorf("skills: frontmatter is not a mapping")
	}
	if edit.Description != nil {
		setKey(root, "description", strScalar(*edit.Description))
	}
	if len(edit.SetMeta) > 0 || len(edit.UnsetMeta) > 0 {
		meta := mappingChild(root, "metadata")
		if meta == nil && len(edit.SetMeta) > 0 {
			meta = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setKey(root, "metadata", meta)
		}
		if meta != nil {
			keys := make([]string, 0, len(edit.SetMeta))
			for k := range edit.SetMeta {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				setKey(meta, k, strScalar(edit.SetMeta[k]))
			}
			for _, k := range edit.UnsetMeta {
				unsetKey(meta, k)
			}
			if len(meta.Content) == 0 {
				unsetKey(root, "metadata")
			}
		}
	}
	var buf bytes.Buffer
	buf.WriteString("---\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	buf.WriteString(rest)
	return buf.Bytes(), nil
}

// documentMapping returns the top-level mapping of a decoded block, turning
// an empty block into an empty mapping so a bare `---\n---` can be filled.
func documentMapping(doc *yaml.Node) *yaml.Node {
	if doc.Kind == 0 {
		m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		*doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{m}}
		return m
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode {
		return doc.Content[0]
	}
	return nil
}

// strScalar is a string value the encoder quotes when its plain spelling
// would read back as something else (a bare `true`, `1.0`, `null`).
func strScalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

// mappingChild returns the mapping value under key, or nil.
func mappingChild(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.MappingNode {
			return m.Content[i+1]
		}
	}
	return nil
}

// setKey replaces the value under key in place, keeping its position (and
// the key's comments), or appends the pair when the key is new.
func setKey(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			// A value replaced wholesale would lose the comments hanging off
			// the old one; carry them across.
			val.HeadComment, val.LineComment, val.FootComment =
				m.Content[i+1].HeadComment, m.Content[i+1].LineComment, m.Content[i+1].FootComment
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
}

// unsetKey drops key and its value; a missing key is not an error.
func unsetKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// replaceBody swaps everything after the closing `---` line for body,
// leaving the frontmatter block byte for byte. The closing line itself is
// kept; body follows it on the next line.
func replaceBody(skillMD []byte, body string) ([]byte, error) {
	block, rest, err := splitFrontmatter(skillMD)
	if err != nil {
		return nil, err
	}
	// rest is "---<eol>...": keep the closing line, drop what follows it.
	closing, _, _ := strings.Cut(rest, "\n")
	return []byte("---\n" + block + "\n" + closing + "\n" + body), nil
}

// newSkillMD renders a minimal SKILL.md: name, description, the metadata
// given, and body verbatim after the closing line. Nothing else — the
// mechanism's own frontmatter format is the whole template.
func newSkillMD(name, description string, meta map[string]string, body string) ([]byte, error) {
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setKey(root, "name", strScalar(name))
	setKey(root, "description", strScalar(description))
	if len(meta) > 0 {
		m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		keys := make([]string, 0, len(meta))
		for k := range meta {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			setKey(m, k, strScalar(meta[k]))
		}
		setKey(root, "metadata", m)
	}
	doc := yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	var buf bytes.Buffer
	buf.WriteString("---\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	buf.WriteString("---\n")
	buf.WriteString(body)
	return buf.Bytes(), nil
}
