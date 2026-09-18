//go:build windows

package ask

import "io/fs"

// checkOwner is a no-op on Windows.
//
// There is no uid to compare, and the NTFS ACL equivalent needs the security APIs
// rather than a stat. The per-user profile directory the ask root lives under is
// already ACL'd to the user, which is the protection this check is standing in for
// on unix. Recorded as a known asymmetry rather than a silently missing check.
func checkOwner(string, fs.FileInfo) error { return nil }
