package sensitive

import (
	"regexp"
	"strings"
	"testing"
)

func TestClipboardShapeMatchesNativePattern(t *testing.T) {
	for _, tc := range []struct {
		name, kind, value string
		want              bool
	}{
		{"SSN", "text", "123-45-6789", true},
		{"SSN", "text", "wrong", false},
		{"Routing number", "text", "123456789", true},
		{"Routing number", "text", "12345678", false},
		{"One-time code", "secret", "123456", true},
		{"IBAN", "text", "GB82WEST12345698765432", true},
		{"Unclear field", "text", "anything", false},
	} {
		category := ClipboardCategory(tc.name, tc.kind)
		got, reason := ClipboardShape(category, tc.value)
		if got != tc.want || strings.Contains(reason, tc.value) {
			t.Fatalf("%s: got %v, reason %q", tc.name, got, reason)
		}
		pattern, known := ClipboardPattern(category)
		if known && regexp.MustCompile(pattern).MatchString(tc.value) != got {
			t.Fatalf("%s: native preview differs from Go check", tc.name)
		}
	}
	if ok, reason := ClipboardShape("ssn", ""); ok || reason != "clipboard is empty" {
		t.Fatalf("empty clipboard: %v, %q", ok, reason)
	}
}
