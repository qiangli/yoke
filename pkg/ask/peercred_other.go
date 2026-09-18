//go:build !windows && !darwin && !linux

package ask

import "fmt"

// peerUIDFromFD has no implementation on this platform, so peer authorization
// FAILS CLOSED and the socket channel is effectively unavailable — callers fall
// back to the file channel, which is protected by directory ownership instead.
//
// Returning an error rather than a permissive stub is the whole point: a security
// check that quietly succeeds where it is not implemented is worse than one that
// is absent, because the code above it believes it ran.
func peerUIDFromFD(uintptr) (int, error) {
	return -1, fmt.Errorf("peer credentials are not available on this platform")
}
