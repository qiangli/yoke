// Package lockfileconsumers holds the yoke half of one invariant shared with
// github.com/qiangli/coreutils/pkg/lockfile: there is exactly ONE platform
// pair of file-lock implementations (lockfile's lock_unix.go/lock_windows.go),
// and every consumer here uses it rather than growing its own. coreutils'
// TestExactlyOneScopedPlatformPair asserts the pair; this test asserts the
// consumers stayed empty after the Sprint 208 split moved them here.
package lockfileconsumers

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLockfileConsumersHaveNoPlatformPair(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
	scoped := []string{
		"pkg/weave",
		"pkg/meet",
		"pkg/steward",
		"pkg/policy/coord",
		"pkg/policy/audit",
	}
	var got []string
	for _, dir := range scoped {
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || (!strings.Contains(name, "lock") && !strings.Contains(name, "flock")) {
				continue
			}
			if strings.HasSuffix(name, "_unix.go") || strings.HasSuffix(name, "_windows.go") {
				got = append(got, filepath.ToSlash(filepath.Join(dir, name)))
			}
		}
	}
	if len(got) != 0 {
		t.Fatalf("file-lock platform implementations outside coreutils/pkg/lockfile: %v (use lockfile's single shared pair)", got)
	}
}
