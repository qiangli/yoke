package secrets

import (
	"strings"
	"testing"
)

// TestOverlappingSecretsSuffixLeak: When two registered secret values overlap and the
// shorter one starts earlier, the leftmost-greedy replacement consumes the overlapping
// bytes but silently emits the suffix of the longer secret past the shorter match.
// The suffix of a registered secret value appears unredacted in output.
func TestOverlappingSecretsSuffixLeak(t *testing.T) {
	// shorter starts at pos 0, 16 bytes: "AAAAbbbbbbbbbbbb"
	// longer  starts at pos 4, 16 bytes: "bbbbbbbbbbbbCCCC"
	// Overlap on "bbbbbbbbbbbb" (positions 4-15).
	// After the shorter match at pos 0, longer's suffix "CCCC" at pos 16-19
	// falls outside any match and appears in plaintext.
	const shorter = "AAAAbbbbbbbbbbbb"
	const longer = "bbbbbbbbbbbbCCCC"

	r := NewRedactor()
	if err := r.Register("SHORT_SECRET", shorter); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("LONG_SECRET", longer); err != nil {
		t.Fatal(err)
	}

	// Input contains shorter followed by the suffix of longer:
	// "AAAAbbbbbbbbbbbbCCCC_suffix"
	input := shorter + longer[12:] + "_suffix"

	got := string(r.Redact([]byte(input)))

	// "CCCC" is the last 4 bytes of registered secret LONG_SECRET.
	// They leak into the output because the leftmost match (SHORT_SECRET at pos 0)
	// consumes positions 0-15, leaving positions 16+ unprotected.
	if strings.Contains(got, "CCCC") {
		t.Fatalf("suffix of registered secret LONG_SECRET leaked into output: %q", got)
	}
}
