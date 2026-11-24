// Package privtest is a TEST-ONLY helper: it asserts that a file the browser
// packages wrote as "private" really is private on this OS. On POSIX that is
// the exact 0600 mode. Go's Windows Stat always reports 0666/0444 and says
// nothing about the ACL, so on Windows the assertion reads the real DACL
// (see privtest_windows.go). Import it only from _test.go files.
package privtest

import "testing"

// Private fails t unless path is private to its owner (POSIX: mode 0600;
// Windows: owner and every allow entry limited to the running user, SYSTEM and
// Administrators, with an allow entry for the running user; see acl.go).
func Private(t testing.TB, path string) {
	t.Helper()
	if err := check(path); err != nil {
		t.Errorf("%s is not private: %v", path, err)
	}
}
