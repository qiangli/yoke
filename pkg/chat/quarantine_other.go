//go:build !darwin && !windows

package chat

// stripQuarantine is a no-op on Linux/unix: there is no Gatekeeper quarantine
// or Mark-of-the-Web equivalent — a downloaded binary with +x just runs.
func stripQuarantine(string) {}
