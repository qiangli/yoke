// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package release

import (
	"fmt"
	"strings"
	"text/template"
)

// Fields is the template context for name templates. It is deliberately
// small: the subset goreleaser configs in this tree actually use. An unknown
// field is an ERROR, never an empty string — a name template that silently
// renders "ycode--.tar.gz" would produce an asset the fleet-upgrade contract
// cannot find, and nothing would report it.
//
// A field that is KNOWN but UNSET is the same hazard and is refused the same
// way: rendering the checksum template (which has no target bound, so Os and
// Arch are empty) through a per-target name_template is precisely how one gets
// "ycode--". See vars.
type Fields struct {
	ProjectName string
	Version     string
	Tag         string
	Commit      string
	ShortCommit string
	Os          string
	Arch        string
	Arm         string
	Binary      string
	Env         map[string]string
}

// vars renders Fields as the template context. Only fields that actually have
// a value are present, so text/template's missingkey=error turns BOTH an
// unknown field and an unset-but-known one into an error.
//
// A struct context cannot do this: missingkey applies to map index only, so
// {{ .Os }} against a struct with an empty Os renders "" and the run ships a
// misnamed asset. The map is what makes the guarantee above true.
func (f Fields) vars() map[string]any {
	m := make(map[string]any, 10)
	for k, v := range map[string]string{
		"ProjectName": f.ProjectName,
		"Version":     f.Version,
		"Tag":         f.Tag,
		"Commit":      f.Commit,
		"ShortCommit": f.ShortCommit,
		"Os":          f.Os,
		"Arch":        f.Arch,
		"Arm":         f.Arm,
		"Binary":      f.Binary,
	} {
		if v != "" {
			m[k] = v
		}
	}
	if len(f.Env) > 0 {
		m["Env"] = f.Env
	}
	return m
}

// Render expands a name template. Only the fields above resolve, and only
// where they carry a value.
func Render(tmpl string, f Fields) (string, error) {
	t, err := template.New("name").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("template %q: %w", tmpl, err)
	}
	var sb strings.Builder
	if err := t.Execute(&sb, f.vars()); err != nil {
		return "", fmt.Errorf("template %q: %w", tmpl, err)
	}
	out := sb.String()
	if strings.Contains(out, "<no value>") {
		return "", fmt.Errorf("template %q: unresolved field", tmpl)
	}
	return out, nil
}
