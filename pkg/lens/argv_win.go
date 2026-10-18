package lens

// The Windows command-line dialects. Both are pure text transforms that run
// on every host; the real cmd.exe and pwsh gates run on a Windows machine
// (TestWindowsTruth, opt-in).
//
// cmd.exe: a program's argv on Windows is one string the C runtime splits
// (CommandLineToArgvW rules), and cmd.exe pre-processes that string — ^ is
// its escape, " toggles its quote mode, & | < > ( ) separate commands, % and
// ! expand variables. The render is the C-runtime quoting of each word
// followed by a caret before EVERY cmd metacharacter including the quotes
// themselves, so cmd never enters quote mode, strips the carets, and hands
// the program exactly the C-runtime line. ^% works because the caret splits
// a would-be variable name (%PATH^% names no variable) and is then removed.
// A newline cannot be carried: cmd ends the line there. That is ExitInvalid.
//
// pwsh: every word is a single-quoted PowerShell string ('' escapes a quote;
// nothing else is special, newlines included) and the line starts with the
// call operator so a quoted program name is invoked, not echoed:
// & 'prog' 'arg one' 'arg''two'. How pwsh then passes the words to a native
// program is its own contract ($PSNativeCommandArgumentPassing); 7.3+ passes
// them verbatim, older releases drop empty arguments.

import (
	"fmt"
	"strings"
)

// ---- cmd.exe -------------------------------------------------------------------

const cmdMeta = "()%!^\"<>&|"

func renderCmd(argv []string) ([]byte, error) {
	if err := validUTF8(argv); err != nil {
		return nil, err
	}
	words := make([]string, len(argv))
	for i, a := range argv {
		if strings.ContainsAny(a, "\n\r") {
			return nil, errorf(ExitInvalid, "argument %d has a line break: a cmd.exe command line cannot carry one", i)
		}
		words[i] = caretEscape(crtQuote(a))
	}
	return []byte(strings.Join(words, " ") + "\n"), nil
}

// crtQuote spells one argument so the C runtime's parser reads it back:
// nothing to do without space, tab or quote; otherwise wrap in quotes,
// escape each quote as \" and double the backslashes that precede a quote
// or the end.
func crtQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			slashes++
			continue
		case '"':
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
			b.WriteByte('"')
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
			b.WriteByte(c)
		}
		slashes = 0
	}
	b.WriteString(strings.Repeat(`\`, 2*slashes))
	b.WriteByte('"')
	return b.String()
}

func caretEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(cmdMeta, s[i]) >= 0 {
			b.WriteByte('^')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseCmd reads one cmd.exe command line: first cmd's own pass (carets and
// quote mode, with an unescaped command separator or redirection refused),
// then the C runtime's split.
func parseCmd(line string) ([]string, error) {
	line = strings.TrimRight(line, "\r\n")
	if strings.ContainsAny(line, "\n\r") {
		return nil, errorf(ExitInvalid, "invalid cmd input: want exactly one command line")
	}
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inQuote {
			if c == '"' {
				inQuote = false
			}
			b.WriteByte(c)
			continue
		}
		switch c {
		case '^':
			if i+1 < len(line) {
				i++
				b.WriteByte(line[i])
			}
		case '"':
			inQuote = true
			b.WriteByte(c)
		case '&', '|', '<', '>', '(', ')':
			return nil, errorf(ExitInvalid, "invalid cmd input: %q outside quotes starts another command or a redirection; lens reads one simple command", string(c))
		default:
			b.WriteByte(c)
		}
	}
	return crtSplit(b.String()), nil
}

// crtSplit is the C runtime's command-line split: whitespace separates words
// outside quotes; 2n backslashes before a quote are n backslashes and the
// quote toggles quoting; 2n+1 are n backslashes and a literal quote; inside
// quotes "" is a literal quote; backslashes elsewhere are literal.
func crtSplit(s string) []string {
	var args []string
	var b strings.Builder
	have, inQuote, slashes := false, false, 0
	flush := func(n int) {
		b.WriteString(strings.Repeat(`\`, n))
		slashes = 0
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			slashes++
			continue
		case '"':
			odd := slashes%2 == 1
			flush(slashes / 2)
			if odd {
				b.WriteByte('"')
			} else if inQuote && i+1 < len(s) && s[i+1] == '"' {
				b.WriteByte('"')
				i++
			} else {
				inQuote = !inQuote
			}
			have = true
			slashes = 0
			continue
		case ' ', '\t':
			if !inQuote {
				flush(slashes)
				if have {
					args = append(args, b.String())
					b.Reset()
					have = false
				}
				continue
			}
		}
		flush(slashes)
		b.WriteByte(c)
		have = true
	}
	flush(slashes)
	if have {
		args = append(args, b.String())
	}
	return args
}

// ---- PowerShell ------------------------------------------------------------------

func renderPwsh(argv []string) string {
	words := make([]string, 0, len(argv)+1)
	words = append(words, "&")
	for _, a := range argv {
		if a == "--%" {
			words = append(words, pwshStopParsing)
			continue
		}
		words = append(words, "'"+strings.ReplaceAll(a, "'", "''")+"'")
	}
	return strings.Join(words, " ")
}

// pwshStopParsing carries a literal "--%" argument. pwsh's native binder
// swallows any string argument equal to "--%" and passes every later argument
// verbatim (unquoted); a non-string object whose text is "--%" is not that
// marker, so the argument and its successors are bound normally.
const pwshStopParsing = "([System.Text.StringBuilder]'--%')"

// pwshEscapes are the backtick escapes PowerShell knows inside double quotes
// and bare words.
var pwshEscapes = map[byte]byte{
	'0': 0, 'a': '\a', 'b': '\b', 'e': 0x1b, 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t', 'v': '\v',
}

// parsePwsh reads one PowerShell command line of literal words: an optional
// leading call operator, then single-quoted, double-quoted (backtick escapes,
// no $ expansion) and bare pieces, adjacent pieces concatenating into one
// word. Anything PowerShell would evaluate is refused.
func parsePwsh(line string) ([]string, error) {
	if err := validUTF8([]string{line}); err != nil {
		return nil, errorf(ExitInvalid, "invalid pwsh input: not UTF-8")
	}
	var argv []string
	var b strings.Builder
	have := false
	end := func() {
		if have {
			argv = append(argv, b.String())
			b.Reset()
			have = false
		}
	}
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case c == ' ' || c == '\t':
			end()
			i++
		case c == '\n' || c == '\r':
			end()
			if strings.TrimSpace(line[i:]) != "" {
				return nil, errorf(ExitInvalid, "invalid pwsh input: want exactly one command line")
			}
			i = len(line)
		case c == '\'':
			j := i + 1
			for {
				k := strings.IndexByte(line[j:], '\'')
				if k < 0 {
					return nil, errorf(ExitInvalid, "invalid pwsh input: unterminated single quote")
				}
				b.WriteString(line[j : j+k])
				j += k + 1
				if j < len(line) && line[j] == '\'' {
					b.WriteByte('\'')
					j++
					continue
				}
				break
			}
			have = true
			i = j
		case c == '"':
			j := i + 1
			for {
				if j >= len(line) {
					return nil, errorf(ExitInvalid, "invalid pwsh input: unterminated double quote")
				}
				d := line[j]
				switch {
				case d == '"' && j+1 < len(line) && line[j+1] == '"':
					b.WriteByte('"')
					j += 2
					continue
				case d == '"':
					j++
				case d == '`' && j+1 < len(line):
					j = pwshEscape(&b, line, j)
					continue
				case d == '$':
					return nil, errorf(ExitInvalid, "invalid pwsh input: $ inside double quotes expands; use single quotes for a literal")
				default:
					b.WriteByte(d)
					j++
					continue
				}
				break
			}
			have = true
			i = j
		case c == '&' && !have && len(argv) == 0 && (i+1 == len(line) || line[i+1] == ' ' || line[i+1] == '\t'):
			i++ // the call operator
		case c == '(' && !have && strings.HasPrefix(line[i:], pwshStopParsing) &&
			(i+len(pwshStopParsing) == len(line) || strings.IndexByte(" \t\r\n", line[i+len(pwshStopParsing)]) >= 0):
			b.WriteString("--%")
			have = true
			i += len(pwshStopParsing)
		case strings.IndexByte(";|&(){}<>,@$#", c) >= 0:
			return nil, errorf(ExitInvalid, "invalid pwsh input: unquoted %q is PowerShell syntax, not a literal word", string(c))
		case c == '`':
			if i+1 >= len(line) {
				return nil, errorf(ExitInvalid, "invalid pwsh input: trailing backtick")
			}
			i = pwshEscape(&b, line, i)
			have = true
		default:
			if !have && strings.HasPrefix(line[i:], "--%") {
				return nil, errorf(ExitInvalid, "invalid pwsh input: --%% hands the rest to the native parser; lens reads literal words")
			}
			b.WriteByte(c)
			have = true
			i++
		}
	}
	end()
	return argv, nil
}

// pwshEscape decodes the backtick escape at s[i] and returns the index after it.
func pwshEscape(b *strings.Builder, s string, i int) int {
	e := s[i+1]
	if v, ok := pwshEscapes[e]; ok {
		b.WriteByte(v)
	} else {
		b.WriteByte(e) // `" `' `` `$ and any other char: itself
	}
	return i + 2
}

// cmdLineFor is the exact line a Windows gate runs for a rendered cmd row.
func cmdLineFor(rendered string) string {
	return fmt.Sprintf("cmd.exe /d /c %s", strings.TrimSuffix(rendered, "\n"))
}
