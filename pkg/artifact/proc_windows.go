// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build windows

package artifact

import (
	"errors"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

const stillActiveExitCode = 259 // STATUS_PENDING / STILL_ACTIVE

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActiveExitCode
}

func isSharingOrLockError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrSharingViolation) ||
		errors.Is(err, os.ErrPermission) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, syscall.Errno(32)) ||
		errors.Is(err, syscall.Errno(33)) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "sharing violation") || strings.Contains(msg, "used by another process")
}
