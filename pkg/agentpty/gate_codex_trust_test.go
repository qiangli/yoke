package agentpty

import (
	"bytes"
	"testing"
)

// codexTrustDialog is codex-cli 0.156.1's folder-trust dialog as it reaches the
// PTY (measured 2026-09-24; path replaced). Every word is placed by its own
// cursor move, so no phrase exists in the raw bytes.
const codexTrustDialog = "\x1b[?2026h\x1b[1;1H\x1b[J\x1b[?25l\x1b[2;1H\x1b[1m  Folder access\x1b[3;3H\x1b[22m\x1b[2m\x1b[2m/work/repo  " +
	"\x1b[5;3H\x1b[22mTrust\x1b[5;9Hthis\x1b[5;14Hfolder?\x1b[5;22HCodex\x1b[5;28Hcan\x1b[5;32Hread,\x1b[5;38Hedit,\x1b[5;44Hand" +
	"\x1b[5;48Hrun\x1b[5;52Hfiles\x1b[5;58Hhere,\x1b[5;64Hsubject\x1b[5;72Hto\x1b[5;75Hyour\x1b[5;80Hpermission\x1b[5;91Hsettings." +
	"\x1b[6;61HContinue\x1b[6;70Honly\x1b[6;75Hif\x1b[6;78Hyou\x1b[6;82Htrust\x1b[6;88Hthese\x1b[6;94Hfiles.\x1b[6;101HYour" +
	"\x1b[6;106Htrust\x1b[7;3Hdecision\x1b[7;12Hwill\x1b[7;17Hbe\x1b[7;20Hsaved.\x1b[9;1H\x1b[7m\x1b[1m\xe2\x80\xba 1. Trust and continue" +
	"\x1b[10;3H2. Quit\x1b[?2026l"

// The dialog quit codex whenever an instruction arrived: the tap never matched
// it, because it matched raw bytes and codex never writes "trust this folder".
func TestClassifyGateMatchesCodexTrustDialogThroughANSI(t *testing.T) {
	v := ClassifyGate(codexTrustDialog)
	if v.Kind != GateTrust {
		t.Fatalf("ClassifyGate(codex 0.156.1 dialog) = %q, want %q; screen text: %q",
			v.Kind, GateTrust, ScreenText(codexTrustDialog))
	}
}

func TestScreenTextSeparatesCursorPlacedWords(t *testing.T) {
	got := ScreenText("\x1b[5;3HTrust\x1b[5;9Hthis\x1b[5;14Hfolder?\n\n  \x1b]0;title\x07next")
	if want := "Trust this folder? next"; got != want {
		t.Fatalf("ScreenText = %q, want %q", got, want)
	}
}

// The tap answers the dialog once, and tells the caller it did — the caller's
// readiness wait is still looking at the answered dialog in its own tail.
func TestTrustClearTapClearsCodexDialog(t *testing.T) {
	var said []string
	var routed []string
	var out bytes.Buffer
	tap := &trustClearTap{
		w: &out,
		deps: RouteDeps{State: &GateRouteState{}, Say: func(p string) error {
			said = append(said, p)
			return nil
		}},
		onRouted: func(_ GateVerdict, action string) { routed = append(routed, action) },
	}
	// Two writes, as a PTY read splits it; then a redraw of the same frame.
	half := len(codexTrustDialog) / 2
	for _, chunk := range []string{codexTrustDialog[:half], codexTrustDialog[half:], codexTrustDialog} {
		if _, err := tap.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if len(said) != 1 || said[0] != GateTrustClearPayload {
		t.Fatalf("said = %q, want exactly one %q", said, GateTrustClearPayload)
	}
	if len(routed) != 1 || routed[0] != "say_trust" {
		t.Fatalf("onRouted = %q, want one say_trust", routed)
	}
	if out.Len() != 2*len(codexTrustDialog) {
		t.Fatalf("tap must pass every byte through: %d", out.Len())
	}
}
