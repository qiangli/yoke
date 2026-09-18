package fleet

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/assetring"
)

// Writes always land in the host-local ring. An entry that comes from the
// embedded baseline, a shared dir, or an org overlay is copied into the
// local store on first modification, so the operator's edit shadows the
// original instead of mutating a source they do not own.

// ringLocal is the only writable ring.
func ringLocal() assetring.Ring { return assetring.RingLocal }

// MaterializeTool copies a tool into the local store if it is not already
// there, and returns the path an editor should open.
func (c *Catalog) MaterializeTool(name string) (string, error) {
	t, ok := c.Tool(name)
	if !ok {
		return "", fmt.Errorf("fleet: no tool %q", name)
	}
	if t.Ring != ringLocal() {
		if err := c.SaveTool(t); err != nil {
			return "", err
		}
	}
	return entryPath(c.nounDir(dirTools), t.Name)
}

// MaterializeModel copies a model into the local store if needed.
func (c *Catalog) MaterializeModel(name string) (string, error) {
	m, ok := c.Model(name)
	if !ok {
		return "", fmt.Errorf("fleet: no model %q", name)
	}
	if m.Ring != ringLocal() {
		if err := c.SaveModel(m); err != nil {
			return "", err
		}
	}
	return entryPath(c.nounDir(dirModels), m.Name)
}

// MaterializeAgent copies an agent into the local store if needed.
func (c *Catalog) MaterializeAgent(name string) (string, error) {
	a, ok := c.Agent(name)
	if !ok {
		return "", fmt.Errorf("fleet: no agent %q", name)
	}
	if a.Ring != ringLocal() {
		if err := c.SaveAgent(a); err != nil {
			return "", err
		}
	}
	return entryPath(c.nounDir(dirAgents), a.Name)
}

// SaveTool writes a tool into the local store as canonical YAML.
func (c *Catalog) SaveTool(t Tool) error {
	if err := validName(t.Name); err != nil {
		return err
	}
	if t.Kind == "" {
		t.Kind = ToolKindCLI
	}
	data, err := Marshal(t)
	if err != nil {
		return err
	}
	return writeEntry(c.nounDir(dirTools), t.Name, data)
}

// SaveModel writes a model into the local store as canonical YAML.
func (c *Catalog) SaveModel(m Model) error {
	if err := validName(m.Name); err != nil {
		return err
	}
	if m.Band < 0 || m.Band > MaxBand {
		return fmt.Errorf("fleet: band %d is out of range (1-%d, or 0 for unpegged)", m.Band, MaxBand)
	}
	data, err := Marshal(m)
	if err != nil {
		return err
	}
	return writeEntry(c.nounDir(dirModels), m.Name, data)
}

// SaveAgent writes an agent into the local store, wrapped in the asset
// envelope so the file is a valid catalog entry on either side.
func (c *Catalog) SaveAgent(a Agent) error {
	if err := validName(a.Name); err != nil {
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
