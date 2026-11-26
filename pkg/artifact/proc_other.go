// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build !linux && !darwin && !windows

package artifact

import "errors"

func lookupProcessStartID(int) (string, error) {
	return "", errors.New("native process birth identity unavailable on this OS")
}
