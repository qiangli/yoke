package binmgr

import "mvdan.cc/sh/v3/pathconv"

// PrepareArgs is Command's argument adaptation, for a caller that builds its
// own exec.Cmd: the shell's POSIX drive paths become native (Windows only).
func PrepareArgs(args []string) []string {
	return pathconv.NativeArgs(args)
}
