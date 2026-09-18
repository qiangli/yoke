// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

//go:build linux

package steward

// machineID reads the systemd/D-Bus machine id: a stable, per-installation 128-bit id
// that survives reboots and renames, and differs between two machines that happen to
// share a hostname.
func machineID() (string, string, error) {
	return machineIDFromFiles("/etc/machine-id", "/var/lib/dbus/machine-id")
}
