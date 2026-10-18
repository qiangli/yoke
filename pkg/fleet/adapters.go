package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/qiangli/yoke/pkg/assetring"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const KindAdapter = "adapter"
const dirAdapters = "adapters"

// AdapterMapping is an argv fragment, or (for JSON only) a declared lens row.
// nil is unsupported; an explicit empty array means native output already fits.
type AdapterMapping []string

func (m AdapterMapping) MarshalYAML() (any, error) {
	if m == nil {
		return nil, nil
	}
	return []string(m), nil
}

func (m *AdapterMapping) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode && strings.HasPrefix(n.Value, "lens: cmd:") {
		*m = []string{n.Value}
		return nil
	}
	var a []string
	if err := n.Decode(&a); err != nil {
		return fmt.Errorf("mapping must be an argv array or lens: cmd: row")
	}
	*m = a
	return nil
}

type Adapter struct {
	Explain      AdapterMapping `yaml:"explain" json:"explain"`
	Name         string         `yaml:"name" json:"name"`
	Kind         string         `yaml:"kind" json:"kind"`
	Match        []string       `yaml:"match" json:"match"`
	Summary      string         `yaml:"summary,omitempty" json:"summary,omitempty"`
	JSON         AdapterMapping `yaml:"json" json:"json"`
	Fields       AdapterMapping `yaml:"fields" json:"fields"`
	Null         AdapterMapping `yaml:"null" json:"null"`
	DryRun       AdapterMapping `yaml:"dry_run" json:"dry_run"`
	NoInput      AdapterMapping `yaml:"no_input" json:"no_input"`
	Depth        AdapterMapping `yaml:"depth" json:"depth"`
	Since        AdapterMapping `yaml:"since" json:"since"`
	Listing      AdapterMapping `yaml:"listing" json:"listing"`
	PreviewLevel string         `yaml:"preview_level" json:"preview_level"`
	Effects      []string       `yaml:"effects" json:"effects"`
	Ring         assetring.Ring `yaml:"-" json:"ring"`
}

func (a Adapter) Validate() error {
	if err := validName(a.Name); err != nil {
		return err
	}
	if a.Kind != KindAdapter || a.Name == "" || len(a.Match) == 0 {
		return fmt.Errorf("adapter requires kind: adapter, name and match")
	}
	for _, s := range a.Match {
		if s == "" {
			return fmt.Errorf("adapter match contains an empty token")
		}
	}
	switch a.PreviewLevel {
	case "unsupported", "explain-only", "local-preview", "provider-validated-preview":
	default:
		return fmt.Errorf("adapter requires a valid preview_level")
	}
	if a.DryRun != nil && a.PreviewLevel != "local-preview" && a.PreviewLevel != "provider-validated-preview" {
		return fmt.Errorf("dry_run requires an executing preview level")
	}

	for _, s := range a.JSON {
		if strings.HasPrefix(s, "lens:") && (len(a.JSON) != 1 || !strings.HasPrefix(s, "lens: cmd:") || len(s) == len("lens: cmd:")) {
			return fmt.Errorf("invalid JSON lens mapping")
		}
	}
	for _, m := range []AdapterMapping{a.Fields, a.Null, a.DryRun, a.NoInput, a.Depth, a.Since, a.Listing, a.Explain} {
		for _, s := range m {
			if strings.HasPrefix(s, "lens:") {
				return fmt.Errorf("lens is only a JSON mapping")
			}
		}
	}
	for _, s := range a.NoInput {
		for _, bad := range []string{"-y", "--yes", "-f", "--force"} {
			if s == bad || strings.HasPrefix(s, bad+"=") {
				return fmt.Errorf("no_input must not grant consent or force")
			}
		}
	}
	return nil
}

func ParseAdapter(name string, body []byte, src assetring.Source) (Adapter, error) {
	// Normalize only declaration keys. Argv values must remain byte-identical.
	var n yaml.Node
	if err := yaml.Unmarshal(body, &n); err != nil {
		return Adapter{}, fmt.Errorf("invalid adapter YAML")
	}
	if len(n.Content) != 1 || n.Content[0].Kind != yaml.MappingNode {
		return Adapter{}, fmt.Errorf("adapter must be a mapping")
	}
	keys := n.Content[0].Content
	seen := map[string]bool{}
	for i := 0; i < len(keys); i += 2 {
		k := strings.ReplaceAll(keys[i].Value, "-", "_")
		if seen[k] {
			return Adapter{}, fmt.Errorf("duplicate adapter field")
		}
		seen[k] = true
		keys[i].Value = k
		keys[i].Tag = "!!str"
	}
	b, err := yaml.Marshal(&n)
	if err != nil {
		return Adapter{}, err
	}
	var a Adapter
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err = d.Decode(&a); err != nil {
		return Adapter{}, fmt.Errorf("invalid adapter fields or mappings")
	}
	if a.Name == "" {
		a.Name = name
	}
	if src != nil {
		a.Ring = src.Ring()
	}
	return a, a.Validate()
}

func (c *Catalog) Adapters() ([]Adapter, []error) {
	var errs []error
	cat := assetring.Catalog[Adapter]{Sources: c.sources(dirAdapters), Parse: func(n string, b []byte, s assetring.Source) Adapter {
		errs = append(errs, sourceWarnings(s)...)
		a, e := ParseAdapter(n, b, s)
		if e != nil {
			errs = append(errs, fmt.Errorf("adapter %q: %w", n, e))
		}
		return a
	}}
	rows, e := cat.Rows()
	if e != nil {
		errs = append(errs, e)
	}
	out := make([]Adapter, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Entry)
	}
	return out, errs
}
func (c *Catalog) Adapter(name string) (Adapter, bool) {
	rows, _ := c.Adapters()
	for _, a := range rows {
		if a.Name == name {
			return a, true
		}
	}
	return Adapter{}, false
}
func (c *Catalog) SaveAdapter(a Adapter) error {
	if e := a.Validate(); e != nil {
		return e
	}
	b, e := Marshal(a)
	if e != nil {
		return e
	}
	return writeEntry(c.nounDir(dirAdapters), a.Name, b)
}
func (c *Catalog) RemoveAdapter(n string) error {
	return removeEntry(c.nounDir(dirAdapters), dirAdapters, n)
}
func (c *Catalog) MaterializeAdapter(n string) (string, error) {
	a, ok := c.Adapter(n)
	if !ok {
		return "", fmt.Errorf("unknown adapter")
	}
	if a.Ring != ringLocal() {
		if e := c.SaveAdapter(a); e != nil {
			return "", e
		}
	}
	return entryPath(c.nounDir(dirAdapters), n)
}
func init() {
	registerKind(kindSpec{
		Name: KindAdapter, Plural: dirAdapters, DirEnv: "BASHY_ADAPTERS_DIR", PathEnv: "BASHY_ADAPTERS_PATH",
		Type: reflect.TypeOf(Adapter{}), Lookup: holder((*Catalog).Adapter, func(a Adapter) string { return a.Name }), Names: listed((*Catalog).Adapters, func(a Adapter) string { return a.Name }), Seeded: true, Mechanism: true,
		Record: newRecordSpec(typedRecord[Adapter]{Get: (*Catalog).Adapter, Name: func(a *Adapter) *string { return &a.Name }, Ring: func(a Adapter) assetring.Ring { return a.Ring }, Parse: func(n string, b []byte) ([]Adapter, error) { a, e := ParseAdapter(n, b, nil); return []Adapter{a}, e }, Save: (*Catalog).SaveAdapter, FileFlag: true, AddDoc: verbDoc{Use: "add FILE|-", Short: "Import a declarative adapter"}}),
	})
}

// NewAdaptersCmd uses the fleet kind-core write surface and its claim guard.
func NewAdaptersCmd(opts ...Option) *cobra.Command {
	var asJSON bool
	list := &cobra.Command{Use: "list", Short: "List invocation adapters", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		rows, errs := New(opts...).Adapters()
		if len(errs) > 0 {
			return errs[0]
		}
		if asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(rows)
		}
		for _, a := range rows {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", a.Name, strings.Join(a.Match, " "), a.PreviewLevel)
		}
		return nil
	}}
	list.Flags().BoolVar(&asJSON, "json", false, "JSON records")
	return newRoot(KindAdapter, "Declarative foreign-command mappings", list, newShow(KindAdapter, opts), newSchema(KindAdapter), newAdd(KindAdapter, opts), newSet(KindAdapter, opts), newRm(KindAdapter, opts, (*Catalog).RemoveAdapter), newEdit(KindAdapter, opts, (*Catalog).MaterializeAdapter))
}
