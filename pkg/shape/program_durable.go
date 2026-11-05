package shape

import (
	"os"
	"runtime"
)

// Directory sync makes journal publication/removal durable on supporting hosts.
// Windows does not expose directory fsync through os.File.Sync.
func syncProgramDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, e := os.Open(path)
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}
