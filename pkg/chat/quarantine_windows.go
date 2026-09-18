//go:build windows

package chat

import "os"

// stripQuarantine removes the Windows Mark-of-the-Web from an operator-configured
// agent binary so a background/CI launch doesn't hang on a SmartScreen "Windows
// protected your PC" popup — the Gatekeeper-quarantine analog. MOTW lives in the
// `Zone.Identifier` alternate data stream, addressable as `<path>:Zone.Identifier`;
// deleting that stream is the programmatic equivalent of `Unblock-File`.
// Best-effort: a missing stream (the common case) just returns an error we ignore.
func stripQuarantine(path string) {
	_ = os.Remove(path + ":Zone.Identifier")
}
