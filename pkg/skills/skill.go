package skills

import (
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/unit/frontmatter"
)

// Ring, the ring constants, and Ring.String live in source.go as
// aliases of pkg/assetring.

// Skill is one catalog entry: a standard Agent Skills folder (SKILL.md
// + optional reference.md), with the P0 machine-checkable extras parsed
// out of the spec-legal frontmatter.
type Skill struct {
	Name          string
	Description   string
	Type          string // frontmatter `type` — the OKF page type (dag skills carry `dag`)
	Ring          Ring
	Dir           string            // source dir ("" for embedded)
	Meta          map[string]string // frontmatter `metadata` string map
	Compatibility string            // frontmatter free text (advisory only)
	Requires      *Requires         // parsed Meta["requires"]; nil = none
	RequiresErr   string            // non-empty when Meta["requires"] did not parse
	HasDhnt       bool              // a skill.dhnt sits beside SKILL.md
	Dhnt          *DhntInfo         // parsed canonical face (nil when absent)
	HasTasks      bool              // a tasks face (tasks.md, metadata.tasks pointer, or inline ## Tasks) exists
}

// ParseFrontmatter reads the YAML frontmatter block of a SKILL.md through
// the one shared reader (pkg/unit/frontmatter), so a skill and the dag file
// beside it parse the same block the same way — name, description, type and
// metadata; every other key is kept for the writer, not consumed here. A
// missing block or a block that is not strict YAML is an error the caller
// degrades on (the skill stays listed with its directory name) — it never
// hides a skill.
func ParseFrontmatter(b []byte) (Skill, error) {
	fm, err := frontmatter.Parse(b)
	if err != nil {
		return Skill{}, err
	}
	if fm.YAMLErr != "" {
		return Skill{}, fmt.Errorf("skills: frontmatter: %s", fm.YAMLErr)
	}
	sk := Skill{
		Name:          fm.Name,
		Description:   fm.Description,
		Type:          fm.Type,
		Compatibility: strings.TrimSpace(frontmatter.Scalar(fm.Top["compatibility"])),
		Meta:          fm.Metadata,
	}
	if sk.Meta == nil {
		sk.Meta = map[string]string{}
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
