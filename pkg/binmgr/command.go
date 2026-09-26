package binmgr

import (
	"context"
	"os/exec"
)

// Command is the one way to run a binmgr-managed tool: exec.CommandContext
// with the pre-run adaptation every managed tool gets. Front doors call it
// instead of exec.Command for a binary binmgr provisioned, so a change here
// reaches all of them.
//
// The adaptation is per OS (prepare_windows.go, prepare_other.go): on Windows
// the shell's POSIX drive paths ("/c/Users/x", "--dir=/c/x") become native
// ("C:\Users\x"), which a native tool cannot otherwise read; Linux and macOS
// need none. Only managed tools get it; the shell's own spelling is unchanged
// everywhere else (Bash# relies on it).
func Command(ctx context.Context, bin string, args ...string) *exec.Cmd {
	if ctx == nil {
		ctx = context.Background()
	}
	return exec.CommandContext(ctx, bin, PrepareArgs(args)...)
}
