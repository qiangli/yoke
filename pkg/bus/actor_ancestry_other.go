//go:build !linux && !darwin && !windows

package bus

// Unsupported targets fail closed rather than treating an unproved process as
// the owner of a registered communication identity.
func parentProcessID(int) (int, bool) { return 0, false }
