//go:build windows

package weave

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

type weaveFreeSpace struct {
	Bytes uint64
}

const weaveWorkspaceFreeSpaceThreshold = 2 << 30

var weaveWorkspaceFreeSpaceFn = weaveWorkspaceFreeSpace

func weaveCheckWorkspaceFreeSpace(workspace string) error {
	free, err := weaveWorkspaceFreeSpaceFn(workspace)
	if err != nil {
		return fmt.Errorf("check workspace free space for %s: %w", workspace, err)
	}
	if free.Bytes >= weaveWorkspaceFreeSpaceThreshold {
		return nil
	}
	return fmt.Errorf("workspace volume has %s free space, below threshold %s; leaving run queued until space is available",
		weaveFormatBytes(free.Bytes), weaveFormatBytes(weaveWorkspaceFreeSpaceThreshold))
}

func weaveWorkspaceFreeSpace(path string) (weaveFreeSpace, error) {
	probe := path
	for {
		st, err := os.Stat(probe)
		if err == nil {
			if !st.IsDir() {
				probe = filepath.Dir(probe)
			}
			break
		}
		if !os.IsNotExist(err) {
			return weaveFreeSpace{}, err
		}
		next := filepath.Dir(probe)
		if next == probe {
			return weaveFreeSpace{}, err
		}
		probe = next
	}
	if !strings.HasSuffix(probe, string(os.PathSeparator)) {
		probe += string(os.PathSeparator)
	}
	p, err := windows.UTF16PtrFromString(probe)
	if err != nil {
		return weaveFreeSpace{}, err
	}
	var freeBytesAvailableToCaller uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeBytesAvailableToCaller, nil, nil); err != nil {
		return weaveFreeSpace{}, err
	}
	return weaveFreeSpace{Bytes: freeBytesAvailableToCaller}, nil
}
