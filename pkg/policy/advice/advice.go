// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package advice is the opt-in rules loader for policy-applied decorators
// (Bash++ "advice" — see dhnt/docs/bashpp-decorators-and-advice.md §4). A
// rules file maps selectors bashy already has — function-name glob, source
// file glob, the agentic marker — to decorator specs the engine applies at
// function registration. There is no pointcut DSL and no call-site join
// point by design.
//
// The contract, in order of importance:
//
//   - Opt-in: no rules unless BASHY_ADVICE names a file, and always zero
//     rules under VSC_PROFILE=cert (certification measures the host; a
//     developer-supplied rules file must not change what it measures).
//   - Add-only, outermost, deny-only: the advisable decorator set is
//     observe/deny-class only (trace, guard today; log when implemented).
//     retry, memo, and timeout are NEVER advisable — a policy-applied
//     re-execution or replacement of side effects behind the author's back
//     is exactly the AOP obliviousness hazard this design refuses.
//   - Deterministic: matching is pure, rules apply in file order, and every
//     rule has a stable ID (explicit, or content-derived) so application is
//     idempotent — a re-eval'd function never stacks duplicates.
//   - Preamble excluded by default: a bare "*" selector does not reach the
//     host shell's own preamble functions unless a rule opts in with
//     "exclude": [].
//
// The Spec/Arg/Value types are standalone value types: this package imports
// no shell engine, so both mvdan.cc/sh's interp.Advice option and bashy can
// mirror or consume them without a dependency cycle.
//
// Deps: stdlib + pkg/atlas (the effect vocabulary the guard cap projects
// onto).
package advice

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
)

// SchemaVersion identifies the rules-file shape. Bump on a breaking change.
const SchemaVersion = "bashy-advice-v1"

// EnvVar is the opt-in switch: the environment variable naming the rules
// file. Unset or empty means advice is off.
const EnvVar = "BASHY_ADVICE"

// Kind discriminates a decorator argument value.
type Kind int

const (
	KindString Kind = iota
	KindInt
	KindBool
)

// Value is one decorator argument value. It is a closed sum — string,
// integer, or bool — because that is what both the JSON rules file and the
// Bash# keyword-argument binding can carry.
type Value struct {
	Kind Kind
	Str  string
	Int  int64
	Bool bool
}

// String renders the value the way a decorator line would spell it.
func (v Value) String() string {
	switch v.Kind {
	case KindInt:
		return fmt.Sprintf("%d", v.Int)
	case KindBool:
		return fmt.Sprintf("%t", v.Bool)
	default:
		return fmt.Sprintf("%q", v.Str)
	}
}

// Arg is one named decorator argument.
type Arg struct {
	Name  string
	Value Value
}

// Spec is one decorator application: the decorator's name and its keyword
// arguments, sorted by name (the JSON object that carries them has no order,
// so sorted order is the deterministic one).
type Spec struct {
	Decorator string
	Args      []Arg
}

// String renders the spec as the decorator line it stands for, e.g.
// `@guard(effects: "net,read")`.
func (s Spec) String() string {
	var b strings.Builder
	b.WriteString("@")
	b.WriteString(s.Decorator)
	b.WriteString("(")
	for i, a := range s.Args {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(a.Name)
		b.WriteString(": ")
		b.WriteString(a.Value.String())
	}
	b.WriteString(")")
	return b.String()
}

// Rule is one validated advice rule: selectors plus the single decorator it
// advises. One decorator per rule keeps the rule ID and the applied
// decorator one-to-one, which is what makes idempotence-by-rule-ID
// unambiguous.
type Rule struct {
	// ID is stable across loads: either the explicit "id" from the file, or
	// derived from the rule's content (so the same rule always carries the
	// same ID regardless of its position in the file).
	ID string

	// Selectors. A rule matches when every set selector matches (AND).
	// Name and File are path.Match globs; empty means "not selecting on
	// this axis". Agentic nil means "either".
	Name    string
	File    string
	Agentic *bool

	// ExcludePreamble is true unless the file said "exclude": [] — the
	// host's own preamble functions (registered before user code) are out
	// of scope by default so "*" means the user's code.
	ExcludePreamble bool

	Spec Spec
}

// Rules is a validated, ordered rule set. The zero of *Rules (nil) is the
// advice-off state: every method is nil-safe and matches nothing.
type Rules struct {
	rules []Rule
}

// Len reports the number of rules.
func (r *Rules) Len() int {
	if r == nil {
		return 0
	}
	return len(r.rules)
}

// All returns a copy of the rules in file order, for inspection surfaces
// (`bashy inspect advice`).
func (r *Rules) All() []Rule {
	if r == nil {
		return nil
	}
	out := make([]Rule, len(r.rules))
	copy(out, r.rules)
	for i := range out {
		out[i].Spec.Args = append([]Arg(nil), out[i].Spec.Args...)
		if out[i].Agentic != nil {
			b := *out[i].Agentic
			out[i].Agentic = &b
		}
	}
	return out
}

// FromEnv resolves the opt-in: it loads the rules file named by BASHY_ADVICE
// in env (os.Environ shape, last duplicate wins), or returns (nil, nil) when
// advice is off. Advice is off when BASHY_ADVICE is unset or empty, and
// ALWAYS under VSC_PROFILE=cert — the certification profile carries zero
// rules and never even opens the file, so a cert run cannot vary on a
// developer host's rules file (precedent: the registered-command ring and
// the locale host-default, which stand down the same way).
func FromEnv(env []string) (*Rules, error) {
	if profile, ok := getEnv(env, "VSC_PROFILE"); ok && profile == "cert" {
		return nil, nil
	}
	path, ok := getEnv(env, EnvVar)
	if !ok || strings.TrimSpace(path) == "" {
		return nil, nil
	}
	return Load(path)
}

// Load reads and parses the rules file at path.
func Load(path string) (*Rules, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("advice: %w", err)
	}
	r, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%w (in %s)", err, path)
	}
	return r, nil
}

// wireRule is the JSON shape of one rule. Exclude is a pointer so an absent
// field (default: exclude preamble) is distinguishable from an explicit
// empty list (opt in to preamble).
type wireRule struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	File      string         `json:"file"`
	Agentic   *bool          `json:"agentic"`
	Exclude   *[]string      `json:"exclude"`
	Decorator string         `json:"decorator"`
	Args      map[string]any `json:"args"`
}

type wireFile struct {
	Schema string     `json:"schema"`
	Rules  []wireRule `json:"rules"`
}

// Parse decodes and validates a rules file. Decoding is strict — an unknown
// field anywhere is an error, so a typo'd selector cannot silently match
// everything — and every rule is validated: selectors present and
// well-formed, decorator advisable, arguments typed, IDs unique.
func Parse(data []byte) (*Rules, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	var f wireFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("advice: parse rules: %w", err)
	}
	var dummy any
	if err := dec.Decode(&dummy); err != io.EOF {
		return nil, fmt.Errorf("advice: parse rules: trailing data after the rules object")
	}
	if f.Schema != SchemaVersion {
		return nil, fmt.Errorf("advice: schema %q, want %q", f.Schema, SchemaVersion)
	}
	out := &Rules{}
	seen := map[string]int{} // id -> 1-based rule number
	for i, w := range f.Rules {
		r, err := validateRule(w)
		if err != nil {
			return nil, fmt.Errorf("advice: rule %d%s: %w", i+1, idSuffix(w.ID), err)
		}
		if prev, dup := seen[r.ID]; dup {
			return nil, fmt.Errorf("advice: rule %d%s: duplicate id %q (also rule %d)", i+1, idSuffix(w.ID), r.ID, prev)
		}
		seen[r.ID] = i + 1
		out.rules = append(out.rules, r)
	}
	return out, nil
}

func idSuffix(id string) string {
	if id == "" {
		return ""
	}
	return fmt.Sprintf(" (%s)", id)
}

// neverAdvisable are decorators that re-execute or replace the target's side
// effects; applying one by policy, behind the author's back, is the
// obliviousness hazard the design refuses. Decision-of-record #6 in
// bashpp-decorators-and-advice.md: not "not yet" — never.
var neverAdvisable = map[string]bool{"retry": true, "memo": true, "timeout": true}

// notYetAdvisable are observe/deny-class decorators planned for the
// advisable set but not implemented; a rules file naming one fails loudly
// instead of silently advising nothing.
var notYetAdvisable = map[string]bool{"log": true}

func validateRule(w wireRule) (Rule, error) {
	r := Rule{
		ID:              w.ID,
		Name:            w.Name,
		File:            w.File,
		Agentic:         w.Agentic,
		ExcludePreamble: true,
	}
	// Selectors: at least one, and globs must be well-formed.
	if w.Name == "" && w.File == "" && w.Agentic == nil {
		return Rule{}, fmt.Errorf("no selector (set name, file, or agentic; %q matches every user function)", "*")
	}
	for _, g := range []struct{ field, pat string }{{"name", w.Name}, {"file", w.File}} {
		if g.pat == "" {
			continue
		}
		if _, err := path.Match(g.pat, ""); err != nil {
			return Rule{}, fmt.Errorf("%s glob %q: %w", g.field, g.pat, err)
		}
	}
	// Exclude: absent means ["preamble"]; the only recognized token is
	// "preamble".
	if w.Exclude != nil {
		r.ExcludePreamble = false
		for _, e := range *w.Exclude {
			if e != "preamble" {
				return Rule{}, fmt.Errorf("unknown exclude %q (only %q)", e, "preamble")
			}
			r.ExcludePreamble = true
		}
	}
	// Decorator + args.
	spec, err := validateSpec(w.Decorator, w.Args)
	if err != nil {
		return Rule{}, err
	}
	r.Spec = spec
	if r.ID == "" {
		r.ID = deriveID(r)
	}
	return r, nil
}

func validateSpec(name string, rawArgs map[string]any) (Spec, error) {
	args, err := decodeArgs(rawArgs)
	if err != nil {
		return Spec{}, err
	}
	s := Spec{Decorator: name, Args: args}
	switch {
	case name == "":
		return Spec{}, fmt.Errorf("no decorator")
	case neverAdvisable[name]:
		return Spec{}, fmt.Errorf("decorator %q is never advisable: a policy-applied retry/memo/timeout re-executes or replaces the author's side effects", name)
	case notYetAdvisable[name]:
		return Spec{}, fmt.Errorf("decorator %q is not implemented yet (advisable today: guard, trace)", name)
	case name == "trace":
		if len(args) != 0 {
			return Spec{}, fmt.Errorf("trace takes no arguments")
		}
	case name == "guard":
		if len(args) != 1 || args[0].Name != "effects" {
			return Spec{}, fmt.Errorf("guard takes exactly one argument, effects: %q", "read,net")
		}
		if args[0].Value.Kind != KindString {
			return Spec{}, fmt.Errorf("guard effects must be a string")
		}
		if _, err := ParseCap(args[0].Value.Str); err != nil {
			return Spec{}, err
		}
	default:
		return Spec{}, fmt.Errorf("unknown decorator %q (advisable: guard, trace)", name)
	}
	return s, nil
}

// decodeArgs turns the JSON args object into the sorted, closed-sum Arg
// list. Only string, integer, and bool values are representable.
func decodeArgs(raw map[string]any) ([]Arg, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(raw))
	for k := range raw {
		names = append(names, k)
	}
	sort.Strings(names)
	args := make([]Arg, 0, len(names))
	for _, k := range names {
		var v Value
		switch t := raw[k].(type) {
		case string:
			v = Value{Kind: KindString, Str: t}
		case bool:
			v = Value{Kind: KindBool, Bool: t}
		case json.Number:
			n, err := t.Int64()
			if err != nil {
				return nil, fmt.Errorf("arg %s: %v is not an integer (string, integer, and bool only)", k, t)
			}
			v = Value{Kind: KindInt, Int: n}
		default:
			return nil, fmt.Errorf("arg %s: unsupported value type (string, integer, and bool only)", k)
		}
		args = append(args, Arg{Name: k, Value: v})
	}
	return args, nil
}

// deriveID returns the stable content-derived ID for a rule without an
// explicit one. It hashes the rule's normalized content — never its position
// — so the same rule keeps the same ID across loads and reorders, which is
// what lets application stay idempotent by rule ID.
func deriveID(r Rule) string {
	var b strings.Builder
	agentic := ""
	if r.Agentic != nil {
		agentic = fmt.Sprintf("%t", *r.Agentic)
	}
	for _, part := range []string{r.Name, r.File, agentic, fmt.Sprintf("%t", r.ExcludePreamble), r.Spec.String()} {
		b.WriteString(part)
		b.WriteByte(0)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "r-" + hex.EncodeToString(sum[:])[:12]
}

// getEnv looks up key in env (os.Environ shape); the last duplicate wins,
// matching tool.RunContext.Getenv semantics.
func getEnv(env []string, key string) (string, bool) {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return env[i][len(prefix):], true
		}
	}
	return "", false
}
