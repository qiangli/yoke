// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

//go:build windows

package steward

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// accountID is the OS account this process actually runs as: its SID.
//
// NOT %USERNAME%, which is an environment string the process can overwrite — the
// windows half of the same hole the unix side closes with the uid. The SID is what the
// kernel checks its own access decisions against, it is taken from THIS PROCESS's token
// rather than from any ambient string, and it is stable across a rename of the account.
func accountID() (string, error) {
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("cannot read this process's token user: %w", err)
	}
	sid := u.User.Sid.String()
	if strings.TrimSpace(sid) == "" {
		return "", fmt.Errorf("this process's token carries no user SID")
	}
	return "sid:" + sid, nil
}

// accountHome is the profile directory WINDOWS records for the account this process runs
// as, resolved from its access token.
//
// NOT %USERPROFILE%, and NOT os.UserHomeDir — which is %USERPROFILE% with a different
// spelling. The canonical seat registry is rooted here (see registry.go), and a registry
// root read out of the environment is one the process it governs can move: set
// %USERPROFILE% and $BASHY_STEWARD_DIR together and you look in a brand-new registry, find
// no binding, and mint a SECOND seat for a machine-and-account that already has one. Both
// sources below take the PROCESS TOKEN and ask the OS where that account's profile lives —
// GetUserProfileDirectory first, the FOLDERID_Profile known folder second (the same answer
// by the shell's route, for a token whose profile the userenv API declines to report).
// Neither reads an environment string.
func accountHome() (dir, source string, err error) {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &tok); err != nil {
		return "", "", fmt.Errorf("cannot open this process's token: %w", err)
	}
	defer tok.Close()

	home, perr := tok.GetUserProfileDirectory()
	src := "account:token:profile"
	if perr != nil || strings.TrimSpace(home) == "" {
		var kerr error
		home, kerr = tok.KnownFolderPath(windows.FOLDERID_Profile, windows.KF_FLAG_DEFAULT)
		src = "account:knownfolder:profile"
		if kerr != nil {
			return "", "", fmt.Errorf("cannot resolve this account's profile directory: %v (known folder: %w)", perr, kerr)
		}
	}
	home = strings.TrimSpace(home)
	if !filepath.IsAbs(home) {
		return "", "", fmt.Errorf("this account's profile directory is not absolute (%q)", home)
	}
	return filepath.Clean(home), src, nil
}
