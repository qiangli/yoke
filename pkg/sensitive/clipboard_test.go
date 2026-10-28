package sensitive

import (
	"regexp"
	"strings"
	"testing"
)

func TestClipboardShapeIsAuthoritativeOverNativePattern(t *testing.T) {
	for _, tc := range []struct {
		name, kind, value string
		want              bool
	}{
		{"SSN", "text", "123-45-6789", true},
		{"SSN", "text", "wrong", false},
		{"Routing number", "text", "123456789", false}, // syntax ok, checksum fails
		{"Routing number", "text", "021000021", true},
		{"Routing number", "text", "12345678", false},
		{"Card number", "text", "4111 1111 1111 1111", true},
		{"Card number", "text", "4111 1111 1111 1112", false}, // Luhn
		{"EIN", "text", "94-1234567", true},
		{"EIN", "text", "07-1234567", false}, // unassigned prefix
		{"ITIN", "text", "912-70-1234", true},
		{"ITIN", "text", "912-40-1234", false}, // group outside the ITIN ranges
		{"SSN", "text", "000-12-3456", false},
		{"IBAN", "text", "GB82WEST12345698765431", false}, // mod-97
		{"One-time code", "secret", "123456", true},
		{"IBAN", "text", "GB82WEST12345698765432", true},
		{"Unclear field", "text", "anything", false},
		// Word boundaries: "being" is not an EIN field, "provide" not an ID.
		{"Password — being careful to provide it", "secret", "hunter2!", true},
	} {
		category := ClipboardCategory(tc.name, tc.kind)
		got, reason := ClipboardShape(category, tc.value)
		if got != tc.want || strings.Contains(reason, tc.value) {
			t.Fatalf("%s: got %v, reason %q", tc.name, got, reason)
		}
		// The native preview is a pre-filter: a regex miss must be a Go
		// miss (the button stays disabled for nothing Go would accept);
		// a regex hit may still fail the Go checksum.
		pattern, known := ClipboardPattern(category)
		if known && !regexp.MustCompile(pattern).MatchString(tc.value) && got {
			t.Fatalf("%s: native preview rejects what the Go check accepts", tc.name)
		}
	}
	if ok, reason := ClipboardShape("ssn", ""); ok || reason != "clipboard is empty" {
		t.Fatalf("empty clipboard: %v, %q", ok, reason)
	}
}
