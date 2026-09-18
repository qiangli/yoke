// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

//go:build darwin

package steward

import (
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

// machineID reads the kernel's per-machine UUID via sysctl.
//
// Read by SYSCALL, not by shelling out to ioreg/system_profiler: this repo never spawns
// a program to implement its own behavior, and a machine identity that depended on a
// binary being present would fail on exactly the stripped-down hosts an agent userland
// exists for.
func machineID() (string, string, error) {
	v, err := unix.Sysctl("kern.uuid")
	if err != nil {
		return "", "", fmt.Errorf("sysctl kern.uuid: %w", err)
	}
	if v = strings.TrimSpace(v); v == "" {
		return "", "", fmt.Errorf("sysctl kern.uuid is empty")
	}
	return "darwin-uuid:" + v, "sysctl:kern.uuid", nil
}
