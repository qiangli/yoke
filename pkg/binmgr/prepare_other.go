//go:build !windows

package binmgr

// PrepareArgs is Command's argument adaptation, for a caller that builds its
// own exec.Cmd. Linux and macOS need none: a managed tool reads the shell's
// paths as they are.
func PrepareArgs(args []string) []string { return args }
