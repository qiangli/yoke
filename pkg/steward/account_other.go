// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

//go:build !windows

package steward

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// accountID is the OS account this process actually runs as: the numeric UID.
//
// NOT $USER, $LOGNAME, or $USERNAME. Those are strings a process inherits and can
// overwrite, and a seat keyed on them was a seat an agent could change by exporting a
// variable — `USER=someone-else bashy steward claim` was a different seat, so the
// singleton could be sidestepped without touching a single file. The UID is what the
// kernel says, and no amount of exporting changes it.
//
// The uid, not the username, for the same reason at one remove: a username is a lookup
// through a name service that can be renamed, aliased, or unavailable; the uid is the
// identity the OS enforces its own permissions with.
func accountID() (string, error) {
	uid := os.Getuid()
	if uid < 0 {
		// Getuid returns -1 where the concept does not exist (js/wasm, plan9).
		return "", fmt.Errorf("this platform has no OS account identity")
	}
	return "uid:" + strconv.Itoa(uid), nil
}

// accountHome is the home directory the OS ITSELF records for the account this process
// runs as: the passwd entry for the real UID.
//
// NOT $HOME, and NOT os.UserHomeDir — which is $HOME with a different spelling. That
// distinction is the entire point, because the canonical seat registry is rooted here (see
// registry.go). A registry root read out of the environment is a registry root the process
// it governs can move:
//
//	HOME=/tmp/other BASHY_STEWARD_DIR=/tmp/other/store bashy steward claim
//
// found no binding (the registry it looked in was brand new), bound the seat to a store of
// the agent's choosing, minted its own epoch ladder from an empty journal, and handed out a
// SECOND seat for the same machine-and-account — the exact failure the registry exists to
// prevent, reached by exporting a variable. The uid is what the kernel says, and the passwd
// record is what the OS says about that uid; neither is rewritable by the process asking.
//
// LookupId, not user.Current: Current is documented to consult the environment on the
// platforms where there is no account database, and this is the one place that must not.
func accountHome() (dir, source string, err error) {
	uid := os.Getuid()
	if uid < 0 {
		return "", "", fmt.Errorf("this platform has no OS account identity")
	}
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return "", "", fmt.Errorf("no account record for uid %d: %w", uid, err)
	}
	home := strings.TrimSpace(u.HomeDir)
	if !filepath.IsAbs(home) {
		return "", "", fmt.Errorf("the account record for uid %d carries no absolute home directory (%q)", uid, u.HomeDir)
	}
	return filepath.Clean(home), "account:uid:" + strconv.Itoa(uid), nil
}
