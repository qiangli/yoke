package fleet

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/qiangli/yoke/pkg/assetring"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// RecordLifecycle is shared by every registry kind. Lifecycle changes go
// through retire/unretire rather than the editable record schema.
// The name distinguishes it from Agent's existing task-owned Lifecycle field.
type RecordLifecycle struct {
	Retired *Retirement `yaml:"retired,omitempty" json:"retired,omitempty"`
}

type Retirement struct {
	At         string `yaml:"at" json:"at"`
	Reason     string `yaml:"reason,omitempty" json:"reason,omitempty"`
	ReplacedBy string `yaml:"replaced_by,omitempty" json:"replaced_by,omitempty"`
}

func (l RecordLifecycle) RegistryLifecycle() RecordLifecycle { return l }
func (l RecordLifecycle) IsRetired() bool                    { return l.Retired != nil }

// CheckRetired refuses new work without changing historical resolution.
func (l RecordLifecycle) CheckRetired(kind, name string) error {
	if l.Retired == nil {
		return nil
	}
	hint := ""
	if l.Retired.ReplacedBy != "" {
		hint = "; use " + l.Retired.ReplacedBy
	}
	return fmt.Errorf("%s %q retired%s", kind, name, hint)
}

func lifecycleOf[T interface{ RegistryLifecycle() RecordLifecycle }](get func(*Catalog, string) (T, bool)) func(*Catalog, string) (RecordLifecycle, bool) {
	return func(c *Catalog, name string) (RecordLifecycle, bool) {
		v, ok := get(c, name)
		return v.RegistryLifecycle(), ok
	}
}

// MatchRetirement is the common list rule, independent of ring and visibility.
func MatchRetirement(v interface{ IsRetired() bool }, retiredOnly bool) bool {
	return v.IsRetired() == retiredOnly
}

// Retire preserves the definition and writes only its lifecycle in a lower-ring
// overlay. Existing local edits (including full-copy overrides) are preserved.
func (c *Catalog) Retire(kind, name, reason, replacedBy string) error {
	return c.setRetirement(kind, name, &Retirement{At: time.Now().UTC().Format(time.RFC3339), Reason: reason, ReplacedBy: replacedBy})
}
func (c *Catalog) Unretire(kind, name string) error { return c.setRetirement(kind, name, nil) }

// recordMap also handles bundled documents, without a separate noun writer.
func recordMap(doc yamlMap, plural, name string) (yamlMap, bool) {
	if items, ok := doc[plural].([]any); ok {
		for _, item := range items {
			if m, ok := item.(yamlMap); ok && m["name"] == name {
				return m, true
			}
		}
		return nil, false
	}
	for _, key := range []string{"name", "handle"} {
		if v, ok := doc[key]; ok {
			return doc, v == name
		}
	}
	return doc, true // file basename supplies the identity
}

func (c *Catalog) setRetirement(kind, name string, retired *Retirement) error {
	spec, ok := kindByName(kind)
	if !ok {
		return fmt.Errorf("fleet: unknown kind %q", kind)
	}
	canonical, ok := spec.Lookup(c, name)
	if !ok {
		return fmt.Errorf("fleet: no %s %q", kind, name)
	}
	name = canonical
	sources := c.sources(spec.Plural)
	for i := len(sources) - 1; i >= 0; i-- {
		source := sources[i]
		files, err := source.Names()
		if err != nil {
			return err
		}
		for _, file := range files {
			body, ok := source.Body(file)
			if !ok {
				continue
			}
			doc, err := decodeMap(body)
			if err != nil {
				continue
			}
			record, ok := recordMap(doc, spec.Plural, name)
			if !ok {
				continue
			}
			// A nameless single record is identified by its file, not the first file.
			if _, hasName := doc["name"]; !hasName {
				if _, hasHandle := doc["handle"]; !hasHandle {
					if _, bundled := doc[spec.Plural]; !bundled && file != name {
						continue
					}
				}
			}
			if source.Ring() == assetring.RingLocal {
				path, err := entryPath(c.nounDir(spec.Plural), file)
				if err != nil {
					return err
				}
				body, err = os.ReadFile(path)
				if err != nil {
					return err
				}
				doc, err = decodeMap(body)
				if err != nil {
					return err
				}
				record, ok = recordMap(doc, spec.Plural, name)
				if !ok { // another identity in the same local bundle has the overlay
					record = yamlMap{"name": name}
					items, _ := doc[spec.Plural].([]any)
					doc[spec.Plural] = append(items, record)
				}
			} else {
				_, bundled := doc[spec.Plural].([]any)
				doc = yamlMap{"overlay": true}
				record = doc
				if bundled || spec.Envelope != "" {
					record = yamlMap{"name": name}
					doc[spec.Plural] = []any{record}
				}
			}
			if retired == nil && !isOverlay(body) && source.Ring() == assetring.RingLocal {
				delete(record, "retired")
			} else {
				record["retired"] = nil
				if retired != nil {
					// Explicit empty values clear metadata inherited from a retired seed.
					record["retired"] = yamlMap{"at": retired.At, "reason": retired.Reason, "replaced_by": retired.ReplacedBy}
				}
			}
			data, err := yaml.Marshal(doc)
			if err != nil {
				return err
			}
			return writeEntry(c.nounDir(spec.Plural), file, data)
		}
	}
	return fmt.Errorf("fleet: no stored %s %q", kind, name)
}

// NewRetireCmd and NewUnretireCmd are also used by noun roots outside fleet.
func NewRetireCmd(kind string, opts ...Option) *cobra.Command {
	return newRetirementCmd(kind, false, opts)
}
func NewUnretireCmd(kind string, opts ...Option) *cobra.Command {
	return newRetirementCmd(kind, true, opts)
}
func newRetirementCmd(kind string, undo bool, opts []Option) *cobra.Command {
	verb := "retire"
	if undo {
		verb = "unretire"
	}
	var reason, replacement string
	cmd := &cobra.Command{Use: verb + " NAME", Short: verb + " a registry entry", Args: cobra.ExactArgs(1), SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := New(opts...)
			var err error
			if undo {
				err = cat.Unretire(kind, args[0])
			} else {
				err = cat.Retire(kind, args[0], reason, replacement)
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%sd %s %s\n", verb, kind, args[0])
			return nil
		}}
	if !undo {
		cmd.Flags().StringVar(&reason, "reason", "", "reason for retiring")
		cmd.Flags().StringVar(&replacement, "replaced-by", "", "replacement entry")
	}
	return cmd
}

func (c *Catalog) WarnUnretired(w io.Writer, kind, name string) {
	if k, ok := kindByName(kind); ok && k.Lifecycle != nil {
		if life, found := k.Lifecycle(c, name); found && !life.IsRetired() {
			fmt.Fprintf(w, "warning: %s %q is not retired; retire it before removal\n", kind, name)
		}
	}
}

// AgentAvailability reports a retired dependency without retiring the agent.
func (c *Catalog) AgentAvailability(a Agent) error {
	if t, ok := c.Tool(a.Tool); ok {
		if err := t.CheckRetired(KindTool, t.Name); err != nil {
			return fmt.Errorf("unavailable: dependency retired: %w", err)
		}
	}
	if m, ok := c.Model(a.Model); ok {
		if err := m.CheckRetired(KindModel, m.Name); err != nil {
			return fmt.Errorf("unavailable: dependency retired: %w", err)
		}
	}
	return nil
}

func agentResolution(r agentRow) string {
	if r.Unavailable != "" {
		return r.Unavailable
	}
	return yesNo(r.Resolves)
}

func checkNewAgentRetirement(cmd *cobra.Command, cat *Catalog, a Agent) error {
	allow, _ := cmd.Flags().GetBool("allow-retired")
	if allow {
		return nil
	}
	if err := a.CheckRetired(KindAgent, a.Name); err != nil {
		return err
	}
	return cat.AgentAvailability(a)
}
