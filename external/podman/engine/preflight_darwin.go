//go:build darwin

package engine

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// freeMemoryMB returns (free, total) memory in MB on darwin.
//
// "Free" here matches the Activity Monitor "Memory Used" complement —
// pages that can be allocated without paging out user data. macOS
// aggressively uses memory for file cache (the inactive + speculative
// pools), so just summing "Pages free" undercounts by ~50% on a warm
// system. The pools we include:
//   - free: never touched
//   - inactive: dirty pages that have been pushed out of working set;
//     reclaimable without paging
//   - speculative: prefetched into page cache; reclaimable
//   - purgeable: pages apps explicitly marked as discardable
//
// Total memory comes from `sysctl hw.memsize`. Both calls shell out
// rather than going through C bindings — no cgo, no platform headers,
// portable enough for the preflight gate.
func freeMemoryMB() (free uint64, total uint64, err error) {
	totalOut, err := exec.Command(darwinTool("sysctl", "/usr/sbin/sysctl"), "-n", "hw.memsize").Output()
	if err != nil {
		return 0, 0, fmt.Errorf("sysctl hw.memsize: %w", err)
	}
	totalBytes, err := strconv.ParseUint(strings.TrimSpace(string(totalOut)), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse hw.memsize %q: %w", string(totalOut), err)
	}

	vmOut, err := exec.Command(darwinTool("vm_stat", "/usr/bin/vm_stat")).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("vm_stat: %w", err)
	}
	freeMB, err := parseVMStatFreeMB(string(vmOut))
	if err != nil {
		return 0, 0, fmt.Errorf("parse vm_stat: %w", err)
	}
	return freeMB, totalBytes / (1024 * 1024), nil
}

func darwinTool(name, fallback string) string {
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	return fallback
}

// parseVMStatFreeMB lives in preflight.go (platform-agnostic) so the
// parser is unit-testable on every host. The darwin-only piece above
// is just the I/O glue (`sysctl`, `vm_stat`).
