package repair

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/qiangli/yoke/pkg/unit"
)

// decoratorNames is the Bash# contract vocabulary the unit-backed reader
// honours: the A1 pkg/unit reader's six decorators, positional and keyword
// forms alike.
var decoratorNames = map[string]bool{
	"require": true, "ensure": true, "guard": true,
	"effects": true, "confirm": true, "timeout": true,
}

// Function is one contracted function in a single Bash# body: a function
// definition carrying at least one contract decorator.
type Function struct {
	// Name is the shell function name.
	Name string
	// Unit is the slash-separated path of the file that declares it.
	Unit string
	// Decorators are the contract lines above the definition, in order and
	// trimmed. They are part of the contract digest: editing a
	// precondition or postcondition changes what was verified.
	Decorators []string
}

// BodyFunctions parses one Bash# body the unit way — through the normative
// sh/syntax grammar, so a decorator counts only where the shell would apply
// it, on a function declaration — and returns its contracted functions and
// its contract. ok is false when the body does not parse or states no
// checkable guarantee: the two cases the minimal line-based reader still
// covers as fallback.
func BodyFunctions(body, unitPath string) (funcs []Function, c unit.Contract, ok bool) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBashPP)).Parse(strings.NewReader(body), "")
	if err != nil {
		return nil, unit.Contract{}, false
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		var name *syntax.Lit
		var decs []*syntax.BashPPDecorator
		switch decl := node.(type) {
		case *syntax.FuncDecl:
			name, decs = decl.Name, decl.Decorators
		case *syntax.BashPPFuncDecl:
			name, decs = decl.Name, decl.Decorators
		default:
			return true
		}
		if name == nil {
			return true
		}
		var lines []string
		for _, d := range decs {
			if d == nil || d.Name == nil || !decoratorNames[d.Name.Value] {
				continue
			}
			lines = append(lines, strings.TrimSpace(decoratorSpan(body, d)))
		}
		if len(lines) > 0 {
			funcs = append(funcs, Function{Name: name.Value, Unit: unitPath, Decorators: lines})
		}
		return true
	})
	if len(funcs) == 0 {
		return nil, unit.Contract{}, false
	}
	c, err = unit.ReadContract(body)
	if err != nil || !c.Contracted() {
		return nil, unit.Contract{}, false
	}
	return funcs, c, true
}

// decoratorSpan is the decorator's source text. Single-line decorators (the
// norm) yield their trimmed line; a decorator split across lines yields its
// whole span, so nothing about the contract is silently dropped.
func decoratorSpan(src string, d *syntax.BashPPDecorator) string {
	start, end := int(d.Pos().Offset()), int(d.End().Offset())
	if start < 0 || end > len(src) || start >= end {
		return ""
	}
	return src[start:end]
}
