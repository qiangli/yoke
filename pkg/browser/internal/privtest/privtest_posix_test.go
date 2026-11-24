//go:build !windows

package privtest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPOSIXRequiresExact0600(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		mode os.FileMode
		ok   bool
	}{{0o600, true}, {0o400, false}, {0o640, false}, {0o700, false}, {0o666, false}} {
		if err := os.Chmod(p, c.mode); err != nil {
			t.Fatal(err)
		}
		if got := check(p) == nil; got != c.ok {
			t.Errorf("mode %v: private=%v, want %v", c.mode, got, c.ok)
		}
	}
}
