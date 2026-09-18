package meet

import "testing"

// Under a pty a tool resets the terminal as it exits. Those resets are NOT plain
// CSI colour codes — they carry private-mode markers (`\x1b[>4m`, `\x1b[<u`) and
// two-char forms (`\x1b7`, `\x1b(B`) — and an ANSI pattern that only knows about
// `[0-9;?]` params leaves them behind. The tail of every claude turn was being
// recorded as the literal text `(B[>4m[<u78`.
func TestSanitizeStripsPTYResetResidue(t *testing.T) {
	raw := "OK\n\x1b[?1006l\x1b[?25h\x1b(B\x1b[>4m\x1b[<u\x1b7\x1b8"
	if got := sanitizeTurn(raw); got != "OK" {
		t.Errorf("sanitizeTurn = %q, want %q — terminal reset residue leaked into the turn", got, "OK")
	}
}

// The ordinary cases must keep working: colour codes go, real prose stays.
func TestSanitizeKeepsProse(t *testing.T) {
	if got := sanitizeTurn("\x1b[32mwrite-through\x1b[0m is safer — 90% of the time"); got != "write-through is safer — 90% of the time" {
		t.Errorf("sanitizeTurn mangled prose: %q", got)
	}
}
