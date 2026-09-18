//go:build !darwin && !linux && !windows

package engine

import "fmt"

// freeMemoryMB on unsupported platforms (windows, freebsd, etc. — for
// now). Returns an error so the preflight does a log-and-skip rather
// than gating; we'd rather let the user attempt the provision than
// hard-refuse on a platform we haven't certified.
func freeMemoryMB() (free uint64, total uint64, err error) {
	return 0, 0, fmt.Errorf("preflight not implemented for this platform")
}
