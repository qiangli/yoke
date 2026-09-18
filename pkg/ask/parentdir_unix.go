//go:build !windows

package ask

import (
	"fmt"
	"io/fs"
)

func checkParentDirAccess(path string, fi fs.FileInfo) error {
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("ask: refusing to write a secret into %s — it is mode %#o and other users can write there", path, perm)
	}
	return nil
}
