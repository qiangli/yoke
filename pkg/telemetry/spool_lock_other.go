//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !windows

package telemetry

import (
	"fmt"
	"os"
)

func trySpoolLock(*os.File) (bool, func() error, error) {
	return false, nil, fmt.Errorf("telemetry: cross-process spool locking is unavailable on this platform")
}
