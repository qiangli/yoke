package chat

import (
	"regexp"
	"strings"
)

// ansiEscape matches the escape sequences that leak into an agent CLI's captured
// output.
//
// Three alternatives, and the first two both matter:
//
//   - CSI — `\x1b[ … final`. The parameter class INCLUDES the private markers
//     `<>=?`. Under a pty a tool resets the terminal on exit and emits `\x1b[>4m`
//     and `\x1b[<u`; without the private markers those do not match, and the tail
//     of every claude turn gets recorded as literal `(B[>4m[<u78`.
//   - OSC — `\x1b] … BEL|ST`.
//   - Two-character escapes — `\x1b7`, `\x1b8` (save/restore cursor) and `\x1b(B`
//     (charset select). Not CSI at all, so nothing else catches them.
var ansiEscape = regexp.MustCompile(
	"\x1b\\[[0-9;?<>=]*[ -/]*[@-~]" + // CSI, incl. private-mode params
		"|\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)" + // OSC
		"|\x1b[()][0-9A-Za-z]" + // charset select: ESC ( B
		"|\x1b[0-9A-Za-z><=]") // two-char: ESC 7, ESC 8, ESC =

var (
	runsOfSpace = regexp.MustCompile(`[ \t]{2,}`)
	runsOfBlank = regexp.MustCompile(`\n{3,}`)
)

// SanitizeTurn makes captured agent output safe to STORE and to REPLAY as prompt
// context.
//
// It lives here, next to Session, because it is the price of the pty: a terminal
// has ONE stream, so a tool's banners, spinners and cursor gymnastics land in the
// same bytes as its answer — sometimes as invalid UTF-8 (a truncated box-drawing
// glyph). Fed back verbatim as the next agent's argv, those crash downstream
// tools outright: codex rejects "invalid UTF-8 in arguments"; aider throws
// UnicodeEncodeError writing its input history.
//
// So: coerce to valid UTF-8, strip ANSI escapes and C0/C1 control chars (keeping
// \n and \t), drop the box-drawing chrome, and collapse the blank runs left
// behind. Ordinary prose survives, including legitimate non-ASCII.
//
// Every consumer of a pty turn must run this — a meeting's minutes, a foreman's
// history, a weave attribution. They used to each have their own copy, which is
// how meet's version got the private-mode CSI fix and nobody else did.
func SanitizeTurn(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = ansiEscape.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || (r >= 0x7f && r < 0xa0): // C0/C1 control
			return -1
		case r >= 0xd800 && r <= 0xdfff: // surrogates (never valid in UTF-8 argv)
			return -1
		case r >= 0x2500 && r <= 0x259f: // box-drawing + block elements — CLI banner chrome
			return -1
		case r == 0xfffd: // replacement char from an earlier lossy decode
			return -1
		default:
			return r
		}
	}, s)
	s = runsOfSpace.ReplaceAllString(s, " ")
	s = runsOfBlank.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// SanitizeLine is SanitizeTurn for a single live line, so a watcher tailing a
// session and the transcript that records it never disagree about what was said.
func SanitizeLine(s string) string {
	return SanitizeTurn(s)
}
