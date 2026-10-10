//go:build !windows && !linux && !darwin

package room

// A Unix with no cheap per-process state answer keeps the signal-0 verdict:
// guessing "exited" without being told would trade the dead-holder loop for
// evicting live members, which is the worse direction for a claim to fail.
func processZombie(pid int) bool { return false }

// pidStart has nothing to report here, so cards written on this platform
// carry no start time and cardAlive skips the identity proof rather than
// fencing on nothing.
func pidStart(pid int) (string, bool) { return "", false }
