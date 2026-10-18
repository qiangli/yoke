//go:build !linux && !darwin

package foreman

// ProcessStart has no start identity here, so Terminate never proves a pid.
func ProcessStart(int) (string, bool) { return "", false }
