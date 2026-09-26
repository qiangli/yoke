package binmgr

import (
	"context"
	"os/exec"

	"mvdan.cc/sh/v3/pathconv"
)

// Command is the one way to run a binmgr-managed tool: exec.CommandContext
// with the pre-run adaptation every managed tool gets. Front doors call it
// instead of exec.Command for a binary binmgr provisioned, so a change here
// reaches all of them.
//
// Today the adaptation is the argument spelling on Windows: the shell's POSIX
// drive paths ("/c/Users/x", "--dir=/c/x") become native ("C:\Users\x"),
// which a native tool cannot otherwise read. Only managed tools get it; the
// shell's own spelling is unchanged everywhere else (Bash# relies on it).
func Command(ctx context.Context, bin string, args ...string) *exec.Cmd {
	if ctx == nil {
		ctx = context.Background()
	}
	return exec.CommandContext(ctx, bin, PrepareArgs(args)...)
}

// PrepareArgs is Command's argument adaptation, for a caller that builds its
// own exec.Cmd (argv[0] set apart from the path, say).
func PrepareArgs(args []string) []string {
	return pathconv.NativeArgs(args)
}
