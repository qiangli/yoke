// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package advice

import (
	"path"
	"strings"
)

// Query describes one function registration: the join point advice can
// select on. These are names the engine already has — never a call site.
type Query struct {
	Name    string // the function's name
	File    string // the source file that defined it ("" when unknown)
	Agentic bool   // declared agentic (from the declaration, not the caller)

	// Preamble marks a function the host registered before user code (the
	// shell's own preamble). Rules skip these by default so a bare "*"
	// means the user's functions, not the substrate's.
	Preamble bool
}

// Advised is one decorator a rule set advises for a query, carrying the
// rule's stable ID so the applier can stay idempotent (never stack the same
// rule twice on re-registration) and Call.Advised can name its provenance.
type Advised struct {
	RuleID string
	Spec   Spec
}

// For returns the decorators advised for q, in file order — the
// deterministic application order (outermost first, per the add-only /
// outermost contract). The result is a fresh slice on every call; matching
// is pure, so equal queries always yield equal results. Nil-safe: a nil
// *Rules advises nothing.
func (r *Rules) For(q Query) []Advised {
	if r == nil {
		return nil
	}
	var out []Advised
	for _, rule := range r.rules {
		if rule.matches(q) {
			out = append(out, Advised{
				RuleID: rule.ID,
				Spec:   Spec{Decorator: rule.Spec.Decorator, Args: append([]Arg(nil), rule.Spec.Args...)},
			})
		}
	}
	return out
}

// matches reports whether every set selector matches q (AND semantics).
func (rule Rule) matches(q Query) bool {
	if q.Preamble && rule.ExcludePreamble {
		return false
	}
	if rule.Name != "" {
		if ok, _ := path.Match(rule.Name, q.Name); !ok {
			return false
		}
	}
	if rule.File != "" {
		// Patterns are written with forward slashes; normalize the
		// candidate so Windows paths match. path.Match semantics: the
		// pattern matches the whole path and '*' does not cross '/'.
		file := strings.ReplaceAll(q.File, "\\", "/")
		if ok, _ := path.Match(rule.File, file); !ok {
			return false
		}
	}
	if rule.Agentic != nil && *rule.Agentic != q.Agentic {
		return false
	}
	return true
}
