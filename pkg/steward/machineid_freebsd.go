// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

//go:build freebsd

package steward

import (
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

// machineID reads the host UUID via sysctl, falling back to the hostid file.
func machineID() (string, string, error) {
	if v, err := unix.Sysctl("kern.hostuuid"); err == nil {
		if v = strings.TrimSpace(v); v != "" && v != "00000000-0000-0000-0000-000000000000" {
			return "freebsd-uuid:" + v, "sysctl:kern.hostuuid", nil
		}
	}
	id, src, err := machineIDFromFiles("/etc/hostid", "/etc/machine-id")
	if err != nil {
		return "", "", fmt.Errorf("kern.hostuuid is unset and no hostid file is readable")
	}
	return id, src, nil
}
