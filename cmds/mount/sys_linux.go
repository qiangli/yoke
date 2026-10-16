//go:build linux

package mountcmd

import (
	"bufio"
	"os"

	"golang.org/x/sys/unix"
)

// The planner's flag constants must be the kernel's: each line fails to
// compile (constant overflow) if a value drifts from x/sys/unix.
const (
	_ = uint64(msRdonly-unix.MS_RDONLY) + uint64(unix.MS_RDONLY-msRdonly)
	_ = uint64(msNosuid-unix.MS_NOSUID) + uint64(unix.MS_NOSUID-msNosuid)
	_ = uint64(msNodev-unix.MS_NODEV) + uint64(unix.MS_NODEV-msNodev)
	_ = uint64(msNoexec-unix.MS_NOEXEC) + uint64(unix.MS_NOEXEC-msNoexec)
	_ = uint64(msSynchronous-unix.MS_SYNCHRONOUS) + uint64(unix.MS_SYNCHRONOUS-msSynchronous)
	_ = uint64(msRemount-unix.MS_REMOUNT) + uint64(unix.MS_REMOUNT-msRemount)
	_ = uint64(msDirsync-unix.MS_DIRSYNC) + uint64(unix.MS_DIRSYNC-msDirsync)
	_ = uint64(msNoatime-unix.MS_NOATIME) + uint64(unix.MS_NOATIME-msNoatime)
	_ = uint64(msNodiratime-unix.MS_NODIRATIME) + uint64(unix.MS_NODIRATIME-msNodiratime)
	_ = uint64(msBind-unix.MS_BIND) + uint64(unix.MS_BIND-msBind)
	_ = uint64(msRec-unix.MS_REC) + uint64(unix.MS_REC-msRec)
	_ = uint64(msUnbindable-unix.MS_UNBINDABLE) + uint64(unix.MS_UNBINDABLE-msUnbindable)
	_ = uint64(msPrivate-unix.MS_PRIVATE) + uint64(unix.MS_PRIVATE-msPrivate)
	_ = uint64(msSlave-unix.MS_SLAVE) + uint64(unix.MS_SLAVE-msSlave)
	_ = uint64(msShared-unix.MS_SHARED) + uint64(unix.MS_SHARED-msShared)
	_ = uint64(msRelatime-unix.MS_RELATIME) + uint64(unix.MS_RELATIME-msRelatime)
	_ = uint64(msStrictatime-unix.MS_STRICTATIME) + uint64(unix.MS_STRICTATIME-msStrictatime)
	_ = uint64(msLazytime-unix.MS_LAZYTIME) + uint64(unix.MS_LAZYTIME-msLazytime)
	_ = uint64(mntForce-unix.MNT_FORCE) + uint64(unix.MNT_FORCE-mntForce)
	_ = uint64(mntDetach-unix.MNT_DETACH) + uint64(unix.MNT_DETACH-mntDetach)
)

type hostSystem struct{}

func (hostSystem) Mount(source, target, fstype string, flags uintptr, data string) error {
	return unix.Mount(source, target, fstype, flags, data)
}

func (hostSystem) Unmount(target string, flags int) error { return unix.Unmount(target, flags) }

func (hostSystem) Mounts() ([]Entry, error) {
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseMounts(bufio.NewScanner(f))
}
