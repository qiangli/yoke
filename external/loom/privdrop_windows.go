//go:build windows

package loom

import (
	"io/fs"
	"os/exec"
)

// Windows never drops: there is no euid, loom never refused to run there, and
// shouldDropPrivileges is false for goos == "windows", so *privDrop is always
// nil on this platform. These are the shapes the shared code needs.

func (d *privDrop) apply(*exec.Cmd) {}

func fileOwner(fs.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
