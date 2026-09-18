// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

//go:build windows

package steward

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// machineID reads MachineGuid: the per-installation id Windows itself uses to tell one
// machine from another. Read straight from the registry — no PowerShell, no wmic.
func machineID() (string, string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return "", "", fmt.Errorf(`registry HKLM\SOFTWARE\Microsoft\Cryptography: %w`, err)
	}
	defer k.Close()

	v, _, err := k.GetStringValue("MachineGuid")
	if err != nil {
		return "", "", fmt.Errorf("registry MachineGuid: %w", err)
	}
	if v = strings.TrimSpace(v); v == "" {
		return "", "", fmt.Errorf("registry MachineGuid is empty")
	}
	return "windows-guid:" + v, "registry:MachineGuid", nil
}
