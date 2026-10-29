// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package unit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// ErrNoContract reports that a unit declares no contract — no Require and no
// Ensure clause — and therefore has no capability key.
//
// It is a refusal, not a fallback: hashing an empty projection would give
// every contract-less unit one shared key, silently merging units that
// guarantee nothing in common. A contract-less unit is UNCONTRACTED: it can
// run and leave a receipt, but nothing about it can ever be verified.
var ErrNoContract = errors.New("unit: unit declares no contract, so it has no capability key")

// Contract is what a unit promises, read the one way whichever face states
// it: a dag target's metadata lines, a Bash# body's decorators, or a dhnt
// skill's contract + effect cap. Only Require, Ensure and the effect cap feed
// the capability key; Guard, Confirm and Timeout are run policy, recorded so
// every reader sees them but projected away like a dag target's placement.
type Contract struct {
	Require []string `json:"require,omitempty"` // preconditions (@require, Require:)
	Ensure  []string `json:"ensure,omitempty"`  // postconditions (@ensure, Ensure:, dhnt contract)
	Effects []string `json:"effects,omitempty"` // declared effect atoms (@effects, Effects:, dhnt effect cap)
	Guard   []string `json:"guard,omitempty"`   // enforced effect cap (@guard)
	Confirm []string `json:"confirm,omitempty"` // @confirm arguments ("on" when bare)
	Timeout string   `json:"timeout,omitempty"` // @timeout duration
}

// Contracted reports whether the unit states a checkable guarantee. An
// uncontracted unit has no capability key and is never verified.
func (c Contract) Contracted() bool {
	return len(normalise(c.Require)) > 0 || len(normalise(c.Ensure)) > 0
}

// EffectCap is the unit's declared blast radius: its @effects/Effects:
// atoms, or — when it declares none — the @guard cap it enforces on itself.
// Authored order is kept (duplicates dropped); the key sorts on its own.
func (c Contract) EffectCap() []string {
	if eff := distinct(c.Effects); len(eff) > 0 {
		return eff
	}
	return distinct(c.Guard)
}

func distinct(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// CapabilityKey is the one capability key ("k" + sha256) over a contract's
// projection: sorted, deduplicated require/ensure/effect-cap lines. Name,
// body, steps and run policy are projected away, so two units keeping the
// same promise share a key whichever face (dag, Bash#, dhnt) states it.
// Returns ErrNoContract for an uncontracted unit.
func CapabilityKey(c Contract) (string, error) {
	if !c.Contracted() {
		return "", ErrNoContract
	}
	var b strings.Builder
	for _, r := range normalise(c.Require) {
		b.WriteString("require: " + r + "\n")
	}
	for _, e := range normalise(c.Ensure) {
		b.WriteString("ensure: " + e + "\n")
	}
	for _, ef := range normalise(c.EffectCap()) {
		b.WriteString("effects: " + ef + "\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "k" + hex.EncodeToString(sum[:]), nil
}

// BodyIdentity is the one content address ("h" + sha256) of a unit's body
// bytes, CRLF normalised to LF. Any other byte change is a new identity.
func BodyIdentity(body []byte) string {
	sum := sha256.Sum256(bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n")))
	return "h" + hex.EncodeToString(sum[:])
}

func normalise(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ReadContract is the one contract reader for a Bash# body. The body is
// parsed by sh/syntax (the normative Bash# grammar), so a decorator counts
// only where the shell would apply it — on a function declaration — and a
// decorator-looking line inside a string or heredoc is not a contract.
//
// Both argument forms are read: positional (`@guard("read,write")`) and
// keyword (`@guard(effects: "read,write")`). @require/@ensure contribute one
// check per argument; @effects/@guard split their atoms on commas and
// spaces; @confirm records its arguments ("on" when bare); @timeout its
// duration. A non-literal argument is kept as its source text.
func ReadContract(body string) (Contract, error) {
	var c Contract
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBashPP)).Parse(strings.NewReader(body), "")
	if err != nil {
		return c, err
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		var decs []*syntax.BashPPDecorator
		switch decl := node.(type) {
		case *syntax.FuncDecl:
			decs = decl.Decorators
		case *syntax.BashPPFuncDecl:
			decs = decl.Decorators
		default:
			return true
		}
		for _, d := range decs {
			if d == nil || d.Name == nil {
				continue
			}
			args := decoratorValues(body, d)
			switch d.Name.Value {
			case "require":
				c.Require = append(c.Require, args...)
			case "ensure":
				c.Ensure = append(c.Ensure, args...)
			case "effects":
				c.Effects = append(c.Effects, atoms(args)...)
			case "guard":
				c.Guard = append(c.Guard, atoms(args)...)
			case "confirm":
				if len(args) == 0 {
					args = []string{"on"}
				}
				c.Confirm = append(c.Confirm, args...)
			case "timeout":
				if len(args) > 0 {
					c.Timeout = args[0]
				}
			}
		}
		return true
	})
	return c, nil
}

// decoratorValues returns a decorator's argument values, positional and
// keyword alike (the keyword names a slot; the value is the contract text).
func decoratorValues(src string, d *syntax.BashPPDecorator) []string {
	var out []string
	for _, w := range d.Args {
		if w == nil {
			continue
		}
		v, ok := literalWord(w)
		if !ok {
			start, end := int(w.Pos().Offset()), int(w.End().Offset())
			if start >= 0 && end <= len(src) && start <= end {
				v = src[start:end]
			}
		}
		out = append(out, v)
	}
	return out
}

func literalWord(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				lit, ok := q.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(lit.Value)
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

func atoms(args []string) []string {
	var out []string
	for _, a := range args {
		out = append(out, strings.FieldsFunc(a, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })...)
	}
	return out
}
