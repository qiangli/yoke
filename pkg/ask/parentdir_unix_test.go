//go:build !windows

package ask

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckParentDirRejectsGroupWritableDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "group-writable")
	if err := os.Mkdir(dir, 0o720); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o720); err != nil {
		t.Fatal(err)
	}
	if err := checkParentDir(dir); err == nil {
		t.Fatal("accepted a group-writable secret directory")
	}
}
