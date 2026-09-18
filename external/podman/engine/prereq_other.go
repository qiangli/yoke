//go:build !windows

package engine

import "context"

func ensurePlatformMachinePrereqs(context.Context) error {
	return nil
}
