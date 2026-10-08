//go:build !darwin && !linux

package broker

import "errors"

// peerCredentialsSupported is false here: without kernel peer credentials
// the door does not open a unix listener at all, so callers use the
// token-authenticated TCP path instead of a socket that would refuse them.
const peerCredentialsSupported = false

func peerUID(uintptr) (int, error) { return -1, errors.New("unix peer credentials unsupported") }
