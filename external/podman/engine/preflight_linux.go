//go:build linux

package engine

import (
	"fmt"
	"os"
)

// freeMemoryMB returns (free, total) memory in MB on Linux by reading
// /proc/meminfo. Uses MemAvailable (added kernel 3.14, every modern
// distro has it) — it's the kernel's own estimate of how much memory
// can be allocated without swapping, accounting for caches that can
// be reclaimed. Equivalent semantics to the darwin
// free+inactive+speculative+purgeable sum.
//
// MemTotal is the physical memory size, same as `sysctl hw.memsize`
// on darwin.
func freeMemoryMB() (free uint64, total uint64, err error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	free, total, err = parseMeminfo(string(data))
	if err != nil {
		return 0, 0, fmt.Errorf("parse /proc/meminfo: %w", err)
	}
	return free, total, nil
}

// parseMeminfo lives in preflight.go (platform-agnostic) so the parser
// is unit-testable on every host. The linux-only piece above is just
// the I/O glue (`os.ReadFile("/proc/meminfo")`).
