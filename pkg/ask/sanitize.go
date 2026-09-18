package ask

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// maxPromptRunes caps the agent-supplied prompt: small enough that it cannot push
// the frame off the screen, large enough for a real sentence. There is no separate
// line cap because line breaks are folded to spaces — the prompt cannot grow
// vertically at all.
const maxPromptRunes = 512

// ansiEscape matches the escape sequences that must never reach a terminal from
// caller-supplied text.
//
// Adapted from pkg/chat/sanitize.go, where the same expression was derived
// empirically from real agent output; copied rather than imported so pkg/ask does
// not inherit pkg/chat's dependency graph for four lines of regexp.
//
//   - CSI — ESC [ ... final. The parameter class includes the private markers
//     <>=? which tools emit on exit.
//   - OSC — ESC ] ... BEL|ST. This is the dangerous one here: OSC can retitle the
//     window and, on some terminals, drive the clipboard.
//   - Two-character escapes — ESC 7 / ESC 8 (save/restore cursor) and ESC ( B
//     (charset select). Not CSI, so nothing else catches them.
var ansiEscape = regexp.MustCompile(
	"\x1b\\[[0-9;?<>=]*[ -/]*[@-~]" +
		"|\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)" +
		"|\x1b[()][0-9A-Za-z]" +
		"|\x1b[0-9A-Za-z><=]")

// sanitizePrompt makes caller-supplied text safe to display inside the frame.
//
// This is the load-bearing anti-phishing control, and without it every other one
// is decorative. The prompt comes from the REQUESTING PROGRAM, which under an
// agentic harness may be acting on prompt-injected instructions. Given raw
// terminal control, that text can do far more than lie:
//
//   - a carriage return plus cursor movement lets it repaint lines ABOVE itself —
//     including bashy's own chrome — so it can forge a different requester, a
//     different sink, or a fake "verified by bashy" banner;
//   - newlines let it draw a complete second frame of its own;
//   - OSC sequences can retitle the terminal to impersonate another application.
//
// So control characters are removed rather than escaped (an escaped sequence still
// occupies the frame and still confuses), newlines are folded to spaces so the
// prompt cannot grow vertically, and the whole thing is capped. What survives is
// printable text with no ability to move the cursor.
func sanitizePrompt(s string) string {
	s = ansiEscape.ReplaceAllString(s, "")

	// Fold every line break to a space FIRST, so a multi-line prompt becomes one
	// line rather than losing its word boundaries.
	s = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(s)

	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteRune(' ')
		case isFrameGlyph(r):
			// The frame is drawn from box-drawing characters, and the caller has
			// no legitimate use for them in a message. Removing them means the
			// caller's text cannot LOOK like chrome even though — thanks to the
			// newline folding above — it can no longer be positioned like chrome
			// either. Defence in depth on the cheapest possible axis: a human
			// skimming the frame never sees a rule they did not draw.
		case r == unicode.ReplacementChar:
			// Invalid UTF-8 arrived; drop it rather than rendering a box glyph
			// that could be mistaken for frame chrome.
		case unicode.IsControl(r):
			// Includes C0 and C1. C1 matters: on a UTF-8 terminal these are the
			// single-byte forms of the same escapes stripped above.
		case !unicode.IsPrint(r) && !unicode.IsSpace(r):
		default:
			b.WriteRune(r)
		}
	}

	out := strings.TrimSpace(collapseSpaces(b.String()))
	if rs := []rune(out); len(rs) > maxPromptRunes {
		out = string(rs[:maxPromptRunes]) + "…"
	}
	return out
}

// isFrameGlyph reports whether a rune is one the frame itself is drawn with:
// Box Drawing (U+2500–U+257F) and Block Elements (U+2580–U+259F).
func isFrameGlyph(r rune) bool { return r >= 0x2500 && r <= 0x259F }

var runsOfSpace = regexp.MustCompile(`[ ]{2,}`)

func collapseSpaces(s string) string { return runsOfSpace.ReplaceAllString(s, " ") }

// namePattern constrains the label.
//
// The name is rendered inside the frame's chrome, unquoted, so unlike the prompt
// it is not merely sanitized but REJECTED when it does not match. A label is
// something the caller chooses from a small alphabet; there is no legitimate
// reason for one to contain a space, a control character, or a box-drawing glyph,
// and refusing is clearer than silently rewriting the caller's identifier.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func validateName(name string) error {
	if name == "" {
		return nil // optional; the frame just shows no label
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("ask: --name %q is not a valid label (letters, digits, _ . - only, up to 64 characters)", name)
	}
	return nil
}
