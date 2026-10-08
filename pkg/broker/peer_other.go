//go:build !darwin && !linux

package broker

import "errors"

func peerUID(uintptr) (int, error) { return -1, errors.New("unix peer credentials unsupported") }
