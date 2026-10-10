package fleet

import (
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/qiangli/yoke/pkg/assetring"
	"gopkg.in/yaml.v3"
)

// Only documents carrying overlay: true are merged. Older local documents
// remain full replacements until an operator reviews their migration.
type overlaySource struct {
	assetring.Source
	lower []assetring.Source
	noun  string
}

func (s overlaySource) Body(name string) ([]byte, bool) {
	body, ok := s.Source.Body(name)
	if !ok || !isOverlay(body) {
		return body, ok
	}
	for i := len(s.lower) - 1; i >= 0; i-- {
		if base, found := s.lower[i].Body(name); found {
			merged, err := mergeOverlay(base, body, s.noun)
			if err != nil {
				return []byte("[invalid fleet overlay: " + err.Error() + "]"), true
			}
			return merged, true
		}
	}
	return []byte("[fleet overlay has no lower entry]"), true
}

func isOverlay(body []byte) bool {
	var head struct {
		Overlay bool `yaml:"overlay"`
	}
	return yaml.Unmarshal(body, &head) == nil && head.Overlay
}

type yamlMap = map[string]any

func decodeMap(body []byte) (yamlMap, error) {
	var m yamlMap
	if err := yaml.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("expected YAML mapping")
	}
	return m, nil
}

func mergeMap(base, patch yamlMap) yamlMap {
	for k, v := range patch {
		if k == "overlay" {
			continue
		}
		if child, ok := v.(yamlMap); ok {
			if old, ok := base[k].(yamlMap); ok {
				base[k] = mergeMap(old, child)
			} else {
				base[k] = child
			}
		} else {
			base[k] = v
		}
	}
	return base
}

func mergeOverlay(lower, overlay []byte, noun string) ([]byte, error) {
	base, err := decodeMap(lower)
	if err != nil {
		return nil, err
	}
	patch, err := decodeMap(overlay)
	if err != nil {
		return nil, err
	}
	if noun == dirAgents {
		// Agent files may contain several identities. Overlay just the named
		// identity while leaving its siblings in the lower file intact.
		bs, bok := base["agents"].([]any)
		ps, pok := patch["agents"].([]any)
		if !pok || len(ps) == 0 {
			return nil, fmt.Errorf("agent overlay needs named agents and a lower agent file")
		}
		if !bok {
			p, ok := ps[0].(yamlMap)
			if !ok {
				return nil, fmt.Errorf("agent overlay item is not a mapping")
			}
			name, _ := p["name"].(string)
			if _, present := base["name"]; !present {
				base["name"] = name
			}
			bs = []any{base}
			base = yamlMap{"agents": bs}
		}
		for _, item := range ps {
			p, ok := item.(yamlMap)
			if !ok {
				return nil, fmt.Errorf("agent overlay item is not a mapping")
			}
			name, _ := p["name"].(string)
			found := false
			for i, current := range bs {
				b, ok := current.(yamlMap)
				if ok && b["name"] == name {
					bs[i] = mergeMap(b, p)
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("agent %q is missing from lower file", name)
			}
		}
		delete(patch, "agents")
	}
	return yaml.Marshal(mergeMap(base, patch))
}

// diffMap records changed leaves. A missing field in the updated canonical
// struct is an explicit null, including zero, false, empty string and nil.
func diffMap(before, after yamlMap) yamlMap {
	out := yamlMap{}
	for k, old := range before {
		newValue, ok := after[k]
		if !ok {
			out[k] = nil
			continue
		}
		if a, ok := old.(yamlMap); ok {
			if b, ok := newValue.(yamlMap); ok {
				if d := diffMap(a, b); len(d) != 0 {
					out[k] = d
				}
				continue
			}
		}
		if !reflect.DeepEqual(old, newValue) {
			out[k] = newValue
		}
	}
	for k, v := range after {
		if _, ok := before[k]; !ok {
			out[k] = v
		}
	}
	return out
}

func setOverlayPath(patch, after yamlMap, path string) {
	parts := strings.Split(path, ".")
	// A list is a single YAML field. Replacing that field is explicit and
	// keeps index/name edits deterministic without inventing list tombstones.
	p, a := patch, after
	for i, key := range parts {
		value, exists := a[key]
		if i == len(parts)-1 || !exists {
			if exists {
				p[key] = value
			} else {
				p[key] = nil
			}
			return
		}
		child, ok := value.(yamlMap)
		if !ok {
			p[key] = value
			return
		}
		a = child
		next, ok := p[key].(yamlMap)
		if !ok {
			next = yamlMap{}
			p[key] = next
		}
		p = next
	}
}

func (c *Catalog) lowerEntry(noun, name string) (string, []byte, bool) {
	sources := c.sources(noun)
	for i := len(sources) - 1; i >= 0; i-- {
		if sources[i].Ring() == ringLocal() {
			continue
		}
		if noun == dirAgents {
			names, err := sources[i].Names()
			if err != nil {
				continue
			}
			for _, file := range names {
				b, ok := sources[i].Body(file)
				if !ok {
					continue
				}
				parsed, err := ParseAgentFile(file, b, nil)
				if err != nil {
					continue
				}
				for _, a := range parsed.Agents {
					if a.Name == name {
						return file, b, true
					}
				}
			}
		} else if b, ok := sources[i].Body(name); ok {
			return name, b, true
		}
	}
	return "", nil, false
}

func (c *Catalog) lowerBody(noun, name string) ([]byte, bool) {
	_, body, ok := c.lowerEntry(noun, name)
	return body, ok
}

func (c *Catalog) saveChanged(noun, name string, before, after any, paths []string) error {
	if err := validName(name); err != nil {
		return err
	}
	switch v := after.(type) {
	case Model:
		if v.Band < 0 || v.Band > MaxBand {
			return fmt.Errorf("fleet: band %d is out of range (1-%d, or 0 for unpegged)", v.Band, MaxBand)
		}
	case Agent:
		if err := ValidEffort(v.Effort); err != nil {
			return err
		}
	}
	fileName, lower, found := c.lowerEntry(noun, name)
	if !found {
		if spec, ok := kindByDir(noun); ok && spec.Record != nil {
			return spec.Record.saveValue(c, after)
		}
	}
	path, err := entryPath(c.nounDir(noun), fileName)
	if err != nil {
		return err
	}
	var existing yamlMap
	if body, err := os.ReadFile(path); err == nil {
		if !isOverlay(body) {
			return fmt.Errorf("fleet: %s %q is a full-copy local override; run %s migrate %s before setting it", noun, name, strings.TrimSuffix(noun, "s"), name)
		}
		existing, err = decodeMap(body)
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if existing == nil {
		existing = yamlMap{"overlay": true}
	}
	if noun == dirAgents {
		before = AgentFile{Agents: []Agent{before.(Agent)}}
		after = AgentFile{Agents: []Agent{after.(Agent)}}
	}
	oldBytes, err := Marshal(before)
	if err != nil {
		return err
	}
	newBytes, err := Marshal(after)
	if err != nil {
		return err
	}
	oldMap, err := decodeMap(oldBytes)
	if err != nil {
		return err
	}
	newMap, err := decodeMap(newBytes)
	if err != nil {
		return err
	}
	if noun == dirAgents {
		oldAgent := oldMap["agents"].([]any)[0].(yamlMap)
		newAgent := newMap["agents"].([]any)[0].(yamlMap)
		patch := diffMap(oldAgent, newAgent)
		for _, p := range paths {
			setOverlayPath(patch, newAgent, p)
		}
		patch["name"] = name
		if agents, ok := existing["agents"].([]any); ok {
			found := false
			for i, item := range agents {
				m, ok := item.(yamlMap)
				if ok && m["name"] == name {
					agents[i] = mergeMap(m, patch)
					found = true
					break
				}
			}
			if !found {
				agents = append(agents, patch)
			}
			existing["agents"] = agents
		} else {
			existing["agents"] = []any{patch}
		}
	} else {
		patch := diffMap(oldMap, newMap)
		for _, p := range paths {
			setOverlayPath(patch, newMap, p)
		}
		patch["name"] = name
		mergeMap(existing, patch)
	}
	data, err := yaml.Marshal(existing)
	if err != nil {
		return err
	}
	// Validate the merged document against the typed parser before writing.
	merged, err := mergeOverlay(lower, data, noun)
	if err != nil {
		return err
	}
	switch noun {
	case dirTools:
		_, err = ParseTool(name, merged, nil)
	case dirModels:
		_, err = ParseModel(name, merged, nil)
	case dirAgents:
		_, err = ParseAgentFile(name, merged, nil)
	}
	if err != nil {
		return err
	}
	return writeEntry(c.nounDir(noun), fileName, data)
}
