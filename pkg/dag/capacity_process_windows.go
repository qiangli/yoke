//go:build windows

package dag

import "os/exec"

func prepareCapacityProcess(cmd *exec.Cmd)   {}
func capacityProcessGone(cmd *exec.Cmd) bool { return cmd.Process == nil }

func capacityGroupGone(pid int) bool { return false }
