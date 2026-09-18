package fleet

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/assetring"
	"github.com/qiangli/yoke/pkg/ref"
)

// RegisterRefs installs this package's resolvers into the uniform-ref
// registry: agent, tool, model, skill, and host, answered by the same
// Catalog the fleet CLI builds from these options. pkg/ref is stdlib-only,
// so the leaf invariant (leaf_test.go) holds.
func RegisterRefs(g *ref.Registry, opts ...Option) {
	c := New(opts...)
	g.Register(ref.Agent, ref.ResolverFunc(c.refAgent))
	g.Register(ref.Tool, ref.ResolverFunc(c.refTool))
	g.Register(ref.Model, ref.ResolverFunc(c.refModel))
	g.Register(ref.Skill, ref.ResolverFunc(c.refSkill))
	g.Register(ref.Host, ref.ResolverFunc(c.refHost))
}

// storeErr extracts a ring read failure from a catalog error list. Per-entry
// parse errors are not read failures — a broken entry still lists under its
// name — but a ring that could not be read at all must surface as its own
// error: reporting "not found" then would be a conclusion reached through
// the absence of evidence.
func storeErr(errs []error) error {
	for _, err := range errs {
		if _, ok := err.(parseErr); !ok {
			return err
		}
	}
	return nil
}

func (c *Catalog) refAgent(id string) (ref.Node, error) {
	a, ok := c.Agent(id)
	if !ok {
		_, errs := c.Agents()
		if err := storeErr(errs); err != nil {
			return ref.Node{}, err
		}
		return ref.Node{}, fmt.Errorf("fleet: agent %q: %w", id, ref.ErrNotFound)
	}
	n := ref.NewNode(ref.Agent, a.Name)
	n.Title = firstNonEmpty(a.Display, a.Description, a.MatrixKey())
	n.Status = a.Ring.String()
	n.Where = a.Ring.String()
	n.Open = "bashy agent show " + a.Name
	return n, nil
}

func (c *Catalog) refTool(id string) (ref.Node, error) {
	t, ok := c.Tool(id)
	if !ok {
		_, errs := c.Tools(true)
		if err := storeErr(errs); err != nil {
			return ref.Node{}, err
		}
		return ref.Node{}, fmt.Errorf("fleet: tool %q: %w", id, ref.ErrNotFound)
	}
	n := ref.NewNode(ref.Tool, t.Name)
	n.Title = firstNonEmpty(t.Display, t.Name)
	n.Status = t.Ring.String()
	n.Where = t.Ring.String()
	n.Open = "bashy tool show " + t.Name
	return n, nil
}

func (c *Catalog) refModel(id string) (ref.Node, error) {
	m, ok := c.Model(id)
	if !ok {
		_, errs := c.Models()
		if err := storeErr(errs); err != nil {
			return ref.Node{}, err
		}
		return ref.Node{}, fmt.Errorf("fleet: model %q: %w", id, ref.ErrNotFound)
	}
	n := ref.NewNode(ref.Model, m.Name)
	n.Title = firstNonEmpty(m.Display, m.Name)
	// A family alias is a floating pointer, not a record; the node must say
	// which record it landed on, or "opus" reads as a page of its own.
	for _, d := range m.Derived {
		if d == id && id != m.Name {
			n.Title += " (newest in family " + m.Family + "; resolved from alias " + id + ")"
			break
		}
	}
	n.Status = m.Ring.String()
	n.Where = m.Ring.String()
	n.Open = "bashy model show " + m.Name
	return n, nil
}

func (c *Catalog) refHost(id string) (ref.Node, error) {
	h, ok := c.Host(id)
	if !ok {
		_, errs := c.Hosts()
		if err := storeErr(errs); err != nil {
			return ref.Node{}, err
		}
		return ref.Node{}, fmt.Errorf("fleet: host %q: %w", id, ref.ErrNotFound)
	}
	n := ref.NewNode(ref.Host, h.Name)
	n.Title = firstNonEmpty(h.Display, h.Target())
	n.Status = h.Ring.String()
	n.Where = h.Ring.String()
	n.Open = "bashy whois " + h.Name
	return n, nil
}

// refSkill answers for the skills ring. This package does not otherwise read
// skills (pkg/skills owns that ring, and the leaf invariant keeps it out of
// the import graph), so the resolver walks the same folder layout the sync
// writer produces: <dir>/<name>/SKILL.md, local store above the cloud cache.
func (c *Catalog) refSkill(id string) (ref.Node, error) {
	if err := validName(id); err != nil {
		// An id the store cannot spell is definitionally absent from it.
		return ref.Node{}, fmt.Errorf("fleet: skill %q: %w", id, ref.ErrNotFound)
	}
	for _, sr := range c.skillRings() {
		marker := filepath.Join(sr.dir, id, "SKILL.md")
		body, err := os.ReadFile(marker)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return ref.Node{}, err // the entry is there and could not be read
		}
		n := ref.NewNode(ref.Skill, id)
		n.Title = skillDescription(body)
		n.Status = sr.ring.String()
		n.Where = marker
		n.Open = "bashy skill show " + id
		return n, nil
	}
	return ref.Node{}, fmt.Errorf("fleet: skill %q: %w", id, ref.ErrNotFound)
}

// skillRing is one place skills live, with the ring it answers as.
type skillRing struct {
	dir  string
	ring assetring.Ring
}

// skillRings lists the skill stores in resolution order (local shadows
// cloud). The local rung mirrors pkg/skills' store ladder, expressed with
// this catalog's root semantics: an explicit WithRoot pins <root>/skills and
// ignores the ambient overrides, exactly as nounDir does for the yaml nouns.
func (c *Catalog) skillRings() []skillRing {
	local := filepath.Join(c.cfg.root, dirSkills)
	if !c.cfg.rootSet {
		if d := strings.TrimSpace(os.Getenv("BASHY_SKILLS_DIR")); d != "" {
			local = d
		} else if home := strings.TrimSpace(os.Getenv("BASHY_HOME")); home != "" {
			local = filepath.Join(home, "skills")
		}
	}
	out := []skillRing{{local, assetring.RingLocal}}
	if !c.cfg.noCloud {
		out = append(out, skillRing{filepath.Join(CloudCacheRoot(c.cfg.root), dirSkills), assetring.RingCloud})
	}
	return out
}

// skillDescription reads the one-line description out of a SKILL.md
// frontmatter block. A skill without one still resolves; the title is empty.
func skillDescription(body []byte) string {
	rest, ok := strings.CutPrefix(string(body), "---\n")
	if !ok {
		rest, ok = strings.CutPrefix(string(body), "---\r\n")
	}
	if !ok {
		return ""
	}
	fm, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		return ""
	}
	var meta struct {
		Description string `yaml:"description"`
	}
	if yaml.Unmarshal([]byte(fm), &meta) != nil {
		return ""
	}
	return strings.TrimSpace(meta.Description)
}
