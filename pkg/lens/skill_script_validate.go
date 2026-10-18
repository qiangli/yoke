package lens

import (
	"bytes"

	"mvdan.cc/sh/v3/syntax"
)

// ValidatePureOutputScript is a standalone static gate over a shell script
// body (Sprint 446 S22): it parses script with the mvdan.cc/sh/v3/syntax AST
// parser and accepts only simple literal calls to echo, printf, true, false
// and pwd — one CallExpr per statement, a literal command name from that set,
// literal-only arguments, no assignments and no redirections. Everything else
// is refused fail-closed with an ExitInvalid error naming the unsupported
// construct and its line: redirections, command substitutions, parameter
// expansions, arithmetic expansions, process substitutions, globbing,
// pipelines and other operators, functions, loops, conditionals, blocks,
// subshells, assignments, unknown commands and syntax errors.
//
// It never executes anything: parsing and AST inspection only. An empty (or
// comment/blank-only) script is valid — it performs no effect.
//
// This is one narrow syntactic check, not the complete S22 effect policy; no
// claim is made that passing scripts are safe to run, only that they stay
// inside the pure-output shape above.
func ValidatePureOutputScript(script []byte) error {
	p := syntax.NewParser(syntax.Variant(syntax.LangBash))
	f, err := p.Parse(bytes.NewReader(script), "")
	if err != nil {
		return errorf(ExitInvalid, "invalid pure-output script: syntax error: %v", err)
	}
	for _, st := range f.Stmts {
		if err := checkPureStmt(st); err != nil {
			return err
		}
	}
	return nil
}

// pureOutputCmds are the only commands a pure-output script may call, each
// with literal arguments only.
var pureOutputCmds = map[string]bool{
	"echo": true, "printf": true, "true": true, "false": true, "pwd": true,
}

func checkPureStmt(st *syntax.Stmt) error {
	line := int(st.Pos().Line())
	switch {
	case st.Negated:
		return errorf(ExitInvalid, "line %d: negation (!) is not allowed in a pure-output script", line)
	case st.Background:
		return errorf(ExitInvalid, "line %d: background (&) jobs are not allowed in a pure-output script", line)
	case st.Coprocess:
		return errorf(ExitInvalid, "line %d: coprocesses are not allowed in a pure-output script", line)
	case len(st.Redirs) > 0:
		return errorf(ExitInvalid, "line %d: redirections are not allowed in a pure-output script", line)
	}
	switch cmd := st.Cmd.(type) {
	case *syntax.CallExpr:
		return checkPureCall(cmd, line)
	case *syntax.BinaryCmd:
		if cmd.Op == syntax.Pipe || cmd.Op == syntax.PipeAll {
			return errorf(ExitInvalid, "line %d: pipelines are not allowed in a pure-output script", line)
		}
		op := "operator"
		switch cmd.Op {
		case syntax.AndStmt:
			op = "&&"
		case syntax.OrStmt:
			op = "||"
		}
		return errorf(ExitInvalid, "line %d: %s operators are not allowed in a pure-output script", line, op)
	case *syntax.IfClause:
		return errorf(ExitInvalid, "line %d: if statements are not allowed in a pure-output script", line)
	case *syntax.ForClause:
		return errorf(ExitInvalid, "line %d: for loops are not allowed in a pure-output script", line)
	case *syntax.WhileClause:
		if cmd.Until {
			return errorf(ExitInvalid, "line %d: until loops are not allowed in a pure-output script", line)
		}
		return errorf(ExitInvalid, "line %d: while loops are not allowed in a pure-output script", line)
	case *syntax.CaseClause:
		return errorf(ExitInvalid, "line %d: case statements are not allowed in a pure-output script", line)
	case *syntax.FuncDecl:
		return errorf(ExitInvalid, "line %d: function definitions are not allowed in a pure-output script", line)
	case *syntax.Block:
		return errorf(ExitInvalid, "line %d: brace blocks are not allowed in a pure-output script", line)
	case *syntax.Subshell:
		return errorf(ExitInvalid, "line %d: subshells are not allowed in a pure-output script", line)
	case *syntax.ArithmCmd:
		return errorf(ExitInvalid, "line %d: arithmetic commands are not allowed in a pure-output script", line)
	case *syntax.TestClause:
		return errorf(ExitInvalid, "line %d: test commands are not allowed in a pure-output script", line)
	case *syntax.LetClause:
		return errorf(ExitInvalid, "line %d: let commands are not allowed in a pure-output script", line)
	case *syntax.TimeClause:
		return errorf(ExitInvalid, "line %d: time clauses are not allowed in a pure-output script", line)
	case *syntax.DeclClause:
		return errorf(ExitInvalid, "line %d: declarations are not allowed in a pure-output script", line)
	case *syntax.CoprocClause:
		return errorf(ExitInvalid, "line %d: coprocesses are not allowed in a pure-output script", line)
	case nil:
		return errorf(ExitInvalid, "line %d: empty commands are not allowed in a pure-output script", line)
	default:
		return errorf(ExitInvalid, "line %d: %T commands are not allowed in a pure-output script", line, cmd)
	}
}

func checkPureCall(call *syntax.CallExpr, line int) error {
	if len(call.Assigns) > 0 {
		return errorf(ExitInvalid, "line %d: assignments are not allowed in a pure-output script", line)
	}
	if len(call.Args) == 0 {
		return errorf(ExitInvalid, "line %d: empty commands are not allowed in a pure-output script", line)
	}
	nameWord := call.Args[0]
	name, err := literalWord(nameWord, syntax.LangBash)
	if err != nil {
		return errorf(ExitInvalid, "line %d: command name is not a literal: %v", int(nameWord.Pos().Line()), err)
	}
	if !pureOutputCmds[name] {
		return errorf(ExitInvalid, "line %d: unknown command %q: only echo, printf, true, false and pwd are allowed in a pure-output script", int(nameWord.Pos().Line()), name)
	}
	for _, w := range call.Args[1:] {
		if _, err := literalWord(w, syntax.LangBash); err != nil {
			return errorf(ExitInvalid, "line %d: %v", int(w.Pos().Line()), err)
		}
	}
	return nil
}
