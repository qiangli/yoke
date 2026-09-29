package ctty

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// receiptLine describes a just-received secret WITHOUT disclosing it: the
// length in characters and the character classes present, nothing else.
//
// It exists because the macOS hidden-answer field gives misleading feedback —
// a 71-character paste renders as ONE dot, and a Backspace then shows the box
// full of them — so the operator cannot tell whether the paste arrived. The
// receipt is what they read instead, which is why it must be informative
// enough to catch a truncated paste (the length) and a mangled one (a missing
// class), and why it must NEVER carry the value: no prefix, no suffix, no
// sample characters. It is shown in a dialog, and dialogs get screenshotted,
// screen-shared, and read aloud.
//
// Pure — no GUI, no I/O — so the never-leaks property is testable without a
// screen.
func receiptLine(value []byte) string {
	var letters, digits, spaces, symbols bool
	for _, r := range string(value) {
		switch {
		case unicode.IsLetter(r):
			letters = true
		case unicode.IsDigit(r):
			digits = true
		case unicode.IsSpace(r):
			spaces = true
		default:
			symbols = true
		}
	}
	var classes []string
	if letters {
		classes = append(classes, "letters")
	}
	if digits {
		classes = append(classes, "digits")
	}
	if symbols {
		classes = append(classes, "symbols")
	}
	if spaces {
		classes = append(classes, "spaces")
	}

	n := utf8.RuneCount(value)
	noun := "characters"
	if n == 1 {
		noun = "character"
	}
	if len(classes) == 0 {
		return fmt.Sprintf("Received %d %s.", n, noun)
	}
	return fmt.Sprintf("Received %d %s (%s).", n, noun, strings.Join(classes, ", "))
}

// receiptSrc shows the receipt and asks the operator to accept or retry. Same
// parameterised-argv rule as osascriptSrc, and the same positive-evidence
// contract: `giving up after` still exits 0, so the tag is what carries the
// outcome.
const receiptSrc = `on run argv
	set theText to item 1 of argv
	set theTitle to item 2 of argv
	set tmo to (item 3 of argv) as integer
	try
		set r to display dialog theText with title theTitle buttons {"Retry", "Use"} default button "Use" with icon caution giving up after tmo
	on error number -128
		return "CANCEL"
	end try
	if gave up of r then return "TIMEOUT"
	if button returned of r is "Retry" then return "RETRY"
	return "USE"
end run`

// parseReceiptResult reads the receipt dialog's USE/RETRY/CANCEL/TIMEOUT
// contract. Mirrors parseTaggedResult: an unrecognised line is an error, never
// a silent acceptance of a value the operator did not confirm.
func parseReceiptResult(raw string) (retry bool, err error) {
	switch strings.TrimRight(raw, "\r\n") {
	case "USE":
		return false, nil
	case "RETRY":
		return true, nil
	case "CANCEL":
		return false, ErrDeclined
	case "TIMEOUT":
		return false, ErrTimeout
	default:
		return false, fmt.Errorf("ctty: the receipt dialog returned an unrecognised result %q", raw)
	}
}
