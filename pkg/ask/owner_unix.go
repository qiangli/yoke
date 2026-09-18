//go:build !windows

package ask

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// checkOwner refuses a directory owned by anyone but us.
//
// Mode bits say who MAY open a path; ownership says who DID create it. A
// directory that is mode 0700 but owned by another user is not protection, it is
// the opposite — we would be writing secrets into a place only they can read.
func checkOwner(path string, fi fs.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("ask: refusing to use %s — it is owned by uid %d, not %d",
			path, st.Uid, os.Getuid())
	}
	return nil
}
