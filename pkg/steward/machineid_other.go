// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

//go:build !linux && !darwin && !freebsd && !windows

package steward

import "fmt"

// machineID probes the file-based machine identities that some of the remaining
// platforms carry, and FAILS otherwise.
//
// Failing is the correct answer, not a gap. A machine that cannot say which machine it
// is cannot safely hold a one-per-machine seat, and the honest response is to say so
// and point at $BASHY_HOST_ID — never to synthesize an id, because every synthesizable
// source (the hostname, a file under $HOME) is one that two machines can share, which
// is the failure this identity exists to detect.
func machineID() (string, string, error) {
	id, src, err := machineIDFromFiles("/etc/machine-id", "/var/lib/dbus/machine-id", "/etc/hostid")
	if err != nil {
		return "", "", fmt.Errorf("this platform exposes no stable machine id to a pure-Go process")
	}
	return id, src, nil
}
