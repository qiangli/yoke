package fleet

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/assetring"
)

// Source-derived provenance for the ported cases below (Sprint 221 story
// S221.8 / B20). The upstream behavior family is PowerShell parameter
// binding and [ValidateSet]; the project-native subject is the PERSISTED
// schema — what a record may declare and what is refused when it is written
// — the shell's binder (sh interp.CommandSchema) owns the bind-time half.
//
//   Project:  PowerShell/PowerShell, tag v7.5.3
//   Commit:   b72c7ab1238c2d95b5c9004bca8399b8b3ca88ac
//   License:  MIT (LICENSE.txt at the repository root)
//   Sources:  test/powershell/Language/Scripting/ParameterBinding.Tests.ps1
//               'Test of positional parameters'
//               'Multiple positional parameters case 1'
//               'Mandatory parameters used in non-interactive host'
//               'Parameter default value is converted correctly to the
//                proper type when nothing is set on parameter'
//               "Validation attributes should not run on default values"
//               "ValidateSet can use custom ErrorMessage"
//             test/powershell/engine/ParameterBinding/ParameterBinding.Tests.ps1
//               "Verify that a SwitchParameter's IsPresent member is false
//                if the parameter is not specified"
//             src/System.Management.Automation/engine/Attributes.cs
//               ValidateSetAttribute(params string[] validValues): an empty
//               set is refused at declaration (ArgumentOutOfRangeException)
//   Also:     Microsoft Learn, "ValidateSet Attribute Declaration"
//             (docs content, CC-BY-4.0) — the same reference the shell half
//             cites, so both halves read one description of the set.
//
// The pre-existing registered-command schema cases (`commands schema`,
// `--set` paths, strict YAML decode, canonical round trip) come from this
// package's own commands_test.go / cli_show_test.go and are extended here.

// paintSchema is the schema the shell half's tests bind against
// (sh interp TestRegisteredCommandSchemaBindValidateInput), persisted.
func paintSchema() *CommandSchema {
	return &CommandSchema{
		Positionals: []CommandParameter{
			{Name: "color", Type: "string", Required: true, Enum: []string{"red", "blue"}},
			{Name: "level", Type: "int", Default: "3"},
		},
		Flags: []CommandFlag{
			{Name: "mode", Shorthand: "m", Type: "string", Default: "fast", Enum: []string{"fast", "slow"}},
			{Name: "count", Shorthand: "c", Type: "int", Required: true},
			{Name: "verbose", Shorthand: "v", Type: "bool"},
		},
	}
}

const paintYAML = `name: paint
kind: command
script: echo "$@"
args:
  positionals:
    - name: color
      type: string
      required: true
      enum:
        - red
        - blue
    - name: level
      type: int
      default: "3"
  flags:
    - name: mode
      shorthand: m
      type: string
      default: fast
      enum:
        - fast
        - slow
    - name: count
      shorthand: c
      type: int
      required: true
    - name: verbose
      shorthand: v
      type: bool
effects:
  - pure
`

// The schema is persisted field for field and survives the canonical
// round trip: YAML → record → store → record, with positional order and
// enum order intact (order is meaning for both).
func TestCommandSchemaRoundTrip(t *testing.T) {
	c, err := ParseCommand("paint", []byte(paintYAML), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Args, paintSchema()) {
		t.Fatalf("parsed schema = %+v", c.Args)
	}
	if err := c.Validate(reservedFixture); err != nil {
		t.Fatalf("valid schema refused: %v", err)
	}
	root := t.TempDir()
	cat := New(WithRoot(root), WithReservedNames(reservedFixture))
	if err := cat.SaveCommand(c); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "commands", "paint.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	stored := string(data)
	for _, want := range []string{"args:\n  positionals:\n    - name: color", "enum:\n        - red\n        - blue", "default: \"3\"", "shorthand: m"} {
		if !strings.Contains(stored, want) {
			t.Errorf("stored YAML lacks %q:\n%s", want, stored)
		}
	}
	again, ok := cat.Command("paint")
	if !ok || !reflect.DeepEqual(again.Args, paintSchema()) {
		t.Fatalf("re-read schema = %+v (ok=%v)", again.Args, ok)
	}
	// An empty-but-present schema means "accepts no arguments" and is kept.
	none := Command{Name: "noargs", Script: "true", Effects: []string{"pure"}, Args: &CommandSchema{}}
	if err := cat.SaveCommand(none); err != nil {
		t.Fatal(err)
	}
	if r, _ := cat.Command("noargs"); r.Args == nil || !r.Args.IsEmpty() {
		t.Errorf("empty schema must round-trip as present: %+v", r.Args)
	}
}

// Every record written before the schema existed is untouched: it parses
// with a nil schema, is saved without an args key, and editing an unrelated
// field never introduces one. A nil schema is the untyped pass-through the
// shell honored before — the compatibility contract of the whole feature.
func TestCommandSchemaBackwardCompatible(t *testing.T) {
	root := t.TempDir()
	opts := []Option{WithRoot(root), WithReservedNames(reservedFixture)}
	old := "name: gl\nkind: command\nscript: git log --oneline\neffects:\n  - read\n"
	c, err := ParseCommand("gl", []byte(old), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Args != nil {
		t.Fatalf("pre-schema record parsed with a schema: %+v", c.Args)
	}
	if !c.Args.IsEmpty() {
		t.Error("nil schema must read as empty")
	}
	if err := c.Args.Validate(); err != nil {
		t.Errorf("nil schema must validate: %v", err)
	}
	cat := New(opts...)
	if err := cat.SaveCommand(c); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "commands", "gl.yaml"))
	if strings.Contains(string(data), "args") {
		t.Errorf("saving a pre-schema record introduced an args key:\n%s", data)
	}
	if _, err := runCmd(t, NewCommandsCmd(opts...), "set", "gl", "--set", "synopsis=compact log"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(root, "commands", "gl.yaml"))
	if strings.Contains(string(data), "args") {
		t.Errorf("editing an unrelated field introduced an args key:\n%s", data)
	}
	if out, err := runCmd(t, NewCommandsCmd(opts...), "show", "gl", "--field", "args"); err != nil || strings.TrimSpace(out) != "null" {
		t.Errorf("show --field args on a pre-schema record: %v %q", err, out)
	}
	// A record from a shared ring without a schema is served as-is.
	shared := t.TempDir()
	if err := os.WriteFile(filepath.Join(shared, "org.yaml"), []byte("name: org\nkind: command\nexec: [/bin/true]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sc := New(WithRoot(t.TempDir()), WithSource(dirCommands, assetring.FileDir(shared, assetring.RingShared, ext)))
	if r, ok := sc.Command("org"); !ok || r.Args != nil {
		t.Errorf("shared pre-schema record = %+v ok=%v", r.Args, ok)
	}
}

// An invalid schema is refused LOUDLY, naming the --set path of the field,
// by Validate (so add/set/verify all say the same thing). Each row is a
// schema the shell's binder could not honor or would fail on every call.
func TestCommandSchemaRefusals(t *testing.T) {
	cases := []struct {
		name string
		args *CommandSchema
		want string
	}{
		{"positional no name", &CommandSchema{Positionals: []CommandParameter{{}}}, "args.positionals.0: name is empty"},
		{"positional bad name", &CommandSchema{Positionals: []CommandParameter{{Name: "a b"}}}, "args.positionals.0: name \"a b\""},
		{"duplicate positional", &CommandSchema{Positionals: []CommandParameter{{Name: "a"}, {Name: "a"}}}, "args.positionals.1: duplicate positional"},
		{"required after optional", &CommandSchema{Positionals: []CommandParameter{{Name: "a"}, {Name: "b", Required: true}}}, "args.positionals.1: required positional \"b\" follows an optional"},
		{"unknown type", &CommandSchema{Positionals: []CommandParameter{{Name: "a", Type: "date"}}}, "args.positionals.0.type: \"date\" is not one of string|int|float|bool"},
		{"required with default", &CommandSchema{Positionals: []CommandParameter{{Name: "a", Required: true, Default: "x"}}}, "args.positionals.0: required and default are exclusive"},
		{"default not int", &CommandSchema{Positionals: []CommandParameter{{Name: "n", Type: "int", Default: "many"}}}, "args.positionals.0.default: \"many\" is not an int"},
		{"default not bool", &CommandSchema{Flags: []CommandFlag{{Name: "v", Type: "bool", Default: "yes"}}}, "args.flags.0.default: \"yes\" is not a bool"},
		{"default not float", &CommandSchema{Flags: []CommandFlag{{Name: "r", Type: "float", Default: "1,5"}}}, "args.flags.0.default: \"1,5\" is not a float"},
		{"default outside enum", &CommandSchema{Positionals: []CommandParameter{{Name: "c", Default: "green", Enum: []string{"red", "blue"}}}}, "args.positionals.0.default: \"green\" is not one of red|blue"},
		{"empty enum entry", &CommandSchema{Positionals: []CommandParameter{{Name: "c", Enum: []string{"red", ""}}}}, "args.positionals.0.enum.1: an enum entry may not be empty"},
		{"duplicate enum entry", &CommandSchema{Flags: []CommandFlag{{Name: "m", Enum: []string{"a", "b", "a"}}}}, "args.flags.0.enum.2: duplicate entry \"a\""},
		{"enum entry not int", &CommandSchema{Flags: []CommandFlag{{Name: "n", Type: "int", Enum: []string{"1", "two"}}}}, "args.flags.0.enum.1: \"two\" is not an int"},
		{"enum entry not canonical", &CommandSchema{Flags: []CommandFlag{{Name: "n", Type: "int", Enum: []string{"007"}}}}, "args.flags.0.enum.0: \"007\" is not the canonical int spelling (\"7\")"},
		{"flag no name", &CommandSchema{Flags: []CommandFlag{{}}}, "args.flags.0: name is empty"},
		{"flag leading dash", &CommandSchema{Flags: []CommandFlag{{Name: "--count"}}}, "args.flags.0: name \"--count\" is not a flag name"},
		{"flag equals", &CommandSchema{Flags: []CommandFlag{{Name: "a=b"}}}, "args.flags.0: name \"a=b\""},
		{"duplicate flag", &CommandSchema{Flags: []CommandFlag{{Name: "a"}, {Name: "a"}}}, "args.flags.1: duplicate flag \"a\""},
		{"bad shorthand", &CommandSchema{Flags: []CommandFlag{{Name: "a", Shorthand: "-a"}}}, "args.flags.0: shorthand \"-a\""},
		{"duplicate shorthand", &CommandSchema{Flags: []CommandFlag{{Name: "a", Shorthand: "x"}, {Name: "b", Shorthand: "x"}}}, "args.flags.1: duplicate shorthand \"x\""},
		{"flag required with default", &CommandSchema{Flags: []CommandFlag{{Name: "a", Required: true, Default: "1"}}}, "args.flags.0: required and default are exclusive"},
	}
	for _, tc := range cases {
		rec := Command{Name: "x", Script: "true", Effects: []string{"pure"}, Args: tc.args}
		rec.applyDefaults()
		err := rec.Validate(reservedFixture)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want containing %q", tc.name, err, tc.want)
		}
		if err != nil && !strings.HasPrefix(err.Error(), `fleet: command "x": `) {
			t.Errorf("%s: refusal must name the command: %v", tc.name, err)
		}
	}
	// The schema is honored for every mode, not only script.
	ex := Command{Name: "x", Exec: []string{"/bin/true"}, Args: &CommandSchema{Positionals: []CommandParameter{{Name: "a", Type: "nope"}}}}
	ex.applyDefaults()
	if err := ex.Validate(reservedFixture); err == nil || !strings.Contains(err.Error(), "args.positionals.0.type") {
		t.Errorf("exec record with a bad schema: %v", err)
	}
	// Strict decode: a misspelled schema key is a parse error, never dropped.
	for _, body := range []string{
		"script: true\nargs:\n  positional:\n    - name: a\n",
		"script: true\nargs:\n  positionals:\n    - name: a\n      requried: true\n",
		"script: true\nargs:\n  flags:\n    - name: a\n      short: x\n",
	} {
		if _, err := ParseCommand("x", []byte(body), nil); err == nil {
			t.Errorf("unknown schema key accepted:\n%s", body)
		}
	}
	// A schema that is not a mapping is a parse error too.
	if _, err := ParseCommand("x", []byte("script: true\nargs: [a, b]\n"), nil); err == nil {
		t.Error("args as a list must be refused")
	}
}

// The schema is reachable through the CLI like every other field: --set
// paths (by index and by name=), `show --field`, `commands schema`, and a
// refused write leaves the store exactly as it was.
func TestCommandSchemaCLI(t *testing.T) {
	root := t.TempDir()
	opts := []Option{WithRoot(root), WithReservedNames(reservedFixture)}
	tree := func() *cobra.Command { return NewCommandsCmd(opts...) }

	out, err := runCmd(t, tree(), "add", "paint", "--set", "script=echo \"$@\"", "--set", "effects.0=pure",
		"--set", "args.positionals.0.name=color", "--set", "args.positionals.0.required=true",
		"--set", "args.positionals.0.enum.0=red", "--set", "args.positionals.0.enum.1=blue",
		"--set", "args.positionals.1.name=level", "--set", "args.positionals.1.type=int", "--set", "args.positionals.1.default=3",
		"--set", "args.flags.0.name=count", "--set", "args.flags.0.shorthand=c", "--set", "args.flags.0.type=int", "--set", "args.flags.0.required=true")
	if err != nil || !strings.Contains(out, "paint (script: pure)") {
		t.Fatalf("add: %v %q", err, out)
	}
	cat := New(opts...)
	r, ok := cat.Command("paint")
	if !ok {
		t.Fatal("paint not stored")
	}
	want := &CommandSchema{
		Positionals: []CommandParameter{{Name: "color", Required: true, Enum: []string{"red", "blue"}}, {Name: "level", Type: "int", Default: "3"}},
		Flags:       []CommandFlag{{Name: "count", Shorthand: "c", Type: "int", Required: true}},
	}
	if !reflect.DeepEqual(r.Args, want) {
		t.Fatalf("stored schema = %+v, want %+v", r.Args, want)
	}
	if out, err := runCmd(t, tree(), "show", "paint", "--field", "args.positionals.name=color.enum"); err != nil || strings.TrimSpace(out) != "- red\n- blue" {
		t.Errorf("show --field by name: %v %q", err, out)
	}
	if out, err := runCmd(t, tree(), "show", "paint", "--field", "args.flags.0.type"); err != nil || strings.TrimSpace(out) != "int" {
		t.Errorf("show --field flag type: %v %q", err, out)
	}
	// set by name= addresses the element; unset drops it.
	if _, err := runCmd(t, tree(), "set", "paint", "--set", "args.flags.name=count.default=1", "--set", "args.flags.name=count.required=false"); err != nil {
		t.Fatalf("set by name: %v", err)
	}
	if r, _ := cat.Command("paint"); r.Args.Flags[0].Default != "1" || r.Args.Flags[0].Required {
		t.Errorf("set by name did not land: %+v", r.Args.Flags[0])
	}
	if _, err := runCmd(t, tree(), "set", "paint", "--unset", "args.positionals.1"); err != nil {
		t.Fatalf("unset positional: %v", err)
	}
	if r, _ := cat.Command("paint"); len(r.Args.Positionals) != 1 {
		t.Errorf("unset did not drop the positional: %+v", r.Args.Positionals)
	}
	before, _ := os.ReadFile(filepath.Join(root, "commands", "paint.yaml"))
	// A refused edit says which path and changes nothing on disk.
	for _, bad := range [][]string{
		{"--set", "args.positionals.0.type=date"},
		{"--set", "args.positionals.0.default=green"},
		{"--set", "args.flags.0.name=--count"},
		{"--set", "args.positionals.1.name=extra", "--set", "args.positionals.1.required=true", "--set", "args.positionals.0.required=false"},
	} {
		out, err := runCmd(t, tree(), append([]string{"set", "paint"}, bad...)...)
		if err == nil || !strings.Contains(err.Error(), "args.") {
			t.Errorf("set %v: err = %v out = %q", bad, err, out)
		}
		after, _ := os.ReadFile(filepath.Join(root, "commands", "paint.yaml"))
		if string(after) != string(before) {
			t.Errorf("set %v: refused write changed the store:\n%s", bad, after)
		}
	}
	// A refused add writes no file.
	if _, err := runCmd(t, tree(), "add", "bad", "--set", "script=true", "--set", "effects.0=pure", "--set", "args.flags.0.name=a", "--set", "args.flags.0.type=bool", "--set", "args.flags.0.default=yes"); err == nil {
		t.Error("add with a bad default must be refused")
	}
	if _, err := os.Stat(filepath.Join(root, "commands", "bad.yaml")); err == nil {
		t.Error("refused add wrote a file")
	}
	// An unknown args path prints the schema, which lists the args paths.
	out, err = runCmd(t, tree(), "set", "paint", "--set", "args.positionals.0.kind=x")
	if err == nil || !strings.Contains(out, "args.positionals.<index>.enum.<index>") {
		t.Errorf("unknown args path must print the schema: %v %q", err, out)
	}
	out, err = runCmd(t, tree(), "schema")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"args", "args.positionals.<index>.name", "args.positionals.name=<value>.default", "args.flags.<index>.shorthand", "args.flags.name=<value>.enum.<index>"} {
		if !strings.Contains(out, path+"\t") && !strings.Contains(out, path+" ") {
			t.Errorf("commands schema lacks %q", path)
		}
	}
	// A record file with a schema is accepted by add <file> too.
	f := filepath.Join(t.TempDir(), "paint2.yaml")
	if err := os.WriteFile(f, []byte(strings.Replace(paintYAML, "name: paint", "name: paint2", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runCmd(t, tree(), "add", f); err != nil {
		t.Fatalf("add file: %v %q", err, out)
	}
	if r, _ := cat.Command("paint2"); !reflect.DeepEqual(r.Args, paintSchema()) {
		t.Errorf("schema from file = %+v", r.Args)
	}
	// And show --yaml reproduces the canonical blob, schema included.
	if out, err := runCmd(t, tree(), "show", "paint2", "--yaml"); err != nil || !strings.Contains(out, "args:\n  positionals:") {
		t.Errorf("show --yaml: %v %q", err, out)
	}
}

// A shared-ring record with an invalid schema cannot be edited by the
// operator, so verify is where it is reported — with the same path-naming
// message add would have given — and the catalog still lists the entry.
func TestCommandSchemaVerifyReportsInvalidSharedRecord(t *testing.T) {
	shared := t.TempDir()
	bad := "name: org\nkind: command\nexec: [/bin/true]\nargs:\n  positionals:\n    - name: mode\n      default: verbose\n      enum: [quiet, loud]\n"
	if err := os.WriteFile(filepath.Join(shared, "org.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	cat := New(WithRoot(t.TempDir()), WithSource(dirCommands, assetring.FileDir(shared, assetring.RingShared, ext)))
	if _, ok := cat.Command("org"); !ok {
		t.Fatal("entry with an invalid schema must still be listed (reported, never hidden)")
	}
	chk := cat.VerifyCommand(context.Background(), "org")
	if chk.OK || !strings.Contains(chk.Reason, "args.positionals.0.default") {
		t.Errorf("verify = %+v", chk)
	}
	// Argv is untouched by the schema: binding is the shell's, and what
	// reaches the record's argv is whatever the shell hands it.
	r, _ := cat.Command("org")
	if got := r.Argv("bashy", "", []string{"--x", "y"}); len(got) != 3 || got[1] != "--x" {
		t.Errorf("Argv rewrote arguments: %v", got)
	}
}

// Ported PowerShell cases (provenance in the file header). Each is the
// persistence-side reading of the upstream behavior: what the record must
// be able to say, and what it must refuse, for the binder to reproduce the
// upstream outcome.
func TestCommandSchemaPortedPowerShellCases(t *testing.T) {
	valid := func(t *testing.T, name string, s *CommandSchema) Command {
		t.Helper()
		rec := Command{Name: name, Script: "true", Effects: []string{"pure"}, Args: s}
		rec.applyDefaults()
		if err := rec.Validate(reservedFixture); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return rec
	}
	refused := func(t *testing.T, name string, s *CommandSchema, want string) {
		t.Helper()
		rec := Command{Name: name, Script: "true", Effects: []string{"pure"}, Args: s}
		rec.applyDefaults()
		if err := rec.Validate(reservedFixture); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v, want %q", name, err, want)
		}
	}

	// 'Test of positional parameters' / 'Multiple positional parameters case
	// 1': `get-foo a` and `get-foo -a b` bind the same parameter; the record
	// declares each positional once, in order, and the order is what is
	// persisted (the binder fills by position).
	t.Run("positional order persists", func(t *testing.T) {
		rec := valid(t, "get-foo", &CommandSchema{Positionals: []CommandParameter{{Name: "a"}, {Name: "b"}}})
		data, err := Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if ia, ib := strings.Index(string(data), "name: a"), strings.Index(string(data), "name: b"); ia < 0 || ib < ia {
			t.Errorf("positional order lost:\n%s", data)
		}
	})

	// 'Mandatory parameters used in non-interactive host': a mandatory
	// parameter the call omits is an ERROR, not a prompt — so `required`
	// is a persisted fact of the parameter, and a required one may not
	// also carry a default (there would be nothing mandatory about it).
	t.Run("mandatory is persisted and exclusive with default", func(t *testing.T) {
		rec := valid(t, "get-foo", &CommandSchema{Positionals: []CommandParameter{{Name: "p", Required: true}}})
		c, err := ParseCommand("get-foo", mustMarshal(t, rec), nil)
		if err != nil || !c.Args.Positionals[0].Required {
			t.Fatalf("required not persisted: %v %+v", err, c.Args)
		}
		refused(t, "get-foo", &CommandSchema{Positionals: []CommandParameter{{Name: "p", Required: true, Default: "x"}}}, "required and default are exclusive")
	})

	// "Verify that a SwitchParameter's IsPresent member is false if the
	// parameter is not specified": a switch needs no default to be false
	// when absent — a bool flag is persisted without one, and its presence
	// alone is the binder's true.
	t.Run("switch is a bool flag with no default", func(t *testing.T) {
		rec := valid(t, "sw", &CommandSchema{Flags: []CommandFlag{{Name: "Parameter1", Type: "bool"}}})
		if strings.Contains(string(mustMarshal(t, rec)), "default") {
			t.Error("a bool flag must not be given a default it did not declare")
		}
	})

	// 'Parameter default value is converted correctly to the proper type
	// when nothing is set on parameter': a default is a value OF the
	// declared type. Upstream converts it at bind time; the record refuses
	// a default that cannot convert, so the binder never meets one.
	t.Run("default must convert to the declared type", func(t *testing.T) {
		valid(t, "get-fooa", &CommandSchema{Positionals: []CommandParameter{{Name: "n", Type: "int", Default: "007"}}})
		valid(t, "get-fooa", &CommandSchema{Flags: []CommandFlag{{Name: "ratio", Type: "float", Default: "1e3"}}})
		valid(t, "get-fooa", &CommandSchema{Flags: []CommandFlag{{Name: "on", Type: "bool", Default: "T"}}})
		refused(t, "get-fooa", &CommandSchema{Positionals: []CommandParameter{{Name: "n", Type: "int", Default: "many"}}}, `"many" is not an int`)
	})

	// "Validation attributes should not run on default values": upstream a
	// [ValidateRange(1,42)] $p = 55 is accepted because validation runs on
	// INPUT, not on the declaration's default. The project-native reading
	// is split by owner: the binder validates input only, exactly as
	// upstream; but the record is checked when WRITTEN, and a default
	// outside its own enum is a declaration the binder (which converts the
	// default and matches it against the enum on every omitted call) would
	// fail for every invocation — so the record refuses it loudly at write
	// time. A default inside the enum is accepted without an input.
	t.Run("default is checked against the enum at write time", func(t *testing.T) {
		valid(t, "get-fooe", &CommandSchema{Positionals: []CommandParameter{{Name: "p", Type: "int", Default: "7", Enum: []string{"1", "7", "42"}}}})
		refused(t, "get-fooe", &CommandSchema{Positionals: []CommandParameter{{Name: "p", Type: "int", Default: "55", Enum: []string{"1", "7", "42"}}}}, `"55" is not one of 1|7|42`)
	})

	// "ValidateSet can use custom ErrorMessage": the refusal names the set
	// in declaration order ('A','B','C'). The set is persisted verbatim and
	// in order; a value outside it is refused by the binder with that list.
	// The record's own literals (a default) get the same list in the same
	// order when refused here.
	t.Run("ValidateSet persists in declaration order", func(t *testing.T) {
		rec := valid(t, "get-fook", &CommandSchema{Flags: []CommandFlag{{Name: "p", Enum: []string{"A", "B", "C"}}}})
		c, err := ParseCommand("get-fook", mustMarshal(t, rec), nil)
		if err != nil || !reflect.DeepEqual(c.Args.Flags[0].Enum, []string{"A", "B", "C"}) {
			t.Fatalf("set not persisted in order: %v %+v", err, c.Args)
		}
		refused(t, "get-fook", &CommandSchema{Flags: []CommandFlag{{Name: "p", Default: "2", Enum: []string{"A", "B", "C"}}}}, `"2" is not one of A|B|C`)
	})

	// Attributes.cs ValidateSetAttribute(params string[] validValues): a
	// set with no values is refused at DECLARATION. A record has no way to
	// spell an empty set (omitempty drops it — no enum means unconstrained,
	// which is the only sensible reading), and an entry that is empty is
	// refused, so the closest project-native statement of the rule is: an
	// enum, when present, has only real values.
	t.Run("ValidateSet has no empty entries", func(t *testing.T) {
		refused(t, "vs", &CommandSchema{Flags: []CommandFlag{{Name: "p", Enum: []string{""}}}}, "an enum entry may not be empty")
		rec := valid(t, "vs", &CommandSchema{Flags: []CommandFlag{{Name: "p", Enum: []string{}}}})
		c, err := ParseCommand("vs", mustMarshal(t, rec), nil)
		if err != nil || len(c.Args.Flags[0].Enum) != 0 {
			t.Fatalf("empty enum must persist as no enum: %v %+v", err, c.Args)
		}
	})
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
