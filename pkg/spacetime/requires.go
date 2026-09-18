package spacetime

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The requires mini-grammar (pinned in the umbrella skills-mechanism
// doc): space-separated clauses AND-ed; commas inside a clause are
// any-of; repeating a clause key is AND; `key>=ver` is version-at-least
// (any probe key); a bare word is a boolean probe. All positive — no
// negation.
//
//	os=linux,darwin has=git has=claude,codex go>=1.26 tty

type Op int

const (
	OpAnyOf   Op = iota // key=v1,v2
	OpAtLeast           // key>=ver
	OpBool              // bare word
)

type Clause struct {
	Key    string
	Op     Op
	Values []string
}

type Requires struct{ Clauses []Clause }

var wordRe = regexp.MustCompile(`^[a-z][a-z0-9._-]*$`)
var valueRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// ParseRequires parses a metadata.requires string. It errors loudly on
// syntax it does not understand — callers decide whether to degrade
// (the skills catalog treats an unparsable requires as advisory-unchecked,
// it never hides the skill).
func ParseRequires(s string) (Requires, error) {
	var r Requires
	for _, tok := range strings.Fields(s) {
		switch {
		case strings.Contains(tok, ">="):
			key, ver, _ := strings.Cut(tok, ">=")
			if !wordRe.MatchString(key) || ver == "" {
				return Requires{}, fmt.Errorf("spacetime: bad requires clause %q", tok)
			}
			r.Clauses = append(r.Clauses, Clause{Key: key, Op: OpAtLeast, Values: []string{ver}})
		case strings.Contains(tok, "="):
			key, vals, _ := strings.Cut(tok, "=")
			if !wordRe.MatchString(key) || vals == "" {
				return Requires{}, fmt.Errorf("spacetime: bad requires clause %q", tok)
			}
			var vs []string
			for _, v := range strings.Split(vals, ",") {
				if !valueRe.MatchString(v) {
					return Requires{}, fmt.Errorf("spacetime: bad requires value %q in %q", v, tok)
				}
				vs = append(vs, v)
			}
			r.Clauses = append(r.Clauses, Clause{Key: key, Op: OpAnyOf, Values: vs})
		default:
			if !wordRe.MatchString(tok) {
				return Requires{}, fmt.Errorf("spacetime: bad requires clause %q", tok)
			}
			r.Clauses = append(r.Clauses, Clause{Key: tok, Op: OpBool})
		}
	}
	return r, nil
}

// coreProbes are the always-on facts addressable by their bare name.
// Everything else without a dot resolves through the tool namespace.
var coreProbes = map[string]bool{
	"os": true, "arch": true, "libc": true, "container": true,
	"tty": true, "elevated": true, "bashy": true,
}

// probeFor maps a clause key to the probe it reads: an exact dotted
// probe name is used as-is; the known core probes by name; anything
// else resolves through the tool namespace (`go` → `tool.go`).
func probeFor(key string) string {
	if strings.Contains(key, ".") {
		return key
	}
	if coreProbes[key] {
		return key
	}
	return "tool." + key
}

// ProbeRefs returns the probe names this Requires reads — the relevant
// probe subset for context keying (callers union {os, arch}).
//
// This subset, not the whole probe set, is what ContextKey is computed
// over. Widening it fragments every cache keyed by the result: an entry
// that never mentions `net.*` must not re-key when the host roams.
func (r Requires) ProbeRefs() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, c := range r.Clauses {
		if c.Key == "has" {
			for _, v := range c.Values {
				add("tool." + v)
			}
			continue
		}
		add(probeFor(c.Key))
	}
	return out
}

// Verdict is the applicability result for one entry at this coordinate.
type Verdict struct {
	Applicable bool
	Failing    string // first failing clause, e.g. "go>=1.26: tool.go=1.24"
	Unchecked  string // advisory-only compatibility that was not machine-gated
}

// Eval checks every clause against the probes. Short-circuits on the
// first failing clause.
func (r Requires) Eval(ps *ProbeSet) Verdict {
	for _, c := range r.Clauses {
		if fail := c.eval(ps); fail != "" {
			return Verdict{Applicable: false, Failing: fail}
		}
	}
	return Verdict{Applicable: true}
}

func (c Clause) eval(ps *ProbeSet) string {
	switch c.Op {
	case OpBool:
		p := probeFor(c.Key)
		v, _ := ps.Value(p)
		if v != "true" {
			return fmt.Sprintf("%s: %s=%s", c.Key, p, orAbsent(v))
		}
	case OpAnyOf:
		if c.Key == "has" {
			for _, t := range c.Values {
				if v, ok := ps.Value("tool." + t); ok && v != "absent" {
					return ""
				}
			}
			return fmt.Sprintf("has=%s: absent", strings.Join(c.Values, ","))
		}
		p := probeFor(c.Key)
		v, _ := ps.Value(p)
		for _, want := range c.Values {
			if v == want {
				return ""
			}
		}
		return fmt.Sprintf("%s=%s: %s=%s", c.Key, strings.Join(c.Values, ","), p, orAbsent(v))
	case OpAtLeast:
		p := probeFor(c.Key)
		v, ok := ps.Value(p)
		if !ok || !versionAtLeast(v, c.Values[0]) {
			return fmt.Sprintf("%s>=%s: %s=%s", c.Key, c.Values[0], p, orAbsent(v))
		}
	}
	return ""
}

func orAbsent(v string) string {
	if v == "" {
		return "absent"
	}
	return v
}

// versionAtLeast compares best-effort dotted numerals ("1.26" < "1.26.2");
// missing segments are zero; non-numeric values ("present", "absent")
// fail. No semver pre-release semantics.
func versionAtLeast(have, want string) bool {
	hs, ws := strings.Split(have, "."), strings.Split(want, ".")
	n := len(hs)
	if len(ws) > n {
		n = len(ws)
	}
	for i := 0; i < n; i++ {
		h, err := segAt(hs, i)
		if err != nil {
			return false
		}
		w, err := segAt(ws, i)
		if err != nil {
			return false
		}
		if h != w {
			return h > w
		}
	}
	return true
}

func segAt(segs []string, i int) (int, error) {
	if i >= len(segs) {
		return 0, nil
	}
	return strconv.Atoi(segs[i])
}
