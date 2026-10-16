package nft

import (
	"fmt"
	"strconv"
	"strings"
)

type tokKind int

const (
	tWord tokKind = iota
	tString
	tPunct // { } ; , @
	tEOL
)

type token struct {
	kind tokKind
	val  string
	line int
}

func (t token) is(s string) bool { return t.kind != tString && t.val == s }

func (t token) String() string {
	if t.kind == tString {
		return strconv.Quote(t.val)
	}
	if t.kind == tEOL {
		return "end of line"
	}
	return t.val
}

// lex splits an nft script into tokens. Whitespace separates words;
// "{ } ; , @" are always single tokens; "..." is a string (Go/C escapes);
// "#" starts a comment; a newline ends a command.
func lex(src string) ([]token, error) {
	var toks []token
	line := 1
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == '\n':
			toks = append(toks, token{tEOL, "\n", line})
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '\\' && i+1 < len(src) && src[i+1] == '\n':
			line++
			i += 2
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '{' || c == '}' || c == ';' || c == ',' || c == '@':
			toks = append(toks, token{tPunct, string(c), line})
			i++
		case c == '"':
			j := i + 1
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' {
					j++
				}
				if j < len(src) && src[j] == '\n' {
					return nil, fmt.Errorf("line %d: unterminated string", line)
				}
				j++
			}
			if j >= len(src) {
				return nil, fmt.Errorf("line %d: unterminated string", line)
			}
			s, err := strconv.Unquote(src[i : j+1])
			if err != nil {
				return nil, fmt.Errorf("line %d: bad string %s: %v", line, src[i:j+1], err)
			}
			toks = append(toks, token{tString, s, line})
			i = j + 1
		default:
			j := i
			for j < len(src) && !strings.ContainsRune(" \t\r\n{};,@\"#", rune(src[j])) {
				j++
			}
			toks = append(toks, token{tWord, src[i:j], line})
			i = j
		}
	}
	return toks, nil
}

// splitCommands groups tokens into commands: a newline or ";" outside
// braces ends one.
func splitCommands(toks []token) ([][]token, error) {
	var cmds [][]token
	var cur []token
	depth := 0
	flush := func() {
		if len(cur) > 0 {
			cmds = append(cmds, cur)
			cur = nil
		}
	}
	for _, t := range toks {
		switch {
		case t.kind == tPunct && t.val == "{":
			depth++
			cur = append(cur, t)
		case t.kind == tPunct && t.val == "}":
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("line %d: unbalanced '}'", t.line)
			}
			cur = append(cur, t)
		case depth == 0 && (t.kind == tEOL || (t.kind == tPunct && t.val == ";")):
			flush()
		case t.kind == tEOL:
			// newlines inside braces are whitespace
		default:
			cur = append(cur, t)
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("unbalanced '{'")
	}
	flush()
	return cmds, nil
}
