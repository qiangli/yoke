//go:build windows || plan9

package gate

import "os/exec"

// prepareGateCommand retains exec.CommandContext's platform cancellation.
// POSIX process groups are unavailable here.
func prepareGateCommand(_ *exec.Cmd) {}
