package skills

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Ring, the ring constants, and Ring.String live in source.go as
// aliases of pkg/assetring.

// Skill is one catalog entry: a standard Agent Skills folder (SKILL.md
// + optional reference.md), with the P0 machine-checkable extras parsed
// out of the spec-legal frontmatter.
type Skill struct {
	Name          string
	Description   string
	Ring          Ring
	Dir           string            // source dir ("" for embedded)
	Meta          map[string]string // frontmatter `metadata` string map
	Compatibility string            // frontmatter free text (advisory only)
	Requires      *Requires         // parsed Meta["requires"]; nil = none
	RequiresErr   string            // non-empty when Meta["requires"] did not parse
	HasDhnt       bool              // a skill.dhnt sits beside SKILL.md
	Dhnt          *DhntInfo         // parsed canonical face (nil when absent)
	HasTasks      bool              // a tasks.md (dag task file) sits beside SKILL.md
}

// frontmatter is the permissive superset we read; unknown fields are
// ignored — bashy consumes the world's skills, it doesn't lint them.
type frontmatter struct {
	Name          string         `yaml:"name"`
	Description   string         `yaml:"description"`
	Compatibility string         `yaml:"compatibility"`
	Metadata      map[string]any `yaml:"metadata"`
}

// ParseFrontmatter reads the YAML frontmatter block of a SKILL.md.
// A missing or malformed block is an error the caller degrades on (the
// skill stays listed with its directory name) — it never hides a skill.
func ParseFrontmatter(b []byte) (Skill, error) {
	s := string(b)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return Skill{}, fmt.Errorf("skills: no frontmatter block")
	}
	_, rest, _ := strings.Cut(s, "\n")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return Skill{}, fmt.Errorf("skills: unterminated frontmatter block")
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		return Skill{}, fmt.Errorf("skills: frontmatter: %w", err)
	}
	sk := Skill{
		Name:          fm.Name,
		Description:   strings.TrimSpace(fm.Description),
		Compatibility: strings.TrimSpace(fm.Compatibility),
		Meta:          map[string]string{},
	}
	for k, v := range fm.Metadata {
		sk.Meta[k] = fmt.Sprintf("%v", v)
	}
	if req, ok := sk.Meta["requires"]; ok && req != "" {
		parsed, err := ParseRequires(req)
		if err != nil {
			sk.RequiresErr = err.Error()
		} else {
			sk.Requires = &parsed
		}
	}
	return sk, nil
}
