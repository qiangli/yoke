package securejoin

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestWithinBaseRootSlash is the S488.3d follow-up red case: the merged
// candidate's withinBase checked prefix "//", so every path under the
// filesystem root "/" reported as outside the root.
func TestWithinBaseRootSlash(t *testing.T) {
	if !withinBase("/", "/") {
		t.Errorf(`withinBase("/", "/") = false, want true`)
	}
	if !withinBase("/", "/etc") {
		t.Errorf(`withinBase("/", "/etc") = false, want true`)
	}
	if !withinBase("/", "/etc/passwd") {
		t.Errorf(`withinBase("/", "/etc/passwd") = false, want true`)
	}
	// Non-root behavior must not change: "/ab" is not under "/a".
	if withinBase("/a", "/ab") {
		t.Errorf(`withinBase("/a", "/ab") = true, want false`)
	}
	if !withinBase("/a", "/a/b") {
		t.Errorf(`withinBase("/a", "/a/b") = false, want true`)
	}
}

// TestSecureJoinFilesystemRoot pins SecureJoin with root "/": absolute
// inputs resolve under "/", and ".." above the root clamps instead of
// escaping or erroring with an outside path.
func TestSecureJoinFilesystemRoot(t *testing.T) {
	got, err := SecureJoin("/", "/etc")
	if err != nil {
		t.Fatalf("SecureJoin(/, /etc): unexpected error: %v", err)
	}
	// No exact-path assertion: hosts may symlink /etc elsewhere
	// (darwin: /etc -> private/etc) and the join expands links met
	// on the walk. The contract is containment under /.
	if !withinBase("/", got) {
		t.Errorf("SecureJoin(/, /etc) = %q escapes /", got)
	}
	for _, unsafe := range []string{"..", "../escape", "/../escape", "a/../../b"} {
		got, err := SecureJoin("/", unsafe)
		if err != nil {
			if got != "" && !withinBase("/", got) {
				t.Errorf("SecureJoin(/, %q) error still yielded outside path %q", unsafe, got)
			}
			continue
		}
		if !withinBase("/", got) {
			t.Errorf("SecureJoin(/, %q) = %q escapes /", unsafe, got)
		}
		if !strings.HasPrefix(got, string(filepath.Separator)) {
			t.Errorf("SecureJoin(/, %q) = %q is not absolute", unsafe, got)
		}
	}
}
