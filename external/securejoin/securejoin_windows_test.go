//go:build windows

package securejoin

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestJoinStackStaysAbsolute pins the drive-relative fix: seeding the
// join with a bare volume ("C:") makes stdlib filepath.Join produce
// drive-relative paths (Join(`C:`, `f`) = `C:f`), so joinStack seeds
// with the separator attached ("C:\") and every result stays absolute
// on the root's volume.
func TestJoinStackStaysAbsolute(t *testing.T) {
	vol := filepath.VolumeName(t.TempDir())
	if vol == "" {
		t.Fatal("expected a volume name in the temp dir path")
	}
	for _, stack := range [][]string{
		{vol},
		{vol, "Users"},
		{vol, "Users", "x", "..", "y"},
	} {
		got := joinStack(vol, stack)
		if !filepath.IsAbs(got) {
			t.Errorf("joinStack(%q, %q) = %q, want an absolute path", vol, stack, got)
		}
		if filepath.VolumeName(got) != vol {
			t.Errorf("joinStack(%q, %q) = %q, want volume %q", vol, stack, got, vol)
		}
		if strings.HasPrefix(got, vol) && !strings.HasPrefix(got, vol+string(filepath.Separator)) {
			t.Errorf("joinStack(%q, %q) = %q, want volume-rooted (drive-relative)", vol, stack, got)
		}
	}
}

// TestSecureJoinWindowsContainment pins containment on Windows: joins
// stay absolute on the root's volume and inside the root, lexical
// escapes clamp, and a volume-root base contains the whole drive.
func TestSecureJoinWindowsContainment(t *testing.T) {
	root := t.TempDir()
	absRoot := filepath.Clean(root)
	vol := filepath.VolumeName(absRoot)
	check := func(root, unsafe string) (string, bool) {
		got, err := SecureJoin(root, unsafe)
		if err != nil {
			return got, false
		}
		if !filepath.IsAbs(got) {
			t.Errorf("SecureJoin(%q, %q) = %q, want an absolute path", root, unsafe, got)
		}
		if filepath.VolumeName(got) != filepath.VolumeName(filepath.Clean(root)) {
			t.Errorf("SecureJoin(%q, %q) = %q, left volume %q", root, unsafe, got, vol)
		}
		if !withinBase(filepath.Clean(root), got) {
			t.Errorf("SecureJoin(%q, %q) = %q escapes the root", root, unsafe, got)
		}
		return got, true
	}
	for _, unsafe := range []string{`a\b`, `sub\..\inside.txt`, `C:evil`, `..\escape`, `..\..\escape`} {
		check(absRoot, unsafe)
	}
	// A drive-root base contains the drive: nothing escapes it.
	driveRoot := vol + string(filepath.Separator)
	for _, unsafe := range []string{`a`, `..\escape`, `a\..\..\b`} {
		got, err := SecureJoin(driveRoot, unsafe)
		if err != nil {
			if got != "" && !withinBase(driveRoot, got) {
				t.Errorf("SecureJoin(%q, %q) error still yielded outside path %q", driveRoot, unsafe, got)
			}
			continue
		}
		if !withinBase(driveRoot, got) {
			t.Errorf("SecureJoin(%q, %q) = %q escapes the drive root", driveRoot, unsafe, got)
		}
	}
}
