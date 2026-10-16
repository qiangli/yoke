//go:build !linux

package mountcmd

import (
	"fmt"
	"runtime"
)

type hostSystem struct{}

func unsupported() error {
	return fmt.Errorf("mount/umount are supported only on Linux (this is %s)", runtime.GOOS)
}

func (hostSystem) Mount(string, string, string, uintptr, string) error { return unsupported() }
func (hostSystem) Unmount(string, int) error                           { return unsupported() }
func (hostSystem) Mounts() ([]Entry, error)                            { return nil, unsupported() }
