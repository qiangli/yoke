package ctty

import (
	"errors"
	"regexp"
	"testing"
)

// A dialog that went away WITHOUT an operator answer must never read as a
// decline.
//
// This is story f2c250c5f038 made concrete. In a headless managed session
// (no console user session, no window server for the process) osascript fails
// the whole dialog with error -128 — "User canceled" — within seconds and
// without any operator involved. The helper layer mapped -128 to CANCEL, the
// ladder in pkg/ask mapped CANCEL to "ask: cancelled", and the manager's
// request died as a cancel nobody made. Twice.
//
// The fix is definitional, not heuristic: a decline is an operator ACTION with
// positive evidence (the Cancel button). Everything else — the system closing
// the dialog, the window server refusing to show it, a keyboard gesture that
// carries no identity — is ErrDismissed: the rung failed, the human never
// answered, and the caller may fall back without re-prompting anyone.
func TestDismissedDialogIsNotADecline(t *testing.T) {
	if errors.Is(ErrDismissed, ErrDeclined) {
		t.Fatal("ErrDismissed wraps ErrDeclined — a system dismissal would read as an operator cancel")
	}
	if errors.Is(ErrDismissed, ErrTimeout) {
		t.Fatal("ErrDismissed wraps ErrTimeout — a system dismissal is not a shown dialog nobody answered")
	}
	if _, err := parseTaggedResult("osascript", "DISMISS\n"); !errors.Is(err, ErrDismissed) {
		t.Errorf("value dialog: error = %v, want ErrDismissed", err)
	}
	if _, err := parseReceiptResult("DISMISS\n"); !errors.Is(err, ErrDismissed) {
		t.Errorf("receipt dialog: error = %v, want ErrDismissed", err)
	}
}

// The -128 handler IS the incident, so the scripts' contract is pinned here:
// error -128 must report DISMISS, and only the Cancel button may report CANCEL.
// An untagged change to the AppleScript would otherwise reintroduce the
// auto-cancel silently.
func TestOsascriptMinus128ReportsDismissNotCancel(t *testing.T) {
	re := regexp.MustCompile(`on error number -128\r?\n\s*return "([A-Z]+)"`)
	for name, src := range map[string]string{"value dialog": osascriptSrc, "receipt dialog": receiptSrc} {
		m := re.FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("%s script lost its -128 handler — an undisplayable dialog would surface as a raw osascript error", name)
		}
		if m[1] != "DISMISS" {
			t.Errorf("%s script maps -128 to %q; -128 arrives with no operator attached (a headless session's window server refuses the dialog) and must be DISMISS", name, m[1])
		}
	}
}
