package fleet

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/assetring"
)

// Canonical asset keys are `name:` and `kind:`. Tool assets authored
// before that was settled spell them `kit:` and `type:`. Both are read;
// only the canonical pair is written.
//
// This is the Day-1 half of a two-step migration: accept both shapes, emit
// one. Day-2 drops the legacy aliases once every catalog has been rewritten
// and a release cycle has passed. Wire identifiers are never repurposed —
// `kit` and `type` will be removed, not redefined.

// toolWire is the permissive superset read off disk.
type toolWire struct {
	Tool `yaml:",inline"`

	// Legacy spellings. A parsed value folds into the canonical field only
	// when the canonical one is absent.
	Kit  string `yaml:"kit,omitempty"`
	Type string `yaml:"type,omitempty"`
}

// ParseTool reads a tool asset. name is the fallback identity when the
// document carries none (the file's own basename).
func ParseTool(name string, body []byte, src assetring.Source) (Tool, error) {
	var w toolWire
	if err := yaml.Unmarshal(body, &w); err != nil {
		return Tool{}, fmt.Errorf("fleet: tool %q: %w", name, err)
	}
	t := w.Tool
	if t.Name == "" {
		t.Name = w.Kit
	}
	if t.Name == "" {
		t.Name = name
	}
	if t.Kind == "" {
		t.Kind = w.Type
	}
	if src != nil {
		t.Ring = src.Ring()
	}
	if t.CLI.Binary == "" && t.IsCLI() {
		t.CLI.Binary = t.Name
	}
	return t, nil
}

// ParseModel reads a model asset.
func ParseModel(name string, body []byte, src assetring.Source) (Model, error) {
	var m Model
	if err := yaml.Unmarshal(body, &m); err != nil {
		return Model{}, fmt.Errorf("fleet: model %q: %w", name, err)
	}
	if m.Name == "" {
		m.Name = name
	}
	if m.Source == "" {
		m.Source = ModelSourceCloud
	}
	if src != nil {
		m.Ring = src.Ring()
	}
	return m, nil
}

// ParseAgentFile reads an agent asset, which may declare several agents.
// A bare `Agent` document (no `agents:` list) is also accepted, so a
// hand-written single-agent file needs no envelope.
func ParseAgentFile(name string, body []byte, src assetring.Source) (AgentFile, error) {
	var f AgentFile
	if err := yaml.Unmarshal(body, &f); err != nil {
		return AgentFile{}, fmt.Errorf("fleet: agent %q: %w", name, err)
	}
	if len(f.Agents) == 0 {
		var a Agent
		if err := yaml.Unmarshal(body, &a); err != nil || (a.Tool == "" && a.Name == "") {
			return AgentFile{}, fmt.Errorf("fleet: agent %q: no agents declared", name)
		}
		f.Agents = []Agent{a}
	}
	for i := range f.Agents {
		if f.Agents[i].Name == "" {
			f.Agents[i].Name = name
		}
		if src != nil {
			f.Agents[i].Ring = src.Ring()
		}
	}
	return f, nil
}

// ParsePerson reads a person asset.
func ParsePerson(name string, body []byte, src assetring.Source) (Person, error) {
	var p Person
	if err := yaml.Unmarshal(body, &p); err != nil {
		return Person{}, fmt.Errorf("fleet: person %q: %w", name, err)
	}
	if p.Handle == "" {
		p.Handle = name
	}
	if p.Source == "" {
		p.Source = "local"
	}
	if src != nil {
		p.Ring = src.Ring()
	}
	return p, nil
}

// Marshal renders an entry as canonical YAML — the exact bytes an asset
// registry would serve as its Content blob. Emitting is always canonical:
// a legacy `kit:`/`type:` document rewrites to `name:`/`kind:` the first
// time it is saved.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
