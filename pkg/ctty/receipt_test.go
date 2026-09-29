package ctty

import (
	"errors"
	"strings"
	"testing"
)

// The receipt is the operator's only feedback that a paste into the hidden
// macOS field arrived intact, so it must say something useful — the length and
// the character classes present — and it must NEVER carry the value or any
// fragment of it. The test value's characters are chosen to be absent from
// every word the receipt is allowed to use, so a single leaked character fails.
func TestReceiptStatesLengthAndClassesOnly(t *testing.T) {
	const value = "XKQZ93$%" // 8 chars: letters, digits, symbols; none appear in receipt prose

	got := receiptLine([]byte(value))

	if !strings.Contains(got, "8") {
		t.Errorf("receipt does not state the length 8: %q", got)
	}
	for _, class := range []string{"letters", "digits", "symbols"} {
		if !strings.Contains(got, class) {
			t.Errorf("receipt does not name the class %q: %q", class, got)
		}
	}
	if strings.Contains(got, value) {
		t.Fatalf("receipt contains the value: %q", got)
	}
	for _, r := range value {
		if strings.ContainsRune(got, r) {
			t.Errorf("receipt leaked the character %q of the value: %q", r, got)
		}
	}
}

// Only the classes actually PRESENT are named — "letters, digits, symbols" on a
// digits-only value would tell the operator their paste was mangled when it
// was not.
func TestReceiptNamesOnlyPresentClasses(t *testing.T) {
	cases := []struct {
		value   string
		present []string
		absent  []string
	}{
		{"777777", []string{"digits"}, []string{"letters", "symbols", "spaces"}},
		{"QQQQ", []string{"letters"}, []string{"digits", "symbols", "spaces"}},
		{"##%%", []string{"symbols"}, []string{"letters", "digits", "spaces"}},
		{"Q 7", []string{"letters", "digits", "spaces"}, []string{"symbols"}},
	}
	for _, c := range cases {
		got := receiptLine([]byte(c.value))
		for _, class := range c.present {
			if !strings.Contains(got, class) {
				t.Errorf("receiptLine(%q) is missing %q: %q", c.value, class, got)
			}
		}
		for _, class := range c.absent {
			if strings.Contains(got, class) {
				t.Errorf("receiptLine(%q) claims %q: %q", c.value, class, got)
			}
		}
	}
}

// The length is in CHARACTERS, not bytes — the operator pasted characters and
// will compare against a character count.
func TestReceiptCountsRunesNotBytes(t *testing.T) {
	got := receiptLine([]byte("héllo")) // 5 runes, 6 bytes
	if !strings.Contains(got, "5") || strings.Contains(got, "6") {
		t.Errorf("receipt must count 5 characters, not 6 bytes: %q", got)
	}
}

// The receipt dialog's answer follows the same positive-evidence contract as
// the entry dialog: USE and RETRY are the two affirmative outcomes, CANCEL is a
// decline, TIMEOUT is a timeout, and anything else is an error — never a
// silent acceptance.
func TestParseReceiptResult(t *testing.T) {
	if retry, err := parseReceiptResult("USE\n"); err != nil || retry {
		t.Errorf("USE => retry=%v err=%v, want accept", retry, err)
	}
	if retry, err := parseReceiptResult("RETRY\n"); err != nil || !retry {
		t.Errorf("RETRY => retry=%v err=%v, want retry", retry, err)
	}
	if _, err := parseReceiptResult("CANCEL\n"); !errors.Is(err, ErrDeclined) {
		t.Errorf("CANCEL => %v, want ErrDeclined", err)
	}
	if _, err := parseReceiptResult("TIMEOUT\n"); !errors.Is(err, ErrTimeout) {
		t.Errorf("TIMEOUT => %v, want ErrTimeout", err)
	}
	if _, err := parseReceiptResult("something else"); err == nil {
		t.Error("an unrecognised result was accepted")
	}
}
