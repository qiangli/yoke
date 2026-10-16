//go:build !linux

package nft

import (
	"fmt"
	"runtime"
)

// NewNetlink returns the kernel backend; nftables exists only on Linux.
func NewNetlink() (Backend, error) {
	return nil, fmt.Errorf("nftables is not available on %s", runtime.GOOS)
}
