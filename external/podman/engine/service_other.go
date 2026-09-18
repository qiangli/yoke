//go:build !linux && !freebsd

package engine

import (
	"context"
	"fmt"
)

// startServiceInProcess is not available on this platform.
// macOS/Windows require a Linux VM (podman machine) for container execution.
// The VM must be started separately; ycode connects to it via REST socket.
func (e *Engine) startServiceInProcess(_ context.Context, _ *EngineConfig) error {
	return fmt.Errorf("in-process Podman not available on this platform (containers require a Linux kernel); start podman machine or connect to a remote host")
}

// canStartInProcess returns false on non-Linux platforms.
func canStartInProcess() bool {
	return false
}
