package fleet

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/qiangli/yoke/pkg/assetring"
)

// This file is the generic half of the kind table's write surface: `show`,
// `add` and `set` are built once, from a kind's recordSpec, instead of once
// per noun. A kind declares what truly differs — its typed flags, how it is
// read, parsed and saved, what it echoes after a write — and nothing else.

// aliasPath is the record path every kind's alias list lives at.
const aliasPath = "aliases"

type flagRole int

const (
	// roleRecord flags write a field of the record. Giving one makes `add`
	// build the record from flags instead of importing a file.
	roleRecord flagRole = iota
	// roleFinish flags run on every record `add` saves — from flags or from
	// a file, given or not — after every other edit.
	roleFinish
)

// kindFlag is one typed flag of a kind's `add` / `set`. Add and Set are the
// help text on each verb; empty means the verb does not carry the flag.
type kindFlag struct {
	Name string
	Path string // record path the flag writes: what `set` records in an overlay
	Add  string
	Set  string
	Role flagRole
	bind func(fs *pflag.FlagSet, help string) func(rec any) error
}

func (f kindFlag) finish() kindFlag {
	f.Role = roleFinish
	return f
}

func recFlag[T, V any](name, path, add, set string, define func(fs *pflag.FlagSet, p *V, name, help string), apply func(*T, V) error) kindFlag {
	return kindFlag{Name: name, Path: path, Add: add, Set: set,
		bind: func(fs *pflag.FlagSet, help string) func(any) error {
			var v V
			define(fs, &v, name, help)
			return func(rec any) error { return apply(rec.(*T), v) }
		}}
}

func defineString(fs *pflag.FlagSet, p *string, n, h string) { fs.StringVar(p, n, "", h) }

func strFlag[T any](name, path, add, set string, field func(*T) *string) kindFlag {
	return recFlag(name, path, add, set, defineString,
		func(r *T, v string) error { *field(r) = v; return nil })
}

func intFlag[T any](name, path, add, set string, field func(*T) *int) kindFlag {
	return recFlag(name, path, add, set,
		func(fs *pflag.FlagSet, p *int, n, h string) { fs.IntVar(p, n, 0, h) },
		func(r *T, v int) error { *field(r) = v; return nil })
}

func int64Flag[T any](name, path, add, set string, field func(*T) *int64) kindFlag {
	return recFlag(name, path, add, set,
		func(fs *pflag.FlagSet, p *int64, n, h string) { fs.Int64Var(p, n, 0, h) },
		func(r *T, v int64) error { *field(r) = v; return nil })
}

func floatFlag[T any](name, path, add, set string, field func(*T) *float64) kindFlag {
	return recFlag(name, path, add, set,
		func(fs *pflag.FlagSet, p *float64, n, h string) { fs.Float64Var(p, n, 0, h) },
		func(r *T, v float64) error { *field(r) = v; return nil })
}

func boolFlag[T any](name, path, add, set string, field func(*T) *bool) kindFlag {
	return recFlag(name, path, add, set,
		func(fs *pflag.FlagSet, p *bool, n, h string) { fs.BoolVar(p, n, false, h) },
		func(r *T, v bool) error { *field(r) = v; return nil })
}

func listFlag[T any](name, path, add, set string, apply func(*T, []string) error) kindFlag {
	return recFlag(name, path, add, set,
		func(fs *pflag.FlagSet, p *[]string, n, h string) { fs.StringArrayVar(p, n, nil, h) },
		apply)
}

// verbDoc is a verb's help. Use defaults per verb when empty.
type verbDoc struct {
	Use, Short, Long, Example string
}

// aliasDoc is the help of the alias flags. Add is the `add --alias` text;
// empty means `add` has no such flag (the kind's aliases come from a file or
// --set). AddAlias and RmAlias are the `set` flags.
type aliasDoc struct {
	Add, AddAlias, RmAlias string
}

// typedRecord is a kind's write surface over its entry type T. newRecordSpec
// erases T so the generic verbs and the kind table never name it.
type typedRecord[T any] struct {
	Get     func(*Catalog, string) (T, bool)
	Name    func(*T) *string
	Ring    func(T) assetring.Ring
	Aliases func(*T) *[]string // nil: the kind has no alias list
	// Claims lists the names a record must own besides its canonical one.
	// Default: its aliases.
	Claims func(T) []string
	// Parse reads a document into one or more records (an agent file holds
	// several). fallback names a record whose document carries no name.
	Parse func(fallback string, body []byte) ([]T, error)
	Save  func(*Catalog, T) error

	ShowShort, ShowLong string
	// Summary renders the human view `show` prints by default; nil means the
	// record's YAML. YAMLBlob maps a record to the document `--yaml` prints
	// (agents are stored in an envelope); nil means the record itself.
	Summary  func(cmd *cobra.Command, c *Catalog, rec T) error
	YAMLBlob func(T) any

	AddDoc, SetDoc verbDoc
	Flags          []kindFlag
	AliasDoc       aliasDoc
	// NameFlag is the help of `add --name`, which stores an imported
	// document under another name. Empty: no such flag.
	NameFlag string
	// AddCheck refuses a flag-built record that is missing what the kind
	// cannot do without. It does not run when --set is given.
	AddCheck func(T) error
	// Saved echoes a completed write; set is false for add.
	Saved func(cmd *cobra.Command, c *Catalog, rec T, set bool) error
}

// recordSpec is typedRecord with T erased; every rec is a *T.
type recordSpec struct {
	get       func(*Catalog, string) (any, bool)
	clone     func(any) any
	value     func(any) any
	name      func(any) *string
	ring      func(any) assetring.Ring
	aliases   func(any) *[]string
	claims    func(any) []string
	parse     func(string, []byte) ([]any, error)
	save      func(*Catalog, any) error
	saveValue func(*Catalog, any) error // save of a T by value
	blank     func(string) any
	addCheck  func(any) error
	saved     func(*cobra.Command, *Catalog, any, bool) error
	summary   func(*cobra.Command, *Catalog, any) error
	yamlBlob  func(any) any
	showShort string
	showLong  string
	addDoc    verbDoc
	setDoc    verbDoc
	flags     []kindFlag
	aliasDoc  aliasDoc
	nameFlag  string
}

func newRecordSpec[T any](t typedRecord[T]) *recordSpec {
	r := &recordSpec{
		get: func(c *Catalog, name string) (any, bool) {
			v, ok := t.Get(c, name)
			return &v, ok
		},
		clone: func(rec any) any { v := *rec.(*T); return &v },
		value: func(rec any) any { return *rec.(*T) },
		name:  func(rec any) *string { return t.Name(rec.(*T)) },
		ring:  func(rec any) assetring.Ring { return t.Ring(*rec.(*T)) },
		parse: func(fallback string, body []byte) ([]any, error) {
			vs, err := t.Parse(fallback, body)
			out := make([]any, len(vs))
			for i := range vs {
				out[i] = &vs[i]
			}
			return out, err
		},
		save:      func(c *Catalog, rec any) error { return t.Save(c, *rec.(*T)) },
		saveValue: func(c *Catalog, v any) error { return t.Save(c, v.(T)) },
		blank: func(name string) any {
			v := new(T)
			*t.Name(v) = name
			return v
		},
		showShort: t.ShowShort, showLong: t.ShowLong,
		addDoc: t.AddDoc, setDoc: t.SetDoc,
		flags: t.Flags, aliasDoc: t.AliasDoc, nameFlag: t.NameFlag,
	}
	if t.Aliases != nil {
		r.aliases = func(rec any) *[]string { return t.Aliases(rec.(*T)) }
	}
	r.claims = func(rec any) []string {
		if t.Claims != nil {
			return t.Claims(*rec.(*T))
		}
		if t.Aliases == nil {
			return nil
		}
		return *t.Aliases(rec.(*T))
	}
	if t.AddCheck != nil {
		r.addCheck = func(rec any) error { return t.AddCheck(*rec.(*T)) }
	}
	if t.Saved != nil {
		r.saved = func(cmd *cobra.Command, c *Catalog, rec any, set bool) error {
			return t.Saved(cmd, c, *rec.(*T), set)
		}
	}
	if t.Summary != nil {
		r.summary = func(cmd *cobra.Command, c *Catalog, rec any) error {
			return t.Summary(cmd, c, *rec.(*T))
		}
	}
	if t.YAMLBlob != nil {
		r.yamlBlob = func(rec any) any { return t.YAMLBlob(*rec.(*T)) }
	}
	return r
}

// recordOf returns a kind's registered write surface. A kind without one has
// no generic verbs, so asking is a programming error.
func recordOf(kind string) (kindSpec, *recordSpec) {
	spec, ok := kindByName(kind)
	if !ok || spec.Record == nil {
		panic(fmt.Sprintf("fleet: kind %q has no record spec", kind))
	}
	return spec, spec.Record
}

type boundFlag struct {
	kindFlag
	apply func(rec any) error
}

// bindFlags registers the kind's flags that the verb carries.
func bindFlags(c *cobra.Command, flags []kindFlag, set bool) []boundFlag {
	var out []boundFlag
	for _, f := range flags {
		help := f.Add
		if set {
			help = f.Set
		}
		if help == "" {
			continue
		}
		out = append(out, boundFlag{kindFlag: f, apply: f.bind(c.Flags(), help)})
	}
	return out
}

// applyGiven writes every record flag the caller gave onto rec.
func applyGiven(cmd *cobra.Command, bound []boundFlag, rec any) error {
	for _, b := range bound {
		if b.Role == roleRecord && cmd.Flags().Changed(b.Name) {
			if err := b.apply(rec); err != nil {
				return err
			}
		}
	}
	return nil
}

func applyFinish(bound []boundFlag, rec any) error {
	for _, b := range bound {
		if b.Role == roleFinish {
			if err := b.apply(rec); err != nil {
				return err
			}
		}
	}
	return nil
}

func recordFlagGiven(cmd *cobra.Command, bound []boundFlag, paths pathFlags) bool {
	if len(paths.set) > 0 || len(paths.unset) > 0 || cmd.Flags().Changed("alias") {
		return true
	}
	for _, b := range bound {
		if b.Role == roleRecord && cmd.Flags().Changed(b.Name) {
			return true
		}
	}
	return false
}

func (r *recordSpec) usage(d verbDoc, verb string) *cobra.Command {
	use := d.Use
	if use == "" {
		use = verb + " <name>"
	}
	return &cobra.Command{
		Use: use, Short: d.Short, Long: d.Long, Example: d.Example,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
}

// newShow builds a kind's `show`: the record as YAML (--yaml), JSON, one
// dotted --field, or the kind's own human summary when it declares one.
func newShow(kind string, opts []Option) *cobra.Command {
	_, r := recordOf(kind)
	var asJSON, asYAML bool
	var field string
	c := r.usage(verbDoc{Short: r.showShort, Long: r.showLong}, "show")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		if err := checkFormat(asJSON, asYAML); err != nil {
			return err
		}
		cat := New(opts...)
		rec, ok := r.get(cat, args[0])
		if !ok {
			return fmt.Errorf("fleet: no %s %q", kind, args[0])
		}
		val := r.value(rec)
		switch {
		case field != "":
			if err := emitField(cmd.OutOrStdout(), val, kind, field, asJSON); err != nil {
				return reportPathError(cmd, kind, err)
			}
			return nil
		case asJSON:
			return emit(cmd.OutOrStdout(), val, true)
		case r.summary != nil && !asYAML:
			return r.summary(cmd, cat, rec)
		case r.yamlBlob != nil:
			return emit(cmd.OutOrStdout(), r.yamlBlob(rec), false)
		}
		return emit(cmd.OutOrStdout(), val, false)
	}
	jsonHelp, yamlHelp := "emit JSON instead of the canonical YAML", "emit the canonical YAML asset blob (the default)"
	if r.summary != nil {
		jsonHelp, yamlHelp = "emit JSON instead of a summary", "emit the canonical YAML asset blob"
	}
	c.Flags().BoolVar(&asJSON, "json", false, jsonHelp)
	c.Flags().BoolVar(&asYAML, "yaml", false, yamlHelp)
	c.Flags().StringVar(&field, "field", "", "print one dotted path")
	return c
}

func claimRecord(cat *Catalog, kind string, r *recordSpec, rec any, force bool) error {
	return cat.claimName(kind, *r.name(rec), r.claims(rec), force)
}

// newAdd builds a kind's `add`: a record from typed flags and --set paths on a
// blank one — starting from the lower-ring entry of the same name, so that
// unmentioned fields inherit — or a document imported from a file or stdin.
func newAdd(kind string, opts []Option) *cobra.Command {
	_, r := recordOf(kind)
	var force bool
	var name string
	var aliases []string
	var paths pathFlags
	c := r.usage(r.addDoc, "add")
	bound := bindFlags(c, r.flags, false)
	c.RunE = func(cmd *cobra.Command, args []string) error {
		cat := New(opts...)
		arg := args[0]
		var recs []any
		if looksLikePath(arg) && !recordFlagGiven(cmd, bound, paths) {
			fallback := baseName(arg)
			if name != "" {
				fallback = name
			}
			data, err := readSource(arg, cmd.InOrStdin())
			if err != nil {
				return err
			}
			if recs, err = r.parse(fallback, data); err != nil {
				return err
			}
			if name != "" {
				for _, rec := range recs {
					*r.name(rec) = name
				}
			}
		} else {
			rec, err := r.build(cmd, cat, kind, arg, bound, aliases, paths)
			if err != nil {
				return err
			}
			recs = []any{rec}
		}
		for _, rec := range recs {
			if err := applyFinish(bound, rec); err != nil {
				return err
			}
			if err := claimRecord(cat, kind, r, rec, force); err != nil {
				return err
			}
			if err := r.save(cat, rec); err != nil {
				return err
			}
			if r.saved != nil {
				if err := r.saved(cmd, cat, rec, false); err != nil {
					return err
				}
			}
		}
		return nil
	}
	c.Flags().BoolVar(&force, "force", false, "take a name that already belongs to another entry")
	if r.nameFlag != "" {
		c.Flags().StringVar(&name, "name", "", r.nameFlag)
	}
	if r.aliasDoc.Add != "" {
		c.Flags().StringArrayVar(&aliases, "alias", nil, r.aliasDoc.Add)
	}
	paths.bind(c)
	return c
}

// build makes the record a flag-built `add` saves.
func (r *recordSpec) build(cmd *cobra.Command, cat *Catalog, kind, name string, bound []boundFlag, aliases []string, paths pathFlags) (any, error) {
	given := func(rec any) error {
		if err := applyGiven(cmd, bound, rec); err != nil {
			return err
		}
		if cmd.Flags().Changed("alias") && r.aliases != nil {
			al := r.aliases(rec)
			*al = mergeAliases(*al, aliases, nil)
		}
		return nil
	}
	rec := r.blank(name)
	if err := given(rec); err != nil {
		return nil, err
	}
	if len(paths.set) == 0 && r.addCheck != nil {
		if err := r.addCheck(rec); err != nil {
			return nil, err
		}
	}
	if seed, ok := r.get(cat, name); ok && *r.name(seed) == name && r.ring(seed) != ringLocal() {
		// A flag-built mint over a seed starts from the seed, so unmentioned
		// fields inherit instead of nulling out of the sparse overlay
		// (story #1178). Minting under a name that only aliases a seed still
		// refuses in claimName.
		if err := given(seed); err != nil {
			return nil, err
		}
		rec = seed
	}
	if err := applyPathFlags(cmd, kind, rec, paths); err != nil {
		return nil, err
	}
	return rec, nil
}

// newSet builds a kind's `set`: typed flags and --set / --unset paths applied
// to the current entry. An entry from a lower ring gains a sparse local
// overlay; the lower ring is left unchanged.
func newSet(kind string, opts []Option) *cobra.Command {
	spec, r := recordOf(kind)
	var force bool
	var addAlias, rmAlias []string
	var paths pathFlags
	c := r.usage(r.setDoc, "set")
	bound := bindFlags(c, r.flags, true)
	overlayPaths := map[string]string{}
	for _, b := range bound {
		if b.Path != "" {
			overlayPaths[b.Name] = b.Path
		}
	}
	if r.aliases != nil {
		overlayPaths["add-alias"], overlayPaths["rm-alias"] = aliasPath, aliasPath
	}
	c.RunE = func(cmd *cobra.Command, args []string) error {
		cat := New(opts...)
		rec, ok := r.get(cat, args[0])
		if !ok {
			return fmt.Errorf("fleet: no %s %q", kind, args[0])
		}
		from := r.ring(rec)
		before := r.clone(rec)
		if err := applyGiven(cmd, bound, rec); err != nil {
			return err
		}
		if r.aliases != nil {
			al := r.aliases(rec)
			*al = mergeAliases(*al, addAlias, rmAlias)
		}
		if err := applyPathFlags(cmd, kind, rec, paths); err != nil {
			return err
		}
		if err := claimRecord(cat, kind, r, rec, force); err != nil {
			return err
		}
		n := *r.name(rec)
		if err := cat.saveChanged(spec.Plural, n, r.value(before), r.value(rec), changedOverlayPaths(cmd, paths, overlayPaths)); err != nil {
			return err
		}
		if from != ringLocal() {
			fmt.Fprintf(cmd.ErrOrStderr(), "note: overlaid %s from the %s ring in the local store\n", n, from)
		}
		if r.saved != nil {
			return r.saved(cmd, cat, rec, true)
		}
		return nil
	}
	c.Flags().BoolVar(&force, "force", false, "take a name that already belongs to another entry")
	if r.aliases != nil {
		c.Flags().StringArrayVar(&addAlias, "add-alias", nil, r.aliasDoc.AddAlias)
		c.Flags().StringArrayVar(&rmAlias, "rm-alias", nil, r.aliasDoc.RmAlias)
	}
	paths.bind(c)
	return c
}
