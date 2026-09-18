package meet

import (
	"testing"
	"unicode/utf8"
)

func TestSanitizeTurn(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"ansi + collapse", "\x1b[0m > build \x1b[0m ok", "> build ok"},
		{"control chars", "a\x07b\x00c\td\ne", "abc\td\ne"},
		{"invalid utf8 dropped", "hi\xff\xfeworld", "hiworld"},
		{"truncated boxdrawing bytes", "line\xe2\x94end", "lineend"},
		{"box-drawing banner stripped", "line ──────── end", "line end"},
		{"legit unicode kept", "cost ≠ parity → fix", "cost ≠ parity → fix"},
		{"trim", "  spaced  ", "spaced"},
	}
	for _, c := range cases {
		if got := sanitizeTurn(c.in); got != c.want {
			t.Errorf("%s: sanitizeTurn(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
	// Load-bearing property: output is always valid UTF-8 and surrogate-free
	// (the argv-safety guarantee for codex/aider).
	for _, c := range cases {
		out := sanitizeTurn(c.in)
		if !utf8.ValidString(out) {
			t.Errorf("%s: output not valid UTF-8: %q", c.name, out)
		}
	}
}
