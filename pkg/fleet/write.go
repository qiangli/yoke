package fleet

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/assetring"
	"gopkg.in/yaml.v3"
)

// Writes always land in the host-local ring. An entry that comes from the
// embedded baseline, a shared dir, or an org overlay becomes a sparse local
// overlay on first modification. Lower sources are never mutated.

// ringLocal is the only writable ring.
func ringLocal() assetring.Ring { return assetring.RingLocal }

// MaterializeTool creates an empty local overlay if needed and returns its path.
func (c *Catalog) MaterializeTool(name string) (string, error) {
	t, ok := c.Tool(name)
	if !ok {
		return "", fmt.Errorf("fleet: no tool %q", name)
	}
	if err := c.requireMigrated(dirTools, t.Name); err != nil {
		return "", err
	}
	if t.Ring != ringLocal() {
		if err := c.saveChanged(dirTools, t.Name, t, t, nil); err != nil {
			return "", err
		}
	}
	return entryPath(c.nounDir(dirTools), t.Name)
}

// MaterializeModel creates an empty local overlay if needed.
func (c *Catalog) MaterializeModel(name string) (string, error) {
	m, ok := c.Model(name)
	if !ok {
		return "", fmt.Errorf("fleet: no model %q", name)
	}
	if err := c.requireMigrated(dirModels, m.Name); err != nil {
		return "", err
	}
	if m.Ring != ringLocal() {
		if err := c.saveChanged(dirModels, m.Name, m, m, nil); err != nil {
			return "", err
		}
	}
	return entryPath(c.nounDir(dirModels), m.Name)
}

// MaterializeAgent creates an empty local overlay if needed.
func (c *Catalog) MaterializeAgent(name string) (string, error) {
	a, ok := c.Agent(name)
	if !ok {
		return "", fmt.Errorf("fleet: no agent %q", name)
	}
	if err := c.requireMigrated(dirAgents, a.Name); err != nil {
		return "", err
	}
	if a.Ring != ringLocal() {
		if err := c.saveChanged(dirAgents, a.Name, a, a, nil); err != nil {
			return "", err
		}
	}
	fileName := a.Name
	if lowerFile, _, ok := c.lowerEntry(dirAgents, a.Name); ok {
		fileName = lowerFile
	}
	return entryPath(c.nounDir(dirAgents), fileName)
}

func (c *Catalog) requireMigrated(noun, name string) error {
	fileName, _, ok := c.lowerEntry(noun, name)
	if !ok {
		return nil
	}
	path, err := entryPath(c.nounDir(noun), fileName)
	if err != nil {
		return err
	}
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !isOverlay(body) {
		return fmt.Errorf("fleet: %s %q is a full-copy local override; run %s migrate %s before editing it", noun, name, strings.TrimSuffix(noun, "s"), name)
	}
	return nil
}

// SaveTool writes a sparse overlay when a lower tool exists, else a full entry.
func (c *Catalog) SaveTool(t Tool) error {
	if err := validName(t.Name); err != nil {
		return err
	}
	if t.Kind == "" {
		t.Kind = ToolKindCLI
	}
	if body, ok := c.lowerBody(dirTools, t.Name); ok {
		base, err := ParseTool(t.Name, body, nil)
		if err != nil {
			return err
		}
		return c.saveChanged(dirTools, t.Name, base, t, nil)
	}
	data, err := Marshal(t)
	if err != nil {
		return err
	}
	return writeEntry(c.nounDir(dirTools), t.Name, data)
}

// SaveModel writes a sparse overlay when a lower model exists, else a full entry.
func (c *Catalog) SaveModel(m Model) error {
	if err := validName(m.Name); err != nil {
		return err
	}
	if m.Band < 0 || m.Band > MaxBand {
		return fmt.Errorf("fleet: band %d is out of range (1-%d, or 0 for unpegged)", m.Band, MaxBand)
	}
	if body, ok := c.lowerBody(dirModels, m.Name); ok {
		base, err := ParseModel(m.Name, body, nil)
		if err != nil {
			return err
		}
		return c.saveChanged(dirModels, m.Name, base, m, nil)
	}
	data, err := Marshal(m)
	if err != nil {
		return err
	}
	return writeEntry(c.nounDir(dirModels), m.Name, data)
}

// SaveAgent writes a sparse overlay when a lower agent exists, else a full
// agent file envelope.
func (c *Catalog) SaveAgent(a Agent) error {
	if err := validName(a.Name); err != nil {
		return err
	}
	if err := ValidEffort(a.Effort); err != nil {
		return err
	}
	// Store the binding by canonical name, whatever the caller typed. `agents
	// add x --model opus` is a fine thing to write and a terrible thing to
	// persist: `opus` floats, so the saved identity would change meaning under
	// the file. A half that does not resolve is left alone — binding ahead of
	// installing is legitimate, and refusing it here would be a new failure
	// mode for no gain.
	if m, ok := c.Model(a.Model); ok {
		a.Model = m.Name
	}
	if t, ok := c.Tool(a.Tool); ok {
		a.Tool = t.Name
	}
	if body, ok := c.lowerBody(dirAgents, a.Name); ok {
		file, err := ParseAgentFile(a.Name, body, nil)
		if err != nil {
			return err
		}
		for _, base := range file.Agents {
			if base.Name == a.Name {
				return c.saveChanged(dirAgents, a.Name, base, a, nil)
			}
		}
		return fmt.Errorf("fleet: lower agent file %q has no matching identity", a.Name)
	}
	data, err := Marshal(AgentFile{Agents: []Agent{a}})
	if err != nil {
		return err
	}
	return writeEntry(c.nounDir(dirAgents), a.Name, data)
}

// SavePerson writes a human principal into the local store.
func (c *Catalog) SavePerson(p Person) error {
	if err := validName(p.Handle); err != nil {
		return err
	}
	data, err := Marshal(p)
	if err != nil {
		return err
	}
	return writeEntry(c.nounDir(dirPeople), p.Handle, data)
}

// RemoveTool deletes a tool from the local store.
func (c *Catalog) RemoveTool(name string) error {
	return removeEntry(c.nounDir(dirTools), dirTools, name)
}

// RemoveModel deletes a model from the local store.
func (c *Catalog) RemoveModel(name string) error {
	return removeEntry(c.nounDir(dirModels), dirModels, name)
}

// RemoveAgent deletes an agent from the local store.
func (c *Catalog) RemoveAgent(name string) error {
	if fileName, _, ok := c.lowerEntry(dirAgents, name); ok {
		path, err := entryPath(c.nounDir(dirAgents), fileName)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) && fileName != name {
				return removeEntry(c.nounDir(dirAgents), dirAgents, name)
			}
			return fmt.Errorf("fleet: agent %q is not in the local store: %w", name, err)
		}
		if !isOverlay(body) {
			if fileName != name {
				return fmt.Errorf("fleet: full-copy agent file %q contains multiple identities; migrate it before removing one", fileName)
			}
			return removeEntry(c.nounDir(dirAgents), dirAgents, fileName)
		}
		patch, err := decodeMap(body)
		if err != nil {
			return err
		}
		items, ok := patch["agents"].([]any)
		if !ok {
			return fmt.Errorf("fleet: agent overlay %q has no agents", fileName)
		}
		for i, item := range items {
			m, ok := item.(yamlMap)
			if ok && m["name"] == name {
				items = append(items[:i], items[i+1:]...)
				if len(items) == 0 {
					return os.Remove(path)
				}
				patch["agents"] = items
				data, err := yaml.Marshal(patch)
				if err != nil {
					return err
				}
				return writeEntry(c.nounDir(dirAgents), fileName, data)
			}
		}
		return fmt.Errorf("fleet: agent %q has no local override", name)
	}
	return removeEntry(c.nounDir(dirAgents), dirAgents, name)
}

// RemovePerson deletes a person from the local store.
func (c *Catalog) RemovePerson(name string) error {
	return removeEntry(c.nounDir(dirPeople), dirPeople, name)
}

// claimName reports an error when name (or any of its aliases) already
// belongs to a different entry of this kind. Aliasing one entry many times
// is free; one name meaning two things is not.
func (c *Catalog) claimName(kind, canonical string, aliases []string, force bool) error {
	if force {
		return nil
	}
	lookup := func(n string) (string, bool) {
		switch kind {
		case KindAgent:
			if a, ok := c.Agent(n); ok {
				return a.Name, true
			}
		case KindTool:
			if t, ok := c.Tool(n); ok {
				return t.Name, true
			}
		case KindModel:
			if m, ok := c.Model(n); ok {
				return m.Name, true
			}
		case KindPerson:
			if p, ok := c.Person(n); ok {
				return p.Handle, true
			}
		case KindCommand:
			if r, ok := c.Command(n); ok {
				return r.Name, true
			}
		case KindApp:
			if a, ok := c.App(n); ok {
				return a.Name, true
			}
		}
		return "", false
	}
	for _, n := range names(canonical, aliases) {
		if holder, ok := lookup(n); ok && holder != canonical {
			return fmt.Errorf("fleet: %s name %q already belongs to %q (use --force to take it)", kind, n, holder)
		}
	}
	return nil
}

// readSource loads an asset document from a path, or from r when the path
// is "-".
func readSource(path string, r io.Reader) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(r)
	}
	return os.ReadFile(path)
}

// looksLikePath distinguishes `agents add ./codex.yaml` from
// `agents add 007 --tool codex`. A bare identifier is a name.
func looksLikePath(s string) bool {
	return s == "-" || strings.ContainsAny(s, `/\`) || strings.HasSuffix(s, ext) ||
		strings.HasSuffix(s, ".yml")
}

// mergeAliases applies --add-alias / --rm-alias to an alias list.
func mergeAliases(cur, add, rm []string) []string {
	drop := map[string]bool{}
	for _, a := range rm {
		drop[a] = true
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(cur)+len(add))
	for _, a := range append(append([]string{}, cur...), add...) {
		if a == "" || drop[a] || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

// ValidEffort accepts an empty (undeclared) effort or one lowercase word such
// as low, medium, high, xhigh or max. Which words a tool accepts is the tool's
// business — the CLI rejects an unknown level itself — but a value that could
// smuggle a second flag or config key into an argv never reaches one.
func ValidEffort(e string) error {
	if e == "" {
		return nil
	}
	for _, r := range e {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return fmt.Errorf("fleet: effort %q must be one lowercase word (e.g. low, medium, high)", e)
		}
	}
	return nil
}
