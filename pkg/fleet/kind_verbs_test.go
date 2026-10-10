package fleet

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/assetring"
)

// gadget is a kind declared only through the kind table: its show / add / set
// come from the generic builders, with no per-noun command code.
type gadget struct {
	Name    string         `yaml:"name" json:"name" doc:"gadget id"`
	Color   string         `yaml:"color,omitempty" json:"color,omitempty" doc:"paint"`
	Size    int            `yaml:"size,omitempty" json:"size,omitempty" doc:"how big"`
	Aliases []string       `yaml:"aliases,omitempty" json:"aliases,omitempty" doc:"other names"`
	Ring    assetring.Ring `yaml:"-" json:"-"`
}

type gadgetStore struct {
	held  map[string]gadget
	saved []string
}

func registerGadgetKind(t *testing.T) *gadgetStore {
	t.Helper()
	st := &gadgetStore{held: map[string]gadget{}}
	find := func(name string) (gadget, bool) {
		for _, g := range st.held {
			if g.Name == name || contains(g.Aliases, name) {
				return g, true
			}
		}
		return gadget{}, false
	}
	rec := newRecordSpec(typedRecord[gadget]{
		Get:     func(_ *Catalog, n string) (gadget, bool) { return find(n) },
		Name:    func(g *gadget) *string { return &g.Name },
		Ring:    func(g gadget) assetring.Ring { return g.Ring },
		Aliases: func(g *gadget) *[]string { return &g.Aliases },
		Parse: func(fallback string, body []byte) ([]gadget, error) {
			var g gadget
			if err := yaml.Unmarshal(body, &g); err != nil {
				return nil, err
			}
			if g.Name == "" {
				g.Name = fallback
			}
			return []gadget{g}, nil
		},
		Save: func(_ *Catalog, g gadget) error {
			g.Ring = ringLocal()
			st.held[g.Name] = g
			st.saved = append(st.saved, g.Name)
			return nil
		},
		ShowShort: "Print a gadget",
		AddDoc:    verbDoc{Use: "add (<name> --color C | <file>|-)", Short: "Add a gadget"},
		SetDoc:    verbDoc{Short: "Modify a gadget"},
		NameFlag:  "store under this name",
		AliasDoc:  aliasDoc{Add: "another name", AddAlias: "add a name", RmAlias: "drop a name"},
		Flags: []kindFlag{
			strFlag("color", "color", "paint it", "repaint it", func(g *gadget) *string { return &g.Color }),
			intFlag("size", "size", "how big", "", func(g *gadget) *int { return &g.Size }),
		},
		AddCheck: func(g gadget) error {
			if g.Color == "" {
				return os.ErrInvalid
			}
			return nil
		},
		Saved: func(cmd *cobra.Command, _ *Catalog, g gadget, set bool) error {
			verb := "added"
			if set {
				verb = "set"
			}
			cmd.Printf("%s %s %s\n", verb, g.Name, g.Color)
			return nil
		},
	})
	registerKind(kindSpec{
		Name: "gadget", Plural: "gadgets", DirEnv: "BASHY_GADGETS_DIR", PathEnv: "BASHY_GADGETS_PATH",
		Type: reflect.TypeOf(gadget{}),
		Lookup: func(_ *Catalog, n string) (string, bool) {
			g, ok := find(n)
			return g.Name, ok
		},
		Names:  func(*Catalog) []string { return nil },
		Record: rec,
	})
	t.Cleanup(func() { unregisterKind("gadget") })
	return st
}

func gadgetRoot(opts []Option) *cobra.Command {
	root := &cobra.Command{Use: "gadget", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newShow("gadget", opts), newAdd("gadget", opts), newSet("gadget", opts))
	return root
}

func TestRegisteredKindGetsShowAddSetFromTheTable(t *testing.T) {
	isolatedFleetRoot(t)
	st := registerGadgetKind(t)
	opts := []Option{WithRoot(t.TempDir())}
	run := func(args ...string) (string, error) { return runCmd(t, gadgetRoot(opts), args...) }

	if out, err := run("add", "g1", "--color", "red", "--size", "3", "--alias", "gee", "--set", "size=4"); err != nil || out != "added g1 red\n" {
		t.Fatalf("add from flags: %q, %v", out, err)
	}
	if got := st.held["g1"]; got.Color != "red" || got.Size != 4 || strings.Join(got.Aliases, ",") != "gee" {
		t.Errorf("stored %+v: typed flags, --alias and --set must all land", got)
	}

	if out, err := run("show", "gee"); err != nil || !strings.Contains(out, "color: red") {
		t.Errorf("show by alias: %q, %v", out, err)
	}
	if out, err := run("show", "g1", "--json"); err != nil || !strings.Contains(out, `"color": "red"`) {
		t.Errorf("show --json: %q, %v", out, err)
	}
	if out, err := run("show", "g1", "--field", "color"); err != nil || out != "red\n" {
		t.Errorf("show --field: %q, %v", out, err)
	}
	if _, err := run("show", "nope"); err == nil || err.Error() != `fleet: no gadget "nope"` {
		t.Errorf("show of a missing entry = %v", err)
	}
	if _, err := run("show", "g1", "--json", "--yaml"); err == nil {
		t.Error("--json with --yaml must be refused")
	}

	// A bare name with no record flag is not a file; the kind's own check
	// runs on the flag-built record, and --set lifts it.
	if _, err := run("add", "g2"); err == nil {
		t.Error("AddCheck must refuse a flag-built record missing its color")
	}
	if _, err := run("add", "g2", "--set", "color=blue"); err != nil {
		t.Errorf("--set satisfies the kind: %v", err)
	}

	// Names are unique within the kind; --force takes one.
	if _, err := run("add", "g3", "--color", "x", "--alias", "gee"); err == nil || !strings.Contains(err.Error(), `gadget name "gee" already belongs to "g1"`) {
		t.Errorf("alias collision = %v", err)
	}
	if _, err := run("add", "g3", "--color", "x", "--alias", "gee", "--force"); err != nil {
		t.Errorf("--force: %v", err)
	}

	out, err := run("set", "g1", "--color", "green", "--add-alias", "gee2", "--rm-alias", "gee")
	if err != nil || out != "set g1 green\n" {
		t.Fatalf("set: %q, %v", out, err)
	}
	if got := st.held["g1"]; got.Color != "green" || got.Size != 4 || strings.Join(got.Aliases, ",") != "gee2" {
		t.Errorf("stored %+v: set must change only what was given", got)
	}
	if _, err := run("set", "g1", "--size", "9"); err == nil {
		t.Error("a flag a verb does not carry (size is add-only here) must fail loudly")
	}
	if _, err := run("set", "nope", "--color", "x"); err == nil || err.Error() != `fleet: no gadget "nope"` {
		t.Errorf("set of a missing entry = %v", err)
	}
}

func TestKindAddImportsAFileOrStdinAndNeverDropsRecordFlags(t *testing.T) {
	isolatedFleetRoot(t)
	st := registerGadgetKind(t)
	opts := []Option{WithRoot(t.TempDir())}
	file := filepath.Join(t.TempDir(), "g9.yaml")
	if err := os.WriteFile(file, []byte("color: teal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, gadgetRoot(opts), "add", file); err != nil || st.held["g9"].Color != "teal" {
		t.Fatalf("import: %+v, %v", st.held, err)
	}
	if _, err := runCmd(t, gadgetRoot(opts), "add", file, "--name", "g10"); err != nil || st.held["g10"].Color != "teal" {
		t.Errorf("--name must store an imported document under another name: %+v", st.held)
	}
	// A record flag next to a file would be silently ignored by an import; it
	// selects the flag-built path instead, which then refuses the path-like name.
	if _, err := runCmd(t, gadgetRoot(opts), "add", file, "--size", "5"); err == nil {
		t.Error("a record flag beside a file must not be dropped silently")
	}
}

func TestToolModelAgentDeclareTheirVerbsOnTheKindTable(t *testing.T) {
	for _, kind := range []string{KindTool, KindModel, KindAgent} {
		spec, ok := kindByName(kind)
		if !ok || spec.Record == nil {
			t.Fatalf("%s: no record spec on the kind table", kind)
		}
		for _, verb := range []*cobra.Command{newShow(kind, nil), newAdd(kind, nil), newSet(kind, nil)} {
			if verb.Flags().Lookup("force") == nil && verb.Name() != "show" {
				t.Errorf("%s %s: no --force", kind, verb.Name())
			}
		}
	}
}

func TestAgentImportClaimsTheNickLikeEveryOtherWrite(t *testing.T) {
	isolatedFleetRoot(t)
	opts := []Option{WithRoot(t.TempDir())}
	cat := New(opts...)
	if err := cat.SaveTool(Tool{Name: "tt", Kind: ToolKindCLI}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(Model{Name: "mm", Provider: "openai", Kind: ModelKindAPI}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(Agent{Name: "first", Tool: "tt", Model: "mm"}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "second.yaml")
	if err := os.WriteFile(file, []byte("agents:\n  - name: second\n    nick: first\n    tool: tt\n    model: mm\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runCmd(t, NewAgentsCmd(opts...), "add", file)
	if err == nil || !strings.Contains(err.Error(), `agent name "first" already belongs to "first"`) {
		t.Errorf("an imported nick naming another agent must be refused, got %v", err)
	}
	if _, err := runCmd(t, NewAgentsCmd(opts...), "add", file, "--force"); err != nil {
		t.Errorf("--force: %v", err)
	}
}
