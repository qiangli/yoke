// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build !windows

package artifact

import (
	"errors"
	"os"
	"strings"
	"syscall"
)

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func isSharingOrLockError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrSharingViolation) || errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.ETXTBSY) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "sharing violation") || strings.Contains(msg, "used by another process")
}
