package fleet

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The persisted argument schema of a registered command (`args:` in the
// record, `--set args.…` on the CLI). It is the OPERATOR's declaration of
// the arguments a command accepts — typed positionals, named flags, enums,
// defaults — so the shell can bind and validate an invocation BEFORE the
// script body (or program) is entered, and a bad call fails with a message
// about the argument instead of whatever the body does with garbage.
//
// The split is load-bearing and there is exactly one binder: yoke owns the
// RECORD — persistence, the canonical YAML, and refusing an invalid schema
// loudly at write time (`commands add/set`) and at `commands verify`; the
// shell (sh's interp.CommandSchema, handed over through the CommandResolver
// as ResolvedCommand.Schema) owns the one bind/validate-input step at
// dispatch. The three types here mirror interp's field for field, so the
// host's adapter is a plain copy and the two halves cannot disagree about
// what a schema says. Nothing in this file binds an argv.
//
// A record WITHOUT `args:` (every record written before the schema existed)
// is untouched: nil here, absent from the canonical YAML, and the shell's
// pre-schema pass-through contract applies exactly.
//
// The two literal checks below (a default converts to its declared type and
// sits in its enum; an enum entry is in the canonical spelling the binder
// compares against) are the write-time half of what the binder does with
// INPUT at run time: the binder converts a value and then matches the
// canonical form against the enum, so a record whose own literals cannot
// pass that step would fail every invocation. Refusing it when it is
// written is the loud-schema rule; it is not a second binder.

// CommandSchema is the argument surface of one registered command.
type CommandSchema struct {
	Positionals []CommandParameter `yaml:"positionals,omitempty" json:"positionals,omitempty" doc:"positional arguments in order; required ones first, then optional (a default makes one optional)"`
	Flags       []CommandFlag      `yaml:"flags,omitempty" json:"flags,omitempty" doc:"named arguments (--name, --name=value; -x for a shorthand); a bool flag given without a value is true"`
}

// CommandParameter is one positional argument.
type CommandParameter struct {
	Name     string   `yaml:"name" json:"name" doc:"parameter name (a bare identifier; what an error message calls it)"`
	Type     string   `yaml:"type,omitempty" json:"type,omitempty" doc:"string (default) | int | float | bool; the value is converted before the body runs"`
	Required bool     `yaml:"required,omitempty" json:"required,omitempty" doc:"the invocation must supply it (a required positional may not carry a default)"`
	Default  string   `yaml:"default,omitempty" json:"default,omitempty" doc:"value bound when the invocation omits it; must convert to type and, with an enum, be one of its entries"`
	Enum     []string `yaml:"enum,omitempty" json:"enum,omitempty" doc:"the closed set of accepted values (non-empty entries, each in the canonical spelling of type)"`
}

// CommandFlag is one named argument. Name is used as --name and Shorthand,
// when set, as -x.
type CommandFlag struct {
	Name      string   `yaml:"name" json:"name" doc:"long flag name, used as --name (letters, digits, _ and -; no leading dash)"`
	Shorthand string   `yaml:"shorthand,omitempty" json:"shorthand,omitempty" doc:"short spelling, used as -x (letters and digits)"`
	Type      string   `yaml:"type,omitempty" json:"type,omitempty" doc:"string (default) | int | float | bool; a bool flag given with no value binds to true"`
	Required  bool     `yaml:"required,omitempty" json:"required,omitempty" doc:"the invocation must supply it (a required flag may not carry a default)"`
	Default   string   `yaml:"default,omitempty" json:"default,omitempty" doc:"value bound when the invocation omits it; must convert to type and, with an enum, be one of its entries"`
	Enum      []string `yaml:"enum,omitempty" json:"enum,omitempty" doc:"the closed set of accepted values (non-empty entries, each in the canonical spelling of type)"`
}

// The closed type vocabulary of a schema value. The empty string means
// string, so a record that names no type reads as untyped.
const (
	ArgTypeString = "string"
	ArgTypeInt    = "int"
	ArgTypeFloat  = "float"
	ArgTypeBool   = "bool"
)

// CommandSchemaTypes returns the types a positional or flag may declare.
func CommandSchemaTypes() []string {
	return []string{ArgTypeString, ArgTypeInt, ArgTypeFloat, ArgTypeBool}
}

var (
	argParamName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
	argFlagName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	argShorthand = regexp.MustCompile(`^[A-Za-z0-9]+$`)
)

// IsEmpty reports whether the schema declares nothing (or is nil). An empty
// but present schema is meaningful — the command accepts no arguments — so
// callers must not collapse it to nil.
func (s *CommandSchema) IsEmpty() bool {
	return s == nil || len(s.Positionals) == 0 && len(s.Flags) == 0
}

// Validate refuses a schema the binder could not honor, naming the `--set`
// path of the offending field (args.positionals.<i>.…, args.flags.<i>.…).
// A nil schema is valid: it is the pre-schema record. The rules are a
// superset of the binder's own structural checks, so a record this accepts
// is one the shell accepts; anything looser here would save a record that
// fails at every invocation.
func (s *CommandSchema) Validate() error {
	if s == nil {
		return nil
	}
	seen := make(map[string]bool, len(s.Positionals))
	optionalSeen := false
	for i, p := range s.Positionals {
		at := fmt.Sprintf("args.positionals.%d", i)
		if p.Name == "" {
			return fmt.Errorf("%s: name is empty", at)
		}
		if !argParamName.MatchString(p.Name) {
			return fmt.Errorf("%s: name %q is not a bare identifier", at, p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("%s: duplicate positional %q", at, p.Name)
		}
		seen[p.Name] = true
		if p.Required && optionalSeen {
			return fmt.Errorf("%s: required positional %q follows an optional one — required positionals come first", at, p.Name)
		}
		if err := validateArgLiterals(at, p.Type, p.Required, p.Default, p.Enum); err != nil {
			return err
		}
		if !p.Required {
			optionalSeen = true
		}
	}
	seenLong := make(map[string]bool, len(s.Flags))
	seenShort := make(map[string]bool, len(s.Flags))
	for i, f := range s.Flags {
		at := fmt.Sprintf("args.flags.%d", i)
		if f.Name == "" {
			return fmt.Errorf("%s: name is empty", at)
		}
		if !argFlagName.MatchString(f.Name) {
			return fmt.Errorf("%s: name %q is not a flag name (letters, digits, _ and -; no leading dash — the record names the flag, the invocation spells --%s)", at, f.Name, strings.TrimLeft(f.Name, "-"))
		}
		if seenLong[f.Name] {
			return fmt.Errorf("%s: duplicate flag %q", at, f.Name)
		}
		seenLong[f.Name] = true
		if f.Shorthand != "" {
			if !argShorthand.MatchString(f.Shorthand) {
				return fmt.Errorf("%s: shorthand %q is not letters and digits (no leading dash)", at, f.Shorthand)
			}
			if seenShort[f.Shorthand] {
				return fmt.Errorf("%s: duplicate shorthand %q", at, f.Shorthand)
			}
			seenShort[f.Shorthand] = true
		}
		if err := validateArgLiterals(at, f.Type, f.Required, f.Default, f.Enum); err != nil {
			return err
		}
	}
	return nil
}

// validateArgLiterals checks the parts a positional and a flag share: the
// type, the required/default exclusion, the enum, and that the default is
// a value the binder would accept.
func validateArgLiterals(at, typ string, required bool, def string, enum []string) error {
	if !slices.Contains(CommandSchemaTypes(), argType(typ)) {
		return fmt.Errorf("%s.type: %q is not one of %s", at, typ, strings.Join(CommandSchemaTypes(), "|"))
	}
	for j, e := range enum {
		if e == "" {
			return fmt.Errorf("%s.enum.%d: an enum entry may not be empty", at, j)
		}
		canon, err := canonicalArgValue(typ, e)
		if err != nil {
			return fmt.Errorf("%s.enum.%d: %w", at, j, err)
		}
		if canon != e {
			return fmt.Errorf("%s.enum.%d: %q is not the canonical %s spelling (%q) the binder compares against", at, j, e, argType(typ), canon)
		}
		if slices.Contains(enum[:j], e) {
			return fmt.Errorf("%s.enum.%d: duplicate entry %q", at, j, e)
		}
	}
	if required && def != "" {
		return fmt.Errorf("%s: required and default are exclusive — a required argument is never defaulted", at)
	}
	if def != "" {
		canon, err := canonicalArgValue(typ, def)
		if err != nil {
			return fmt.Errorf("%s.default: %w", at, err)
		}
		if len(enum) > 0 && !slices.Contains(enum, canon) {
			return fmt.Errorf("%s.default: %q is not one of %s", at, def, strings.Join(enum, "|"))
		}
	}
	return nil
}

func argType(typ string) string {
	if typ == "" {
		return ArgTypeString
	}
	return typ
}

// canonicalArgValue converts one schema literal to the spelling the binder
// produces for a value of typ, or reports why it cannot. Same rules as the
// binder's conversion: base-10 int, Go float syntax, strconv bool spellings.
func canonicalArgValue(typ, value string) (string, error) {
	switch argType(typ) {
	case ArgTypeString:
		return value, nil
	case ArgTypeInt:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return "", fmt.Errorf("%q is not an int", value)
		}
		return strconv.FormatInt(n, 10), nil
	case ArgTypeFloat:
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return "", fmt.Errorf("%q is not a float", value)
		}
		return strconv.FormatFloat(f, 'g', -1, 64), nil
	case ArgTypeBool:
		b, err := strconv.ParseBool(value)
		if err != nil {
			return "", fmt.Errorf("%q is not a bool", value)
		}
		return strconv.FormatBool(b), nil
	}
	return "", fmt.Errorf("type %q is not one of %s", typ, strings.Join(CommandSchemaTypes(), "|"))
}
